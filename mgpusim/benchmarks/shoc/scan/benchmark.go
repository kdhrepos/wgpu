// Package scan implements the Scan (Prefix Sum) benchmark from the SHOC suite.
// OpenCL source: https://github.com/vetter/shoc (src/opencl/level1/scan/Scan.cl)
//
// Algorithm (Blelloch work-efficient parallel scan, 3-kernel):
//
//	reduce     — each block performs a local reduction; per-block sums written to blockSums[].
//	top_scan   — single-block exclusive scan over blockSums[].
//	bottom_scan — each block applies its scanned blockSum to produce the final output.
//
// Characteristics: reduce and bottom_scan use __local memory + barriers within each WG.
// top_scan synchronises across *all* WGs (processes the aggregate of every block),
// making this one of the few patterns that requires global-scope inter-WG ordering.
package scan

import (
	_ "embed"
	"log"
	"math"

	"gitlab.com/akita/mgpusim/driver"
	"gitlab.com/akita/mgpusim/insts"
	"gitlab.com/akita/mgpusim/kernels"
)

// ReduceKernelArgs is the argument layout for the per-block reduction kernel.
type ReduceKernelArgs struct {
	In                  driver.GPUPtr // float32 input array
	Isums               driver.GPUPtr // float32 per-block partial sums (output)
	N                   int32         // total number of elements
	Padding             int32
	Lmem                driver.LocalPtr // local scratch: BlockSize * sizeof(float32)
	HiddenGlobalOffsetX int64
	HiddenGlobalOffsetY int64
	HiddenGlobalOffsetZ int64
}

// TopScanKernelArgs is the argument layout for the single-block block-sum scan kernel.
type TopScanKernelArgs struct {
	Isums               driver.GPUPtr // float32 per-block sums (in-place exclusive scan)
	N                   int32         // number of blocks
	Padding             int32
	Lmem                driver.LocalPtr // local scratch: NumBlocks * sizeof(float32)
	HiddenGlobalOffsetX int64
	HiddenGlobalOffsetY int64
	HiddenGlobalOffsetZ int64
}

// BottomScanKernelArgs is the argument layout for the final scan + add kernel.
type BottomScanKernelArgs struct {
	Out                 driver.GPUPtr // float32 output (exclusive prefix sum)
	In                  driver.GPUPtr // float32 input array
	Isums               driver.GPUPtr // float32 scanned per-block sums
	N                   int32         // total number of elements
	Padding             int32
	Lmem                driver.LocalPtr // local scratch: BlockSize * sizeof(float32)
	HiddenGlobalOffsetX int64
	HiddenGlobalOffsetY int64
	HiddenGlobalOffsetZ int64
}

// Benchmark is the Scan benchmark.
type Benchmark struct {
	driver  *driver.Driver
	context *driver.Context
	gpus    []int
	queues  []*driver.CommandQueue

	kernelReduce     *insts.HsaCo
	kernelTopScan    *insts.HsaCo
	kernelBottomScan *insts.HsaCo

	// NumElements must be a multiple of BlockSize.
	NumElements int32 // default 1<<20
	BlockSize   int32 // default 256

	hInput  []float32
	hOutput []float32

	dInput, dOutput, dBlockSums driver.GPUPtr

	useUnifiedMemory   bool
	useLASPMemoryAlloc bool
}

