package main

import (
	"flag"

	"gitlab.com/akita/mgpusim/benchmarks/polybench/gemm"
	"gitlab.com/akita/mgpusim/runner"
)

var iFlag = flag.Int("i", 16, "Dunno")
var jFlag = flag.Int("j", 16, "Dunno")
var kFlag = flag.Int("k", 16, "Dunno")

func main() {
	flag.Parse()

	runner := new(runner.Runner).ParseFlag().Init()

	benchmark := gemm.NewBenchmark(runner.GPUDriver)
	benchmark.NI = *iFlag
	benchmark.NJ = *jFlag
	benchmark.NK = *kFlag
	benchmark.Alpha = 0.5 //42123.0
	benchmark.Beta = 0.5  //324.0

	runner.AddBenchmark(benchmark)

	runner.Run()
}
