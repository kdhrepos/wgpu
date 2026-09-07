package main

import (
	"flag"

	"gitlab.com/akita/mgpusim/benchmarks/tests/dummy1"
	"gitlab.com/akita/mgpusim/runner"
)

func main() {
	flag.Parse()

	runner := new(runner.Runner).ParseFlag().Init()

	benchmark := dummy1.NewBenchmark(runner.GPUDriver)

	runner.AddBenchmark(benchmark)

	runner.Run()
}
