package main

import (
	"flag"

	"gitlab.com/akita/mgpusim/benchmarks/pannotia/color"
	"gitlab.com/akita/mgpusim/runner"
)

var NumNodes = flag.Int("num-nodes", 1024, "The number of rows in the input matrix.")
var NumEdges = flag.Int("num-edges", 4096, "The number of rows in the input matrix.")

func main() {
	flag.Parse()

	runner := new(runner.Runner).ParseFlag().Init()

	benchmark := color.NewBenchmark(runner.GPUDriver)
	benchmark.NumNodes = int32(*NumNodes)
	benchmark.NumEdges = int32(*NumEdges)

	runner.AddBenchmark(benchmark)

	runner.Run()
}
