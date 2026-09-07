package cc

import (
	"flag"
	"log"

	"gitlab.com/akita/mgpusim/benchmarks/matrix/csr"
	"gitlab.com/akita/mgpusim/footprint"
)

// replay reproduces the address stream of hook_kernel and compress_kernel
// (native/kernel/kernel_cc.cl) on the host.
//
// Unlike the non-iterative benchmarks, the launch count here is data-dependent:
// exec() loops until the device 'changed' flag stays 0 (benchmark.go:190-198),
// so the address stream cannot be derived from the CSR arrays alone. This
// replay therefore executes the kernels: every thread of every launch is walked
// in id order over host copies of comp[]/changed, and every global access is
// recorded as it happens.
//
// EXACTNESS CAVEAT — read before trusting any per-round number from this file.
//
// Both kernels update comp[] and 'changed' with deliberately non-atomic writes
// (kernel_cc.cl:67 and :85, "non-atomic stubs: mgpusim lacks VI flat_atomic_*
// support"). On the GPU those writes race: which of two threads merging the
// same label wins, and whether a thread observes a neighbour's merge from the
// same launch, is decided by the hardware's scheduling. A sequential host walk
// necessarily picks one particular interleaving — the fully serialized one, in
// which every thread sees all writes from every lower-numbered thread. That
// interleaving converges at least as fast as any real GPU run, so the number of
// rounds, and hence the total length of the address stream, may differ from any
// given timing run. No offline method can fix this: the true round count is a
// property of the hardware's scheduling, not of the program.
//
// What survives the caveat is the shared-footprint result, which is what this
// tool measures. Each round's traffic is dominated by the structural gather —
// the binary search over row[], the col[] read per edge, and the two comp[]
// lookups per edge — and that gather visits the same lines, from the same
// chiplets, in every round regardless of interleaving. The set of shared lines
// and their sharing degree are therefore stable; only per-line access counts
// scale with the round count.
//
// The loop also stops at a fixed point, not only when the kernels clear the
// flag: hook_kernel raises 'changed' for every edge whose endpoints carry
// different labels, whether or not a merge follows, so a round that moves no
// comp[] entry would repeat itself forever. See the round loop in Run.
type replay struct {
	numNodes *int
	numEdges *int
}

const (
	// Mirror the block sizes hardcoded in exec() (benchmark.go:159,167).
	hookBlockSizeReplay     = 64
	compressBlockSizeReplay = 64

	// maxRoundsReplay guards against a mis-transcription that never
	// converges. Shiloach-Vishkin needs O(log numNodes) rounds; measured
	// round counts at the sweep's degree are in the single digits.
	maxRoundsReplay = 200
)

func init() {
	footprint.Register("cc", func() footprint.Replay { return &replay{} })
}

func (r *replay) Name() string { return "cc" }

func (r *replay) Flags(fs *flag.FlagSet) {
	r.numNodes = fs.Int("num-nodes", 1024, "Number of graph nodes.")
	r.numEdges = fs.Int("num-edges", 4096, "Number of graph edges.")
}

func (r *replay) Run(rec *footprint.Recorder, cfg footprint.Config) {
	numNodes := int32(*r.numNodes)
	numEdges := int32(*r.numEdges)

	matrix := csr.
		MakeMatrixGenerator(uint32(numNodes), uint32(numEdges)).
		GenerateMatrix()

	// Allocation order matches initMem (benchmark.go:142-145).
	space := footprint.NewSpace()
	rowBase := space.Alloc(uint64(numNodes) * 4)
	colBase := space.Alloc(uint64(numEdges) * 4)
	compBase := space.Alloc(uint64(numNodes) * 4)
	changedBase := space.Alloc(4)

	// comp[i] = i (benchmark.go:151-154).
	comp := make([]int32, numNodes)
	for i := range comp {
		comp[i] = int32(i)
	}

	edgeGlobal := numEdges
	if numEdges%hookBlockSizeReplay > 0 {
		edgeGlobal = (numEdges/hookBlockSizeReplay + 1) * hookBlockSizeReplay
	}
	hookGrid := footprint.Grid{
		GlobalSize: [3]uint32{uint32(edgeGlobal), 1, 1},
		LocalSize:  [3]uint16{hookBlockSizeReplay, 1, 1},
	}

	nodeGlobal := numNodes
	if numNodes%compressBlockSizeReplay > 0 {
		nodeGlobal = (numNodes/compressBlockSizeReplay + 1) * compressBlockSizeReplay
	}
	compressGrid := footprint.Grid{
		GlobalSize: [3]uint32{uint32(nodeGlobal), 1, 1},
		LocalSize:  [3]uint16{compressBlockSizeReplay, 1, 1},
	}

	numChiplets := cfg.NumChiplets()
	kernel := 0

	for round := 0; ; round++ {
		chips := wgChiplets(hookGrid, cfg.PartitionFor(kernel), numChiplets)
		kernel++
		flag, moved := replayHook(rec, chips,
			matrix.RowOffsets, matrix.ColumnNumbers, comp,
			rowBase, colBase, compBase, changedBase, numNodes, numEdges)

		chips = wgChiplets(compressGrid, cfg.PartitionFor(kernel), numChiplets)
		kernel++
		if replayCompress(rec, chips, comp, compBase, changedBase, numNodes) {
			flag, moved = true, true
		}

		if !flag {
			break
		}

		// The flag is still set but comp[] did not move, so every further
		// round would repeat this one byte for byte. Stopping is the only
		// honest truncation: the benchmark itself would spin here.
		if !moved {
			log.Printf("cc replay: fixed point after %d rounds with the "+
				"changed flag still set; stopping", round+1)

			break
		}

		if round+1 >= maxRoundsReplay {
			log.Printf("cc replay: hit the %d-round cap, stopping early",
				maxRoundsReplay)

			break
		}
	}
}

