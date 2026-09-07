package mst

import (
	"flag"
	"log"
	"math/rand"

	"gitlab.com/akita/mgpusim/benchmarks/matrix/csr"
	"gitlab.com/akita/mgpusim/footprint"
)

// replay reproduces the address stream of mst_reset/mst_find_min/mst_union/
// mst_compress (native/kernel/mst_kernel.cl) on the host.
//
// The launch count is data-dependent: exec() loops until the device 'changed'
// flag stays 0 (benchmark.go:229-235), so the stream cannot be derived from the
// CSR arrays alone. This replay executes the kernels instead: every thread of
// every launch is walked in id order over host copies of comp/min_wt/min_edge,
// recording every global access as it happens.
//
// EXACTNESS CAVEAT — read before trusting any per-round number from this file.
//
// The kernels update shared state with deliberately non-atomic writes:
// mst_find_min performs a read-compare-write reduction onto min_wt[src_comp]
// and min_edge[src_comp] (mst_kernel.cl:62-67, "non-atomic conditional write —
// races accepted") and mst_union writes comp[hi] the same way
// (mst_kernel.cl:94). On the GPU those writes race, so the winning edge of a
// Boruvka round — and therefore which components merge — is decided by the
// hardware's scheduling. A sequential host walk necessarily picks one
// particular interleaving: the fully serialized one, in which the reduction
// always yields the true minimum and every thread sees all writes from every
// lower-numbered thread. That is the best case, and it converges at least as
// fast as any real GPU run, so the round count and hence the total length of
// the address stream may differ from any given timing run. No offline method
// can fix this: the true round count is a property of the hardware's
// scheduling, not of the program.
//
// What survives the caveat is the shared-footprint result, which is what this
// tool measures. Each round's traffic is dominated by mst_find_min's structural
// gather — col[e] plus the comp[col[e]] indirection and weight[e], once per
// edge — and that gather visits the same lines, from the same chiplets, in
// every round regardless of interleaving. The set of shared lines and their
// sharing degree are therefore stable; only per-line access counts scale with
// the round count.
//
// The loop also stops at a fixed point, not only when the kernels clear the
// flag: mst_find_min raises 'changed' for the first cross-component edge it
// meets whether or not a merge follows, so a round that moves no comp[] entry
// would repeat itself forever. See the round loop in Run.
type replay struct {
	numNodes *int
	numEdges *int
}

const (
	// blockSizeReplay mirrors the block size in exec() (benchmark.go:189).
	blockSizeReplay = 64

	// weightSeedReplay fixes the edge weights. exec() draws them from the
	// unseeded global math/rand source (benchmark.go:178), which Go 1.20+
	// seeds per process, so a timing run's weights are not reproducible at
	// all. A fixed seed at least makes this replay reproducible; weights
	// only steer which edge wins a round, never which lines are touched.
	weightSeedReplay = 1

	// maxRoundsReplay is a backstop only: the fixed-point test in Run ends a
	// stalled run first, and Boruvka needs O(log numNodes) rounds.
	maxRoundsReplay = 200
)

func init() {
	footprint.Register("mst", func() footprint.Replay { return &replay{} })
}

func (r *replay) Name() string { return "mst" }

func (r *replay) Flags(fs *flag.FlagSet) {
	r.numNodes = fs.Int("num-nodes", 4096, "Number of graph nodes.")
	r.numEdges = fs.Int("num-edges", 65536, "Number of graph edges.")
}

func (r *replay) Run(rec *footprint.Recorder, cfg footprint.Config) {
	numNodes := int32(*r.numNodes)
	numEdges := int32(*r.numEdges)

	matrix := csr.
		MakeMatrixGenerator(uint32(numNodes), uint32(numEdges)).
		GenerateMatrix()
	row := matrix.RowOffsets
	col := matrix.ColumnNumbers

	// Allocation order matches initMem (benchmark.go:161-167).
	space := footprint.NewSpace()
	rowBase := space.Alloc(uint64(numNodes) * 4)
	colBase := space.Alloc(uint64(numEdges) * 4)
	weightBase := space.Alloc(uint64(numEdges) * 4)
	compBase := space.Alloc(uint64(numNodes) * 4)
	minWtBase := space.Alloc(uint64(numNodes) * 4)
	minEdgeBase := space.Alloc(uint64(numNodes) * 4)
	changedBase := space.Alloc(4)

	rng := rand.New(rand.NewSource(weightSeedReplay))
	weight := make([]int32, numEdges)
	for i := range weight {
		weight[i] = rng.Int31()
	}

	// comp[i] = i (benchmark.go:183-186).
	comp := make([]int32, numNodes)
	for i := range comp {
		comp[i] = int32(i)
	}
	minWt := make([]int32, numNodes)
	minEdge := make([]int32, numNodes)

	nodeGlobal := numNodes
	if numNodes%blockSizeReplay > 0 {
		nodeGlobal = (numNodes/blockSizeReplay + 1) * blockSizeReplay
	}
	grid := footprint.Grid{
		GlobalSize: [3]uint32{uint32(nodeGlobal), 1, 1},
		LocalSize:  [3]uint16{blockSizeReplay, 1, 1},
	}

	numChiplets := cfg.NumChiplets()
	kernel := 0

	for round := 0; ; round++ {
		chips := wgChiplets(grid, cfg.PartitionFor(kernel), numChiplets)
		kernel++
		replayReset(rec, chips, minWt, minEdge, minWtBase, minEdgeBase,
			changedBase, numNodes)

		chips = wgChiplets(grid, cfg.PartitionFor(kernel), numChiplets)
		kernel++
		flag := replayFindMin(rec, chips, row, col, weight, comp,
			minWt, minEdge, rowBase, colBase, weightBase, compBase,
			minWtBase, minEdgeBase, changedBase, numNodes)

		chips = wgChiplets(grid, cfg.PartitionFor(kernel), numChiplets)
		kernel++
		unionFlag, moved := replayUnion(rec, chips, col, comp, minWt, minEdge,
			colBase, compBase, minWtBase, minEdgeBase, changedBase, numNodes)
		flag = flag || unionFlag

		chips = wgChiplets(grid, cfg.PartitionFor(kernel), numChiplets)
		kernel++
		if replayCompress(rec, chips, comp, compBase, changedBase, numNodes) {
			flag, moved = true, true
		}

		if !flag {
			break
		}

		// min_wt and min_edge are scratch — mst_reset clears them at the top
		// of every round — so comp[] alone carries state between rounds. A
		// round that leaves it untouched would repeat itself forever.
		if !moved {
			log.Printf("mst replay: fixed point after %d rounds with the "+
				"changed flag still set; stopping", round+1)

			break
		}

		if round+1 >= maxRoundsReplay {
			log.Printf("mst replay: hit the %d-round cap, stopping early",
				maxRoundsReplay)

			break
		}
	}
}

