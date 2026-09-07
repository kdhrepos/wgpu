package spmv

import (
	"flag"

	"gitlab.com/akita/mgpusim/benchmarks/matrix/csr"
	"gitlab.com/akita/mgpusim/footprint"
)

// blockSizeReplay mirrors the hardcoded local size in exec().
const blockSizeReplay = 128

// replay reproduces the address stream of spmv_csr_scalar_kernel
// (benchmarks/shoc/spmv/spmv.cl:63-82) on the host.
//
// Every index the kernel dereferences comes from the CSR arrays, which the
// host generates before the copy to the GPU, so no execution is needed to know
// them.
type replay struct {
	dim      *int
	sparsity *float64
}

func init() {
	footprint.Register("spmv", func() footprint.Replay { return &replay{} })
}

func (r *replay) Name() string { return "spmv" }

func (r *replay) Flags(fs *flag.FlagSet) {
	r.dim = fs.Int("dim", 1024, "the dimension of the matrix")
	r.sparsity = fs.Float64("sparsity", 0.01, "the sparsity of the matrix")
}

func (r *replay) Run(rec *footprint.Recorder, cfg footprint.Config) {
	dim := int32(*r.dim)
	nItems := int32(float64(dim) * float64(dim) * *r.sparsity)

	matrix := csr.
		MakeMatrixGenerator(uint32(dim), uint32(nItems)).
		GenerateMatrix()

	// Allocation order matches initMem (spmv.go:123-147).
	space := footprint.NewSpace()
	valBase := space.AllocNamed("val", uint64(nItems)*4)
	vecBase := space.AllocNamed("vec", uint64(dim)*4)
	colsBase := space.AllocNamed("cols", uint64(nItems)*4)
	rowDBase := space.AllocNamed("rowDelimiters", uint64(dim+1)*4)
	outBase := space.AllocNamed("out", uint64(dim)*4)
	rec.SetBuffers(space.Buffers())

	grid := footprint.Grid{
		GlobalSize: [3]uint32{uint32(dim), 1, 1},
		LocalSize:  [3]uint16{blockSizeReplay, 1, 1},
	}
	partition := cfg.PartitionFor(0)
	numChiplets := cfg.NumChiplets()

	for row := int32(0); row < dim; row++ {
		chiplet := footprint.ChipletOfWG(
			int(row)/blockSizeReplay, 0, grid, partition, numChiplets)

		// int start = rowDelimiters[myRow]; int end = rowDelimiters[myRow+1];
		rec.Touch(chiplet, rowDBase+uint64(row)*4)
		rec.Touch(chiplet, rowDBase+uint64(row+1)*4)

		start := matrix.RowOffsets[row]
		end := matrix.RowOffsets[row+1]
		for j := start; j < end; j++ {
			// int col = cols[j]; t += val[j] * vec[col];
			rec.Touch(chiplet, colsBase+uint64(j)*4)
			rec.Touch(chiplet, valBase+uint64(j)*4)
			rec.Touch(chiplet, vecBase+uint64(matrix.ColumnNumbers[j])*4)
		}

		// out[myRow] = t;
		rec.Touch(chiplet, outBase+uint64(row)*4)
	}
}
