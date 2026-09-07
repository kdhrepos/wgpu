package main

import (
	"flag"

	"gitlab.com/akita/mgpusim/benchmarks/pannotia/bc"
	"gitlab.com/akita/mgpusim/runner"
)

var numNodes = flag.Int("num-nodes", 1024, "Number of graph nodes.")
var numEdges = flag.Int("num-edges", 4096, "Number of graph edges.")
var numSrc = flag.Int("num-src", 1, "Number of source vertices for BC accumulation.")

func main() {
	flag.Parse()

	runner := new(runner.Runner).ParseFlag().Init()

	benchmark := bc.NewBenchmark(runner.GPUDriver)
	benchmark.NumNodes = int32(*numNodes)
	benchmark.NumEdges = int32(*numEdges)
	benchmark.NumSrc = int32(*numSrc)

	runner.AddBenchmark(benchmark)
	runner.Run()
}
