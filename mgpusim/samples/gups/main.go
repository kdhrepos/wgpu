package main

import (
	"flag"

	"gitlab.com/akita/mgpusim/benchmarks/mafiaports/gups"
	"gitlab.com/akita/mgpusim/runner"
)

var threadBlockSizeFlag = flag.Int("thread-block-size", 32, "Dunno")
var nThreadBlocksFlag = flag.Int("nthread-blocks", 128, "Dunno")
var tableParamFlag = flag.Int("table-param", 1024, "")

func main() {
	flag.Parse()

	runner := new(runner.Runner).ParseFlag().Init()

	benchmark := gups.NewBenchmark(runner.GPUDriver)
	benchmark.ThreadBlockSize = uint64(*threadBlockSizeFlag)
	benchmark.NThreadBlocks = uint64(*nThreadBlocksFlag)
	benchmark.TableParam = uint64(*tableParamFlag)

	runner.AddBenchmark(benchmark)

	runner.Run()
}