// NewBenchmark creates a new Scan benchmark.
func NewBenchmark(d *driver.Driver) *Benchmark {
	b := new(Benchmark)
	b.driver = d
	b.context = d.Init()
	b.NumElements = 1 << 20
	b.BlockSize = 256
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
	b.kernelReduce = kernels.LoadProgramFromMemory(hsacoBytes, "reduce")
	if b.kernelReduce == nil {
		log.Panic("Failed to load reduce kernel")
	}
	b.kernelTopScan = kernels.LoadProgramFromMemory(hsacoBytes, "top_scan")
	if b.kernelTopScan == nil {
		log.Panic("Failed to load top_scan kernel")
	}
	b.kernelBottomScan = kernels.LoadProgramFromMemory(hsacoBytes, "bottom_scan")
	if b.kernelBottomScan == nil {
		log.Panic("Failed to load bottom_scan kernel")
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
	// Ensure NumElements is a multiple of BlockSize.
	if b.NumElements%b.BlockSize != 0 {
		b.NumElements = (b.NumElements/b.BlockSize + 1) * b.BlockSize
	}

	inputBytes := uint64(b.NumElements * 4)
	numBlocks := b.NumElements / b.BlockSize
	blockSumsBytes := uint64(numBlocks * 4)

	if b.useUnifiedMemory {
		b.dInput = b.driver.AllocateUnifiedMemory(b.context, inputBytes)
		b.dOutput = b.driver.AllocateUnifiedMemory(b.context, inputBytes)
		b.dBlockSums = b.driver.AllocateUnifiedMemory(b.context, blockSumsBytes)
	} else if b.useLASPMemoryAlloc {
		b.dInput = b.driver.AllocateMemoryLASP(b.context, inputBytes, "div4")
		b.dOutput = b.driver.AllocateMemoryLASP(b.context, inputBytes, "div4")
		b.dBlockSums = b.driver.AllocateMemoryLASP(b.context, blockSumsBytes, "div4")
	} else {
		b.dInput = b.driver.AllocateMemory(b.context, inputBytes)
		b.dOutput = b.driver.AllocateMemory(b.context, inputBytes)
		b.dBlockSums = b.driver.AllocateMemory(b.context, blockSumsBytes)
	}
}

func (b *Benchmark) exec() {
	// Simple repeating input pattern.
	b.hInput = make([]float32, b.NumElements)
	for i := range b.hInput {
		b.hInput[i] = float32(i%17) + 1.0
	}
	b.driver.MemCopyH2D(b.context, b.dInput, b.hInput)

	numBlocks := b.NumElements / b.BlockSize
	// Local memory size: BlockSize float32 values.
	lmemSize := uint64(b.BlockSize * 4)
	// Top-scan local memory: numBlocks float32 values.
	topLmemSize := uint64(numBlocks * 4)

	globalWork := [3]uint32{uint32(b.NumElements), 1, 1}
	localWork := [3]uint16{uint16(b.BlockSize), 1, 1}

	// 1. Reduce: per-block partial sums → dBlockSums
	reduceArgs := ReduceKernelArgs{
		In:    b.dInput,
		Isums: b.dBlockSums,
		N:     b.NumElements,
		Lmem:  driver.LocalPtr(lmemSize),
	}
	b.driver.LaunchKernel(b.context, b.kernelReduce, globalWork, localWork, &reduceArgs)

	// 2. TopScan: exclusive scan over dBlockSums (single WG of numBlocks threads)
	topArgs := TopScanKernelArgs{
		Isums: b.dBlockSums,
		N:     numBlocks,
		Lmem:  driver.LocalPtr(topLmemSize),
	}
	b.driver.LaunchKernel(b.context, b.kernelTopScan,
		[3]uint32{uint32(numBlocks), 1, 1},
		[3]uint16{uint16(numBlocks), 1, 1},
		&topArgs)

	// 3. BottomScan: local scan + add scanned block sum → dOutput
	bottomArgs := BottomScanKernelArgs{
		Out:   b.dOutput,
		In:    b.dInput,
		Isums: b.dBlockSums,
		N:     b.NumElements,
		Lmem:  driver.LocalPtr(lmemSize),
	}
	b.driver.LaunchKernel(b.context, b.kernelBottomScan, globalWork, localWork, &bottomArgs)

	b.hOutput = make([]float32, b.NumElements)
	b.driver.MemCopyD2H(b.context, b.hOutput, b.dOutput)
}

// Verify checks the GPU prefix sum against a CPU sequential exclusive scan.
func (b *Benchmark) Verify() {
	cpuOut := make([]float32, b.NumElements)
	var acc float32
	for i, v := range b.hInput {
		cpuOut[i] = acc
		acc += v
	}

	passed := true
	for i, want := range cpuOut {
		got := b.hOutput[i]
		if math.Abs(float64(got-want)) > 1e-3*math.Abs(float64(want))+1e-5 {
			log.Printf("mismatch at [%d]: GPU %.6f CPU %.6f\n", i, got, want)
			passed = false
			if !passed {
				break // report first mismatch only
			}
		}
	}
	if passed {
		log.Printf("Scan Passed!\n")
	} else {
		log.Panic("Scan verification failed")
	}
}
