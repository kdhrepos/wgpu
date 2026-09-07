package main

import (
	"flag"

	frsimple "gitlab.com/akita/mgpusim/benchmarks/pannotia/fr-simple"
	"gitlab.com/akita/mgpusim/runner"
)

var numNodes = flag.Int("num-nodes", 1024, "Number of graph nodes.")
var numEdges = flag.Int("num-edges", 8192, "Number of graph edges.")

func main() {
	flag.Parse()

	runner := new(runner.Runner).ParseFlag().Init()

	benchmark := frsimple.NewBenchmark(runner.GPUDriver)
	benchmark.NumNodes = int32(*numNodes)
	benchmark.NumEdges = int32(*numEdges)

	runner.AddBenchmark(benchmark)
	runner.Run()
}
