package main

import (
	"flag"

	"gitlab.com/akita/mgpusim/benchmarks/rodinia/nn"
	"gitlab.com/akita/mgpusim/runner"
)

var numRecords = flag.Int("num-records", 1048576, "number of records to process")
var targetLat = flag.Float64("lat", 30.0, "target latitude")
var targetLng = flag.Float64("lng", 90.0, "target longitude")

func main() {
	flag.Parse()

	runner := new(runner.Runner).ParseFlag().Init()

	benchmark := nn.NewBenchmark(runner.GPUDriver)
	benchmark.NumRecords = *numRecords
	benchmark.TargetLat = float32(*targetLat)
	benchmark.TargetLng = float32(*targetLng)

	runner.AddBenchmark(benchmark)
	runner.Run()
}