// replayHook transcribes hook_kernel (kernel_cc.cl:30-69). It reports the
// 'changed' flag the kernel raises and, separately, whether comp[] actually
// moved — the flag is raised for every edge whose endpoints disagree, even when
// no merge follows, so only the second answer proves progress.
func replayHook(
	rec *footprint.Recorder,
	chips []int8,
	row, col []uint32,
	comp []int32,
	rowBase, colBase, compBase, changedBase uint64,
	numNodes, numEdges int32,
) (flag, moved bool) {
	for eid := int32(0); eid < numEdges; eid++ {
		chip := int(chips[eid/hookBlockSizeReplay])

		// Binary search for the largest u with row[u] <= eid. row[mid] is
		// only read when the row[mid+1] test fails (kernel_cc.cl:47-49).
		lo, hi := int32(0), numNodes
		for lo < hi {
			mid := (lo + hi) >> 1

			rec.Touch(chip, rowBase+uint64(mid+1)*4)
			if int32(row[mid+1]) <= eid {
				lo = mid + 1

				continue
			}

			rec.Touch(chip, rowBase+uint64(mid)*4)
			if int32(row[mid]) > eid {
				hi = mid

				continue
			}

			lo = mid

			break
		}
		u := lo

		rec.Touch(chip, colBase+uint64(eid)*4)
		v := int32(col[eid])

		rec.Touch(chip, compBase+uint64(u)*4)
		rec.Touch(chip, compBase+uint64(v)*4)
		cu, cv := comp[u], comp[v]
		if cu == cv {
			continue
		}

		hiLbl, loLbl := cu, cv
		if cv > cu {
			hiLbl, loLbl = cv, cu
		}

		rec.Touch(chip, compBase+uint64(hiLbl)*4)
		if comp[hiLbl] > loLbl {
			rec.Touch(chip, compBase+uint64(hiLbl)*4)
			comp[hiLbl] = loLbl
			moved = true
		}

		// *changed |= 1: load and store.
		rec.Touch(chip, changedBase)
		rec.Touch(chip, changedBase)
		flag = true
	}

	return flag, moved
}

// replayCompress transcribes compress_kernel (kernel_cc.cl:72-88).
func replayCompress(
	rec *footprint.Recorder,
	chips []int8,
	comp []int32,
	compBase, changedBase uint64,
	numNodes int32,
) bool {
	changed := false

	for v := int32(0); v < numNodes; v++ {
		chip := int(chips[v/compressBlockSizeReplay])

		rec.Touch(chip, compBase+uint64(v)*4)
		c := comp[v]

		rec.Touch(chip, compBase+uint64(c)*4)
		c2 := comp[c]

		if c == c2 {
			continue
		}

		rec.Touch(chip, compBase+uint64(v)*4)
		comp[v] = c2

		rec.Touch(chip, changedBase)
		rec.Touch(chip, changedBase)
		changed = true
	}

	return changed
}

// wgChiplets precomputes the chiplet of every work group along X, so the inner
// loops need one indexed load instead of a partition-string match per thread.
func wgChiplets(
	g footprint.Grid,
	partition string,
	numChiplets int,
) []int8 {
	n := g.NumWGX()
	chips := make([]int8, n)
	for i := 0; i < n; i++ {
		chips[i] = int8(footprint.ChipletOfWG(i, 0, g, partition, numChiplets))
	}

	return chips
}
