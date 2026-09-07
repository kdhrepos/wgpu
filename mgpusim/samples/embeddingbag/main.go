package main

import (
	"flag"

	"gitlab.com/akita/mgpusim/benchmarks/dnn/embeddingbag"
	"gitlab.com/akita/mgpusim/runner"
)

var (
	batchSize = flag.Int("B", 1024, "number of bags (batch size)")
	embedDim  = flag.Int("D", 128, "embedding dimension")
	vocabSize = flag.Int("V", 1<<20, "vocab size (rows of the embedding table)")
	bagLength = flag.Int("L", 32, "fixed bag length (indices per bag)")
	useZipf   = flag.Bool("zipf", false, "use Zipf(1.1) distribution for indices instead of uniform")
)

func main() {
	flag.Parse()

	r := new(runner.Runner).ParseFlag().Init()

	b := embeddingbag.NewBenchmark(r.GPUDriver)
	b.B = *batchSize
	b.D = *embedDim
	b.V = *vocabSize
	b.L = *bagLength
	b.Zipf = *useZipf

	r.AddBenchmark(b)
	r.Run()
}
