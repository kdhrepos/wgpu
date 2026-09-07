package pagerank

import (
	"flag"

	"gitlab.com/akita/mgpusim/benchmarks/matrix/csr"
	"gitlab.com/akita/mgpusim/footprint"
)

// localSizeReplay mirrors the hardcoded localWorkSize in exec(). The grid is
// NumNodes*64 threads in groups of 64, and the kernel sets row = thread_id/64
// (native/kernels.cl:38-40), so one work-group is exactly one row.
const localSizeReplay = 64

// replay reproduces the address stream of PageRankUpdateGpu
// (benchmarks/heteromark/pagerank/native/kernels.cl:32-59) on the host.
//
// The row's 64 lanes stride the CSR segment by 64 (kernels.cl:48), so together
// they cover every entry in [row_A_start, row_A_end) exactly once — the same
// index set the host already knows from the generated matrix.
type replay struct {
	numNode       *int
	sparsity      *float64
	maxIterations *int
}

func init() {
	footprint.Register("pagerank", func() footprint.Replay { return &replay{} })
}

func (r *replay) Name() string { return "pagerank" }

func (r *replay) Flags(fs *flag.FlagSet) {
	r.numNode = fs.Int("node", 16, "The number of nodes")
	r.sparsity = fs.Float64("sparsity", 0.001, "The sparsity of the graph")
	r.maxIterations = fs.Int("iterations", 16, "The number of iterations")
}

// problemSize transcribes samples/pagerank/main.go:23-32.
func (r *replay) problemSize() (numNodes, numConnections uint32) {
	sparsity := *r.sparsity
	if sparsity > 1 {
		sparsity = 1
	}

	numNode := *r.numNode
	numConn := int(float64(numNode*numNode) * sparsity)
	if numConn < numNode {
		numConn = numNode
	}

	return uint32(numNode), uint32(numConn)
}

func (r *replay) Run(rec *footprint.Recorder, cfg footprint.Config) {
	numNodes, numConnections := r.problemSize()

	matrix := csr.
		MakeMatrixGenerator(numNodes, numConnections).
		GenerateMatrix()

	// Allocation order matches initMem (pagerank.go:154-163).
	space := footprint.NewSpace()
	pageRankBase := space.Alloc(uint64(numNodes) * 4)
	pageRankTempBase := space.Alloc(uint64(numNodes) * 4)
	rowDBase := space.Alloc(uint64(numNodes+1) * 4)
	colsBase := space.Alloc(uint64(numConnections) * 4)
	valBase := space.Alloc(uint64(numConnections) * 4)

	grid := footprint.Grid{
		GlobalSize: [3]uint32{numNodes * localSizeReplay, 1, 1},
		LocalSize:  [3]uint16{localSizeReplay, 1, 1},
	}
	numChiplets := cfg.NumChiplets()

	for iter := 0; iter < *r.maxIterations; iter++ {
		// exec() ping-pongs x and y every iteration (pagerank.go:192-212).
		xBase, yBase := pageRankBase, pageRankTempBase
		if iter%2 != 0 {
			xBase, yBase = pageRankTempBase, pageRankBase
		}

		partition := cfg.PartitionFor(iter)

		for row := uint32(0); row < numNodes; row++ {
			chiplet := footprint.ChipletOfWG(
				int(row), 0, grid, partition, numChiplets)

			// y[row] = 0.0; every lane stores the same address.
			rec.Touch(chiplet, yBase+uint64(row)*4)

			// rowOffset[row], rowOffset[row+1]
			rec.Touch(chiplet, rowDBase+uint64(row)*4)
			rec.Touch(chiplet, rowDBase+uint64(row+1)*4)

			start := matrix.RowOffsets[row]
			end := matrix.RowOffsets[row+1]
			for j := start; j < end; j++ {
				// vals[local_id] += val[jj] * x[col[jj]];
				rec.Touch(chiplet, valBase+uint64(j)*4)
				rec.Touch(chiplet, colsBase+uint64(j)*4)
				rec.Touch(chiplet, xBase+uint64(matrix.ColumnNumbers[j])*4)
			}

			// y[row] += vals[local_id]: one read and one write, by lane 0.
			rec.TouchN(chiplet, yBase+uint64(row)*4, 2)
		}
	}
}
