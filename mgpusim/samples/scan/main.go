package main

import (
	"flag"

	"gitlab.com/akita/mgpusim/benchmarks/shoc/scan"
	"gitlab.com/akita/mgpusim/runner"
)

var numElements = flag.Int("num-elements", 1<<20, "Number of elements to scan (rounded up to a multiple of block-size).")
var blockSize = flag.Int("block-size", 256, "Workgroup size for scan kernels.")

func main() {
	flag.Parse()

	runner := new(runner.Runner).ParseFlag().Init()

	benchmark := scan.NewBenchmark(runner.GPUDriver)
	benchmark.NumElements = int32(*numElements)
	benchmark.BlockSize = int32(*blockSize)

	runner.AddBenchmark(benchmark)
	runner.Run()
}
