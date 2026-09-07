package main

import (
	"flag"

	"gitlab.com/akita/mgpusim/benchmarks/rodinia/lud"
	"gitlab.com/akita/mgpusim/runner"
)

var matrixDim = flag.Int("matrix-dim", 1024, "matrix dimension")

func main() {
	flag.Parse()

	runner := new(runner.Runner).ParseFlag().Init()

	benchmark := lud.NewBenchmark(runner.GPUDriver)
	benchmark.MatrixDim = *matrixDim

	runner.AddBenchmark(benchmark)

	runner.Run()
}
