package mis

import (
	"flag"
	"log"

	"gitlab.com/akita/mgpusim/benchmarks/matrix/csr"
	"gitlab.com/akita/mgpusim/footprint"
)

// replay reproduces the address stream of init/mis1/mis2/mis3
// (native/kernel/kernel.cl) on the host.
//
// The launch count is data-dependent: exec() loops until the device 'stop' flag
// stays 0 (main.go:258-323), and mis1 raises it while any vertex is still
// unprocessed. The stream therefore cannot be derived from the CSR arrays
// alone. This replay executes the kernels instead: every thread of every launch
// is walked in id order over host copies of c_array/cu_array/s_array/min_array,
// recording every global access as it happens.
//
// EXACTNESS CAVEAT — read before trusting any per-round number from this file.
//
// The kernels update shared state without atomics. mis2 writes c_array[tid]=-2
// before scanning its neighbours (kernel.cl:176) and writes cu_array[col[edge]]
// inside that scan (kernel.cl:182); the cu_array indirection exists precisely
// because the direct c_array update would race (see the "use status update
// array to avoid race" comment there), but the c_array[tid] write and the
// c_array[col[edge]] reads still race against each other across threads. On the
// GPU, whether a thread observes a lower-numbered thread's write is decided by
// the hardware's scheduling. A sequential host walk necessarily picks one
// particular interleaving — the fully serialized one, in which every thread
// sees all writes from every lower-numbered thread — so the independent set
// selected each round, the round count, and hence the total length of the
// address stream may differ from any given timing run. No offline method can
// fix this: the true round count is a property of the hardware's scheduling,
// not of the program.
//
// What survives the caveat is the shared-footprint result, which is what this
// tool measures. Each round's traffic is dominated by the structural gather in
// mis1/mis2 — col[edge] plus the c_array[col[edge]] and node_value[col[edge]]
// indirections — and that gather visits the same lines, from the same chiplets,
// in every round regardless of interleaving. The set of shared lines and their
// sharing degree are therefore stable; only per-line access counts scale with
// the round count.
//
// The host loop does not always terminate, and that is a property of the
// benchmark, not of this replay. mis1 folds c_array[col[edge]] over every
// out-edge including a self-edge (kernel.cl:123-127), so a vertex carrying a
// self-loop always finds min_array[tid] <= node_value[tid] and can never pass
// mis2's test at kernel.cl:161. If no in-neighbour of that vertex ever joins
// the set, c_array stays -1, mis1 keeps raising 'stop', and exec() spins
// forever; a timing run only ends because -max-inst cuts it off. The CSR
// generator does emit self-loops — generateUnoccupiedPosition draws the row and
// column independently — and at the sweep's sizes one or two vertices land in
// this state. The round loop therefore also stops at a fixed point: once a
// round changes no element of c_array/cu_array/s_array/min_array, every later
// round repeats it byte for byte, so continuing would only inflate the access
// counts of lines already recorded.
type replay struct {
	numNodes *int
	numItems *int
}

const (
	// blockSizeReplay mirrors BLOCK_SIZE in exec() (main.go:235).
	blockSizeReplay = 128

	// bigNumReplay is BIGNUM (kernel.cl:59).
	bigNumReplay = float32(99999999)

	// maxRoundsReplay is a backstop only: the fixed-point test in Run ends
	// the non-terminating case first, and converging graphs need a few dozen
	// rounds.
	maxRoundsReplay = 200
)

func init() {
	footprint.Register("mis", func() footprint.Replay { return &replay{} })
}

func (r *replay) Name() string { return "mis" }

func (r *replay) Flags(fs *flag.FlagSet) {
	r.numNodes = fs.Int("numNodes", 128,
		"The number of rows in the input matrix.")
	r.numItems = fs.Int("numItems", 64,
		"The number of rows in the input matrix.")
}

