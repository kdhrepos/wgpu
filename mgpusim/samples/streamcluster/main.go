// streamcluster sample: thin shell that wires benchmark flags through the
// runner CLI defined in mgpusim/runner.
package main

import (
	"flag"

	"gitlab.com/akita/mgpusim/benchmarks/rodinia/streamcluster"
	"gitlab.com/akita/mgpusim/runner"
)

var (
	num  = flag.Int("num", 65536, "Number of points.")
	dim  = flag.Int("dim", 32, "Dimensionality of each point.")
	k    = flag.Int("k", 64, "Number of currently-open centers.")
	iter = flag.Int("iter", 16, "Number of candidate facilities (kernel launches).")
)

func main() {
	flag.Parse()

	r := new(runner.Runner).ParseFlag().Init()

	b := streamcluster.NewBenchmark(r.GPUDriver)
	b.Num = int32(*num)
	b.Dim = int32(*dim)
	b.K = int32(*k)
	b.Iter = int32(*iter)

	r.AddBenchmark(b)
	r.Run()
}
