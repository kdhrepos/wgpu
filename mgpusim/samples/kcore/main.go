package main

import (
	"flag"

	"gitlab.com/akita/mgpusim/benchmarks/pannotia/kcore"
	"gitlab.com/akita/mgpusim/runner"
)

var numNodes = flag.Int("num-nodes", 1024, "Number of graph nodes.")
var numEdges = flag.Int("num-edges", 4096, "Number of graph edges.")
var k = flag.Int("k", 2, "Target core number k.")

func main() {
	flag.Parse()

	runner := new(runner.Runner).ParseFlag().Init()

	benchmark := kcore.NewBenchmark(runner.GPUDriver)
	benchmark.NumNodes = int32(*numNodes)
	benchmark.NumEdges = int32(*numEdges)
	benchmark.K = int32(*k)

	runner.AddBenchmark(benchmark)
	runner.Run()
}