func (r *replay) Run(rec *footprint.Recorder, cfg footprint.Config) {
	numNodes := int32(*r.numNodes)
	numEdges := int32(*r.numItems)

	matrix := csr.
		MakeMatrixGenerator(uint32(numNodes), uint32(numEdges)).
		GenerateMatrix()
	row := matrix.RowOffsets
	col := matrix.ColumnNumbers

	// Allocation order matches the LASP branch of initMem (main.go:169-184),
	// the one the sweeps run under. The default branch orders the same
	// buffers differently; since every buffer is page-aligned and disjoint,
	// the order changes only the address labels, not the footprint.
	space := footprint.NewSpace()
	colBase := space.Alloc(uint64(numEdges) * 4)
	rowBase := space.Alloc(uint64(numNodes) * 4)
	minBase := space.Alloc(uint64(numNodes) * 4)
	cBase := space.Alloc(uint64(numNodes) * 4)
	cuBase := space.Alloc(uint64(numNodes) * 4)
	sBase := space.Alloc(uint64(numNodes) * 4)
	nodeValueBase := space.Alloc(uint64(numNodes) * 4)
	stopBase := space.Alloc(4)

	// node_value[i] = i (main.go:222-225).
	nodeValue := make([]float32, numNodes)
	for i := range nodeValue {
		nodeValue[i] = float32(i)
	}

	cArray := make([]int32, numNodes)
	cuArray := make([]int32, numNodes)
	sArray := make([]int32, numNodes)
	minArray := make([]float32, numNodes)

	grid := footprint.Grid{
		GlobalSize: [3]uint32{uint32(numNodes), 1, 1},
		LocalSize:  [3]uint16{blockSizeReplay, 1, 1},
	}

	numChiplets := cfg.NumChiplets()
	kernel := 0

	chips := wgChiplets(grid, cfg.PartitionFor(kernel), numChiplets)
	kernel++
	replayInit(rec, chips, cArray, cuArray, sArray,
		cBase, cuBase, sBase, numNodes)

	for round := 0; ; round++ {
		chips = wgChiplets(grid, cfg.PartitionFor(kernel), numChiplets)
		kernel++
		stop, moved := replayMis1(rec, chips, row, col, nodeValue,
			cArray, minArray, rowBase, colBase, nodeValueBase, cBase,
			minBase, stopBase, numNodes, numEdges)

		chips = wgChiplets(grid, cfg.PartitionFor(kernel), numChiplets)
		kernel++
		if replayMis2(rec, chips, row, col, nodeValue, minArray,
			cArray, cuArray, sArray,
			rowBase, colBase, nodeValueBase, minBase, cBase, cuBase, sBase,
			numNodes, numEdges) {
			moved = true
		}

		chips = wgChiplets(grid, cfg.PartitionFor(kernel), numChiplets)
		kernel++
		if replayMis3(rec, chips, cArray, cuArray, cBase, cuBase, numNodes) {
			moved = true
		}

		if !stop {
			break
		}

		if !moved {
			log.Printf("mis replay: fixed point after %d rounds with the "+
				"stop flag still set; stopping", round+1)

			break
		}

		if round+1 >= maxRoundsReplay {
			log.Printf("mis replay: hit the %d-round cap, stopping early",
				maxRoundsReplay)

			break
		}
	}
}

// replayInit transcribes the init kernel (kernel.cl:69-83).
func replayInit(
	rec *footprint.Recorder,
	chips []int8,
	cArray, cuArray, sArray []int32,
	cBase, cuBase, sBase uint64,
	numNodes int32,
) {
	for tid := int32(0); tid < numNodes; tid++ {
		chip := int(chips[tid/blockSizeReplay])

		rec.Touch(chip, cBase+uint64(tid)*4)
		cArray[tid] = -1

		rec.Touch(chip, cuBase+uint64(tid)*4)
		cuArray[tid] = -1

		rec.Touch(chip, sBase+uint64(tid)*4)
		sArray[tid] = 0
	}
}

