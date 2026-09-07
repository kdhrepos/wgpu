package main

import (
	"flag"

	"gitlab.com/akita/mgpusim/benchmarks/dnn/relu"
	"gitlab.com/akita/mgpusim/runner"
)

var numData = flag.Int("n", 4096, "The number of samples to filter.")

func main() {
	flag.Parse()

	runner := new(runner.Runner).ParseFlag().Init()

	benchmark := relu.NewBenchmark(runner.GPUDriver)
	benchmark.Length = *numData

	runner.AddBenchmark(benchmark)

	runner.Run()
}
