package spmm

import (
	"flag"

	"gitlab.com/akita/mgpusim/benchmarks/matrix/csr"
	"gitlab.com/akita/mgpusim/footprint"
)

// vectorsPerWG is how many SUBWAVE_SIZE-wide vectors a work-group holds.
// csrmv gives one vector one row (spmm.cl:49,55), and the launch sizes the grid
// so num_vectors >= num_rows, so vector_id == row and work-group = row/32.
const vectorsPerWG = wgSize / subwaveSize

// replay reproduces the address stream of csrmv_batched (spmm.cl:97-122) on the
// host.
//
// The dense-column loop is inside the kernel (spmm.cl:118), and only the
// denseB/denseC addresses move with curr_col: row_offset, col and val are
// re-read identically on all num_cols_C passes. Those collapse to a single
// TouchN with a count of n rather than an n-deep loop.
type replay struct {
	dim      *int
	n        *int
	sparsity *float64
}

func init() {
	footprint.Register("spmm", func() footprint.Replay { return &replay{} })
}

func (r *replay) Name() string { return "spmm" }

func (r *replay) Flags(fs *flag.FlagSet) {
	r.dim = fs.Int("dim", 128, "Rows/cols of the square sparse matrix A.")
	r.n = fs.Int("n", 128, "Number of dense columns of B/C.")
	r.sparsity = fs.Float64("sparsity", 0.01,
		"Ratio of non-zero elements to all elements of the sparse matrix.")
}

func (r *replay) Run(rec *footprint.Recorder, cfg footprint.Config) {
	dim := int32(*r.dim)
	n := uint64(*r.n)
	nItems := int32(float64(dim) * float64(dim) * *r.sparsity)

	matrix := csr.
		MakeMatrixGenerator(uint32(dim), uint32(nItems)).
		GenerateMatrix()

	// Allocation order matches initMem (benchmark.go:166-172).
	space := footprint.NewSpace()
	valBase := space.Alloc(uint64(nItems) * 4)
	colsBase := space.Alloc(uint64(nItems) * 4)
	rowDBase := space.Alloc(uint64(dim+1) * 4)
	bBase := space.Alloc(uint64(dim) * n * 4)
	cBase := space.Alloc(uint64(dim) * n * 4)
	alphaBase := space.Alloc(4)
	betaBase := space.Alloc(4)

	grid := footprint.Grid{
		GlobalSize: [3]uint32{roundUp(uint32(dim)*subwaveSize, wgSize), 1, 1},
		LocalSize:  [3]uint16{wgSize, 1, 1},
	}
	partition := cfg.PartitionFor(0)
	numChiplets := cfg.NumChiplets()

	for row := int32(0); row < dim; row++ {
		chiplet := footprint.ChipletOfWG(
			int(row)/vectorsPerWG, 0, grid, partition, numChiplets)

		// row_offset[row], row_offset[row+1], once per dense column.
		rec.TouchN(chiplet, rowDBase+uint64(row)*4, n)
		rec.TouchN(chiplet, rowDBase+uint64(row+1)*4, n)

		// The lane-strided loop (spmm.cl:61) covers every j in the row's CSR
		// segment exactly once across the subwave.
		start := matrix.RowOffsets[row]
		end := matrix.RowOffsets[row+1]
		for j := start; j < end; j++ {
			rec.TouchN(chiplet, valBase+uint64(j)*4, n)
			rec.TouchN(chiplet, colsBase+uint64(j)*4, n)

			// x[off_x + col[j]*ldx] with x = denseB+curr_col and ldx = n
			// (spmm.cl:64,120): across curr_col this walks row col[j] of B.
			touchElems(rec, chiplet,
				bBase+uint64(matrix.ColumnNumbers[j])*n*4, n)
		}

		// y[off_y + row*ldy] with y = denseC+curr_col. beta is 0, so the
		// _beta == 0 branch runs and it is a pure store (spmm.cl:89-90).
		touchElems(rec, chiplet, cBase+uint64(row)*n*4, n)
	}

	// alpha and beta are re-read at the top of every csrmv call by every
	// thread (spmm.cl:52-53). Lanes share the address, so one access per
	// work-group per dense column, padding work-groups included.
	for wg := 0; wg < grid.NumWGX(); wg++ {
		chiplet := footprint.ChipletOfWG(wg, 0, grid, partition, numChiplets)
		rec.TouchN(chiplet, alphaBase, n)
		rec.TouchN(chiplet, betaBase, n)
	}
}

// touchElems records one access to each of n consecutive 4B elements starting
// at addr, folding them into one TouchN per spanned cacheline. n here is the
// dense-column count, so walking element by element would cost n times more.
func touchElems(rec *footprint.Recorder, chiplet int, addr, n uint64) {
	end := addr + n*4
	for line := addr &^ (footprint.LineBytes - 1); line < end; line += footprint.LineBytes {
		lo := line
		if lo < addr {
			lo = addr
		}

		hi := line + footprint.LineBytes
		if hi > end {
			hi = end
		}

		rec.TouchN(chiplet, lo, (hi-lo)/4)
	}
}
