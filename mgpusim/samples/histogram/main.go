package main

import (
	"flag"

	"gitlab.com/akita/mgpusim/benchmarks/parboil/histogram"
	"gitlab.com/akita/mgpusim/runner"
)

var numElements = flag.Int("num-elements", 1<<20, "Number of input elements.")
var numBins = flag.Int("num-bins", 256, "Number of histogram bins.")

func main() {
	flag.Parse()

	runner := new(runner.Runner).ParseFlag().Init()

	benchmark := histogram.NewBenchmark(runner.GPUDriver)
	benchmark.NumElements = int32(*numElements)
	benchmark.NumBins = int32(*numBins)

	runner.AddBenchmark(benchmark)
	runner.Run()
}
