package main

import (
	"flag"

	"gitlab.com/akita/mgpusim/benchmarks/polybench/fdtd2d"
	"gitlab.com/akita/mgpusim/runner"
)

var nxFlag = flag.Int("nx", 256, "Dunno")
var nyFlag = flag.Int("ny", 64, "Dunno")
var tMax = flag.Int("max_steps", 1, "Dunno")

func main() {
	flag.Parse()

	runner := new(runner.Runner).ParseFlag().Init()

	benchmark := fdtd2d.NewBenchmark(runner.GPUDriver)
	benchmark.NX = *nxFlag
	benchmark.NY = *nyFlag
	benchmark.TMax = *tMax

	runner.AddBenchmark(benchmark)

	runner.Run()
}
