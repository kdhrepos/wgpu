// Package histogram implements the Histogram benchmark from the Parboil suite.
// OpenCL source: https://github.com/yuhc/gpu-parboil (benchmarks/histo/src-opencl/)
//
// Algorithm:
//   Each workgroup processes a contiguous chunk of the input array.
//   Every element is mapped to a bin via (element % NumBins) and accumulated
//   with an atomic_add into the global output histogram.
//
// Characteristics: all WGs simultaneously issue atomic writes to the same
// NumBins-entry output array — a canonical high inter-WG sharing workload.
// Input access is sequential within a WG but random across WGs (irregular at
// the cache-line level once NumBins is small relative to the array).
package histogram

import (
	_ "embed"
	"log"
	"math/rand"

	"gitlab.com/akita/mgpusim/driver"
	"gitlab.com/akita/mgpusim/insts"
	"gitlab.com/akita/mgpusim/kernels"
)

// HistoKernelArgs is the argument layout for the histogram kernel.
type HistoKernelArgs struct {
	Input               driver.GPUPtr // uint32 input data
	Output              driver.GPUPtr // uint32 histogram bins
	NumElements         int32
	NumBins             int32
	HiddenGlobalOffsetX int64
	HiddenGlobalOffsetY int64
	HiddenGlobalOffsetZ int64
}

// Benchmark is the Histogram benchmark.
type Benchmark struct {
	driver  *driver.Driver
	context *driver.Context
	gpus    []int
	queues  []*driver.CommandQueue
	kernel  *insts.HsaCo

	NumElements int32 // number of input elements (default 1<<20)
	NumBins     int32 // number of histogram bins (default 256)

	hInput  []uint32
	hOutput []uint32

	dInput, dOutput driver.GPUPtr

	useUnifiedMemory   bool
	useLASPMemoryAlloc bool
}

// NewBenchmark creates a new Histogram benchmark.
func NewBenchmark(d *driver.Driver) *Benchmark {
	b := new(Benchmark)
	b.driver = d
	b.context = d.Init()
	b.NumElements = 1 << 20
	b.NumBins = 256
	b.loadProgram()
	return b
}

// SelectGPU configures which GPU the benchmark uses.
func (b *Benchmark) SelectGPU(gpus []int) {
	b.gpus = gpus
}

// SetUnifiedMemory switches to unified memory allocation.
func (b *Benchmark) SetUnifiedMemory() {
	b.useUnifiedMemory = true
}

// SetLASPMemoryAlloc switches to LASP-aware memory allocation.
func (b *Benchmark) SetLASPMemoryAlloc() {
	b.useLASPMemoryAlloc = true
}

//go:embed kernels.hsaco
var hsacoBytes []byte

func (b *Benchmark) loadProgram() {
	b.kernel = kernels.LoadProgramFromMemory(hsacoBytes, "histo_kernel")
	if b.kernel == nil {
		log.Panic("Failed to load histo_kernel")
	}
}

// Run executes the benchmark.
func (b *Benchmark) Run() {
	for _, gpu := range b.gpus {
		b.driver.SelectGPU(b.context, gpu)
		b.queues = append(b.queues, b.driver.CreateCommandQueue(b.context))
	}
	b.initMem()
	b.exec()
}

func (b *Benchmark) initMem() {
	inputBytes := uint64(b.NumElements * 4)
	outputBytes := uint64(b.NumBins * 4)

	if b.useUnifiedMemory {
		b.dInput = b.driver.AllocateUnifiedMemory(b.context, inputBytes)
		b.dOutput = b.driver.AllocateUnifiedMemory(b.context, outputBytes)
	} else if b.useLASPMemoryAlloc {
		b.dInput = b.driver.AllocateMemoryLASP(b.context, inputBytes, "div4")
		b.dOutput = b.driver.AllocateMemoryLASP(b.context, outputBytes, "div4")
	} else {
		b.dInput = b.driver.AllocateMemory(b.context, inputBytes)
		b.dOutput = b.driver.AllocateMemory(b.context, outputBytes)
	}
}

func (b *Benchmark) exec() {
	// Generate input: uniform random values in [0, NumBins)
	b.hInput = make([]uint32, b.NumElements)
	for i := range b.hInput {
		b.hInput[i] = uint32(rand.Int31n(b.NumBins)) //nolint:gosec
	}
	b.driver.MemCopyH2D(b.context, b.dInput, b.hInput)

	// Zero the output histogram
	hZero := make([]uint32, b.NumBins)
	b.driver.MemCopyH2D(b.context, b.dOutput, hZero)

	blockSize := int32(256)
	globalSize := b.NumElements
	if b.NumElements%blockSize > 0 {
		globalSize = (b.NumElements/blockSize + 1) * blockSize
	}

	args := HistoKernelArgs{
		Input:       b.dInput,
		Output:      b.dOutput,
		NumElements: b.NumElements,
		NumBins:     b.NumBins,
	}
	b.driver.LaunchKernel(b.context, b.kernel,
		[3]uint32{uint32(globalSize), 1, 1},
		[3]uint16{uint16(blockSize), 1, 1},
		&args)

	b.hOutput = make([]uint32, b.NumBins)
	b.driver.MemCopyD2H(b.context, b.hOutput, b.dOutput)
}

// Verify checks the GPU histogram against a CPU reference.
func (b *Benchmark) Verify() {
	cpuHisto := make([]uint32, b.NumBins)
	for _, v := range b.hInput {
		cpuHisto[v%uint32(b.NumBins)]++
	}

	passed := true
	for i := int32(0); i < b.NumBins; i++ {
		if b.hOutput[i] != cpuHisto[i] {
			log.Printf("mismatch at bin %d: GPU %d CPU %d\n", i, b.hOutput[i], cpuHisto[i])
			passed = false
		}
	}
	if passed {
		log.Printf("Histogram Passed!\n")
	} else {
		log.Panic("Histogram verification failed")
	}
}