// replayReset transcribes mst_reset (mst_kernel.cl:22-34).
func replayReset(
	rec *footprint.Recorder,
	chips []int8,
	minWt, minEdge []int32,
	minWtBase, minEdgeBase, changedBase uint64,
	numNodes int32,
) {
	for u := int32(0); u < numNodes; u++ {
		chip := int(chips[u/blockSizeReplay])

		rec.Touch(chip, minWtBase+uint64(u)*4)
		minWt[u] = intMaxVal

		rec.Touch(chip, minEdgeBase+uint64(u)*4)
		minEdge[u] = -1

		if u == 0 {
			rec.Touch(chip, changedBase)
		}
	}
}

// replayFindMin transcribes mst_find_min (mst_kernel.cl:38-69).
func replayFindMin(
	rec *footprint.Recorder,
	chips []int8,
	row, col []uint32,
	weight, comp, minWt, minEdge []int32,
	rowBase, colBase, weightBase, compBase uint64,
	minWtBase, minEdgeBase, changedBase uint64,
	numNodes int32,
) bool {
	changed := false

	for u := int32(0); u < numNodes; u++ {
		chip := int(chips[u/blockSizeReplay])

		rec.Touch(chip, compBase+uint64(u)*4)
		srcComp := comp[u]

		rec.Touch(chip, rowBase+uint64(u)*4)
		uStart := int32(row[u])

		rec.Touch(chip, rowBase+uint64(u+1)*4)
		uEnd := int32(row[u+1])

		for e := uStart; e < uEnd; e++ {
			rec.Touch(chip, colBase+uint64(e)*4)
			c := col[e]

			rec.Touch(chip, compBase+uint64(c)*4)
			if comp[c] == srcComp {
				continue
			}

			rec.Touch(chip, weightBase+uint64(e)*4)
			w := weight[e]

			rec.Touch(chip, minWtBase+uint64(srcComp)*4)
			if w >= minWt[srcComp] {
				continue
			}

			rec.Touch(chip, minWtBase+uint64(srcComp)*4)
			minWt[srcComp] = w

			rec.Touch(chip, minEdgeBase+uint64(srcComp)*4)
			minEdge[srcComp] = e

			rec.Touch(chip, changedBase)
			changed = true
		}
	}

	return changed
}

// replayUnion transcribes mst_union (mst_kernel.cl:73-96). It reports the
// 'changed' flag and, separately, whether comp[] actually moved: comp[hi] may
// already hold lo, in which case the kernel raises the flag without progress.
func replayUnion(
	rec *footprint.Recorder,
	chips []int8,
	col []uint32,
	comp, minWt, minEdge []int32,
	colBase, compBase, minWtBase, minEdgeBase, changedBase uint64,
	numNodes int32,
) (flag, moved bool) {
	for u := int32(0); u < numNodes; u++ {
		chip := int(chips[u/blockSizeReplay])

		rec.Touch(chip, compBase+uint64(u)*4)
		src := comp[u]
		if src != u {
			continue
		}

		rec.Touch(chip, minWtBase+uint64(src)*4)
		if minWt[src] == intMaxVal {
			continue
		}

		rec.Touch(chip, minEdgeBase+uint64(src)*4)
		e := minEdge[src]
		if e < 0 {
			// Unreachable under sequential execution: min_wt and min_edge
			// are written together. Guards a racy interleaving only.
			continue
		}

		rec.Touch(chip, colBase+uint64(e)*4)
		c := col[e]

		rec.Touch(chip, compBase+uint64(c)*4)
		dst := comp[c]
		if src == dst {
			continue
		}

		hi, lo := src, dst
		if dst > src {
			hi, lo = dst, src
		}

		rec.Touch(chip, compBase+uint64(hi)*4)
		if comp[hi] != lo {
			moved = true
		}
		comp[hi] = lo

		rec.Touch(chip, changedBase)
		flag = true
	}

	return flag, moved
}

// replayCompress transcribes mst_compress (mst_kernel.cl:99-114).
func replayCompress(
	rec *footprint.Recorder,
	chips []int8,
	comp []int32,
	compBase, changedBase uint64,
	numNodes int32,
) bool {
	changed := false

	for v := int32(0); v < numNodes; v++ {
		chip := int(chips[v/blockSizeReplay])

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
