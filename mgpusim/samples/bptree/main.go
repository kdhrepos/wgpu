package main

import (
	"flag"

	"gitlab.com/akita/mgpusim/benchmarks/rodinia/bptree"
	"gitlab.com/akita/mgpusim/runner"
)

var size = flag.Int("size", 3000000, "number of records (leaf keys) in the B+ tree")
var count = flag.Int("count", 5000, "number of queries to issue")
// Named "bptree-zipf-s", not "zipf-s": the runner defines a global "zipf-s"
// (CSR/graph generator skew, runner.go) and two flags with the same name in
// flag.CommandLine panic at init ("flag redefined").
var zipfS = flag.Float64("bptree-zipf-s", 0, "query key skew: <=1 (default) = uniform → spreads the L1 band over L2 for high cross-chiplet sharing + reuse; >1 = Zipfian s → concentrates on the apex path which then caches in L2")

func main() {
	flag.Parse()

	runner := new(runner.Runner).ParseFlag().Init()

	benchmark := bptree.NewBenchmark(runner.GPUDriver)
	benchmark.Size = *size
	benchmark.Count = *count
	benchmark.ZipfS = *zipfS

	runner.AddBenchmark(benchmark)
	runner.Run()
}
