package main

import (
	"flag"

	"gitlab.com/akita/mgpusim/benchmarks/rodinia/particlefilter"
	"gitlab.com/akita/mgpusim/runner"
)

var nparticles = flag.Int("nparticles", 1024, "Number of particles.")
var iszX = flag.Int("isz-x", 128, "Image width.")
var iszY = flag.Int("isz-y", 128, "Image height.")
var nfr = flag.Int("nfr", 10, "Number of frames to process.")

func main() {
	flag.Parse()

	runner := new(runner.Runner).ParseFlag().Init()

	benchmark := particlefilter.NewBenchmark(runner.GPUDriver)
	benchmark.Nparticles = int32(*nparticles)
	benchmark.IszX = int32(*iszX)
	benchmark.IszY = int32(*iszY)
	benchmark.Nfr = int32(*nfr)

	runner.AddBenchmark(benchmark)
	runner.Run()
}
