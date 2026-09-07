package main

import (
	"flag"

	"gitlab.com/akita/mgpusim/benchmarks/pannotia/mst"
	"gitlab.com/akita/mgpusim/runner"
)

var numNodes = flag.Int("num-nodes", 4096, "Number of graph nodes.")
var numEdges = flag.Int("num-edges", 65536, "Number of graph edges.")

func main() {
	flag.Parse()

	runner := new(runner.Runner).ParseFlag().Init()

	benchmark := mst.NewBenchmark(runner.GPUDriver)
	benchmark.NumNodes = int32(*numNodes)
	benchmark.NumEdges = int32(*numEdges)

	runner.AddBenchmark(benchmark)
	runner.Run()
}
