package main

import (
	"flag"

	_ "net/http/pprof"

	"gitlab.com/akita/mgpusim/benchmarks/rodinia/gaussian"
	"gitlab.com/akita/mgpusim/runner"
)

var sizeFlag = flag.Uint("size", 1024, "The row and column size of the matrix.")

func main() {
	flag.Parse()

	runner := new(runner.Runner).ParseFlag().Init()

	benchmark := gaussian.NewBenchmark(runner.GPUDriver)
	benchmark.Size = int(*sizeFlag)

	runner.AddBenchmark(benchmark)

	runner.Run()
}
