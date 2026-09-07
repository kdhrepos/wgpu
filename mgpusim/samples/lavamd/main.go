package main

import (
	"flag"

	"gitlab.com/akita/mgpusim/benchmarks/rodinia/lavamd"
	"gitlab.com/akita/mgpusim/runner"
)

var boxes1d = flag.Int("boxes1d", 8,
	"number of boxes per side (total boxes = boxes1d^3, "+
		"particles = total*100)")

func main() {
	flag.Parse()

	r := new(runner.Runner).ParseFlag().Init()

	benchmark := lavamd.NewBenchmark(r.GPUDriver)
	benchmark.Boxes1D = *boxes1d

	r.AddBenchmark(benchmark)

	r.Run()
}
