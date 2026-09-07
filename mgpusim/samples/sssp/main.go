package main

import (
	"flag"

	"gitlab.com/akita/mgpusim/benchmarks/pannotia/sssp"
	"gitlab.com/akita/mgpusim/runner"
)

var NumNodes = flag.Int("num-nodes", 262144, "The number of rows in the input matrix.")
var NumItems = flag.Int("num-items", 524288, "The number of rows in the input matrix.")

func main() {
	flag.Parse()

	runner := new(runner.Runner).ParseFlag().Init()

	benchmark := sssp.NewBenchmark(runner.GPUDriver)
	benchmark.NumNodes = *NumNodes
	benchmark.NumItems = *NumItems

	runner.AddBenchmark(benchmark)

	runner.Run()
}
