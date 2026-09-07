package main

import (
	"flag"

	"gitlab.com/akita/mgpusim/benchmarks/pannotia/cc"
	"gitlab.com/akita/mgpusim/runner"
)

var numNodes = flag.Int("num-nodes", 1024, "Number of graph nodes.")
var numEdges = flag.Int("num-edges", 4096, "Number of graph edges.")

func main() {
	flag.Parse()

	runner := new(runner.Runner).ParseFlag().Init()

	benchmark := cc.NewBenchmark(runner.GPUDriver)
	benchmark.NumNodes = int32(*numNodes)
	benchmark.NumEdges = int32(*numEdges)

	runner.AddBenchmark(benchmark)
	runner.Run()
}
