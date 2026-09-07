package main

import (
	"flag"

	"gitlab.com/akita/mgpusim/benchmarks/polybench/doitgen"
	"gitlab.com/akita/mgpusim/runner"
)

var rFlag = flag.Int("r", 64, "Dunno")
var qFlag = flag.Int("q", 64, "Dunno")
var pFlag = flag.Int("p", 64, "Dunno")

func main() {
	flag.Parse()

	runner := new(runner.Runner).ParseFlag().Init()

	benchmark := doitgen.NewBenchmark(runner.GPUDriver)
	benchmark.NR = *rFlag
	benchmark.NQ = *qFlag
	benchmark.NP = *pFlag

	runner.AddBenchmark(benchmark)

	runner.Run()
}
