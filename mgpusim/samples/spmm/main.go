package main

import (
	"flag"

	"gitlab.com/akita/mgpusim/benchmarks/clsparse/spmm"
	"gitlab.com/akita/mgpusim/runner"
)

// Dim is the number of rows/cols of the square sparse matrix A (M = K = Dim).
var Dim = flag.Int("dim", 128, "Rows/cols of the square sparse matrix A.")

// N is the number of dense columns of B/C.
var N = flag.Int("n", 128, "Number of dense columns of B/C.")

// Sparsity is nnz / Dim^2.
var Sparsity = flag.Float64("sparsity", 0.01,
	"Ratio of non-zero elements to all elements of the sparse matrix.")

func main() {
	flag.Parse()

	runner := new(runner.Runner).ParseFlag().Init()

	benchmark := spmm.NewBenchmark(runner.GPUDriver)
	benchmark.Dim = int32(*Dim)
	benchmark.N = int32(*N)
	benchmark.Sparsity = *Sparsity

	runner.AddBenchmark(benchmark)

	runner.Run()
}
