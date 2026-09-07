package main

import (
	"flag"

	_ "net/http/pprof"

	"gitlab.com/akita/mgpusim/benchmarks/raytracing"
	"gitlab.com/akita/mgpusim/runner"
)

var widthFlag = flag.Int("width", 256, "The framebuffer width in pixels.")
var heightFlag = flag.Int("height", 256, "The framebuffer height in pixels.")

func main() {
	flag.Parse()

	runner := new(runner.Runner).ParseFlag().Init()

	benchmark := raytracing.NewBenchmark(runner.GPUDriver)
	benchmark.Width = *widthFlag
	benchmark.Height = *heightFlag

	runner.AddBenchmark(benchmark)

	runner.Run()
}
