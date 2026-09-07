package main

import (
	"flag"

	_ "net/http/pprof"

	"gitlab.com/akita/mgpusim/benchmarks/xsbench"
	"gitlab.com/akita/mgpusim/runner"
)

var isotopesFlag = flag.Int("isotopes", 68,
	"Number of isotopes (68 = H-M Small, 355 = H-M Large; Large needs "+
		"-grid-type=nuclide or ~5.7 GB host RAM for the unionized index grid).")
var gridpointsFlag = flag.Int("gridpoints", 11303,
	"Number of gridpoints per isotope.")
var lookupsFlag = flag.Int("lookups", 500000,
	"Number of macroscopic XS lookups (work items).")
var gridTypeFlag = flag.String("grid-type", "unionized",
	"Lookup grid type: unionized, nuclide, or hash.")
var hashBinsFlag = flag.Int("hash-bins", 10000,
	"Number of hash bins (hash grid type only).")

func main() {
	flag.Parse()

	runner := new(runner.Runner).ParseFlag().Init()

	benchmark := xsbench.NewBenchmark(runner.GPUDriver)
	benchmark.NIsotopes = int64(*isotopesFlag)
	benchmark.NGridpoints = int64(*gridpointsFlag)
	benchmark.Lookups = int32(*lookupsFlag)
	benchmark.HashBins = int32(*hashBinsFlag)
	benchmark.SetGridType(*gridTypeFlag)

	runner.AddBenchmark(benchmark)

	runner.Run()
}