// replayMis1 transcribes the mis1 kernel (kernel.cl:97-132). It reports the
// stop flag and, separately, whether min_array actually moved.
//
// col[edge] is counted once per iteration: the loop body contains no store, so
// its three textual reads (kernel.cl:124-126) are a single load, as is
// node_value[col[edge]] across kernel.cl:125-126.
func replayMis1(
	rec *footprint.Recorder,
	chips []int8,
	row, col []uint32,
	nodeValue []float32,
	cArray []int32,
	minArray []float32,
	rowBase, colBase, nodeValueBase, cBase, minBase, stopBase uint64,
	numNodes, numEdges int32,
) (stop, moved bool) {
	for tid := int32(0); tid < numNodes; tid++ {
		chip := int(chips[tid/blockSizeReplay])

		rec.Touch(chip, cBase+uint64(tid)*4)
		if cArray[tid] != -1 {
			continue
		}

		rec.Touch(chip, stopBase)
		stop = true

		rec.Touch(chip, rowBase+uint64(tid)*4)
		start := int32(row[tid])

		end := numEdges
		if tid+1 < numNodes {
			rec.Touch(chip, rowBase+uint64(tid+1)*4)
			end = int32(row[tid+1])
		}

		min := bigNumReplay
		for edge := start; edge < end; edge++ {
			rec.Touch(chip, colBase+uint64(edge)*4)
			c := col[edge]

			rec.Touch(chip, cBase+uint64(c)*4)
			if cArray[c] != -1 {
				continue
			}

			rec.Touch(chip, nodeValueBase+uint64(c)*4)
			if nodeValue[c] < min {
				min = nodeValue[c]
			}
		}

		rec.Touch(chip, minBase+uint64(tid)*4)
		if minArray[tid] != min {
			moved = true
		}
		minArray[tid] = min
	}

	return stop, moved
}

// replayMis2 transcribes the mis2 kernel (kernel.cl:146-187) and reports
// whether it moved any of s_array/c_array/cu_array.
//
// The && at kernel.cl:161 short-circuits, so c_array[tid] is read only when the
// node_value/min_array test passes. min_array is left at its Go zero value for
// vertices mis1 skipped; node_value[tid] = tid >= 0 then makes the first test
// fail, which is the same outcome as any interleaving in which mis1 did not
// write that slot.
func replayMis2(
	rec *footprint.Recorder,
	chips []int8,
	row, col []uint32,
	nodeValue, minArray []float32,
	cArray, cuArray, sArray []int32,
	rowBase, colBase, nodeValueBase, minBase, cBase, cuBase, sBase uint64,
	numNodes, numEdges int32,
) (moved bool) {
	for tid := int32(0); tid < numNodes; tid++ {
		chip := int(chips[tid/blockSizeReplay])

		rec.Touch(chip, nodeValueBase+uint64(tid)*4)
		rec.Touch(chip, minBase+uint64(tid)*4)
		if nodeValue[tid] >= minArray[tid] {
			continue
		}

		rec.Touch(chip, cBase+uint64(tid)*4)
		if cArray[tid] != -1 {
			continue
		}

		rec.Touch(chip, sBase+uint64(tid)*4)
		if sArray[tid] != 2 {
			moved = true
		}
		sArray[tid] = 2

		rec.Touch(chip, rowBase+uint64(tid)*4)
		start := int32(row[tid])

		end := numEdges
		if tid+1 < numNodes {
			rec.Touch(chip, rowBase+uint64(tid+1)*4)
			end = int32(row[tid+1])
		}

		rec.Touch(chip, cBase+uint64(tid)*4)
		cArray[tid] = -2
		moved = true

		for edge := start; edge < end; edge++ {
			rec.Touch(chip, colBase+uint64(edge)*4)
			c := col[edge]

			rec.Touch(chip, cBase+uint64(c)*4)
			if cArray[c] != -1 {
				continue
			}

			rec.Touch(chip, cuBase+uint64(c)*4)
			if cuArray[c] != -2 {
				moved = true
			}
			cuArray[c] = -2
		}
	}

	return moved
}

// replayMis3 transcribes the mis3 kernel (kernel.cl:195-205) and reports
// whether it moved c_array.
func replayMis3(
	rec *footprint.Recorder,
	chips []int8,
	cArray, cuArray []int32,
	cBase, cuBase uint64,
	numNodes int32,
) (moved bool) {
	for tid := int32(0); tid < numNodes; tid++ {
		chip := int(chips[tid/blockSizeReplay])

		rec.Touch(chip, cuBase+uint64(tid)*4)
		if cuArray[tid] != -2 {
			continue
		}

		rec.Touch(chip, cBase+uint64(tid)*4)
		if cArray[tid] != cuArray[tid] {
			moved = true
		}
		cArray[tid] = cuArray[tid]
	}

	return moved
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
