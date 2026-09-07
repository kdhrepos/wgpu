// Package radixsort implements the Radix Sort benchmark from the SHOC suite.
// OpenCL source: https://github.com/vetter/shoc (src/opencl/level1/sort/sort.cl)
//
// Algorithm (LSB radix sort, 1 bit per pass, 32 passes total):
//   reduce     — per-block count of 0-bits and 1-bits for the current bit position.
//                Writes 2 × numBlocks partial counts to isums[].
//   top_scan   — exclusive prefix scan over isums[] (single WG, 2 × numBlocks wide).
//   bottom_scan — scatter each element to its sorted destination using isums[].
//
// Characteristics: each pass, bottom_scan has every WG writing output elements whose
// destinations depend on a globally-shared prefix-sum (isums[]). The reduce phase
// resembles Histogram — all WGs atomically contribute counts for the same 2 buckets.
package radixsort

import (
	_ "embed"
	"log"
	"math/rand"
	"sort"

	"gitlab.com/akita/mgpusim/driver"
	"gitlab.com/akita/mgpusim/insts"
	"gitlab.com/akita/mgpusim/kernels"
)

// ReduceKernelArgs is the argument layout for the per-block histogram kernel.
// Kernel: reduce(__global uint *in, __global uint *isums, int n, __local uint *lmem, int shift)
type ReduceKernelArgs struct {
	In                  driver.GPUPtr
	Isums               driver.GPUPtr
	N                   int32
	Padding             int32
	Lmem                driver.LocalPtr // blockSize * sizeof(uint32)
	Shift               int32
	Padding2            int32
	HiddenGlobalOffsetX int64
	HiddenGlobalOffsetY int64
	HiddenGlobalOffsetZ int64
}

// TopScanKernelArgs is the argument layout for the block-sum scan kernel.
// Kernel: top_scan(__global uint *isums, int n, __local uint *lmem)
type TopScanKernelArgs struct {
	Isums               driver.GPUPtr
	N                   int32
	Padding             int32
	Lmem                driver.LocalPtr // 2 * numBlocks * sizeof(uint32)
	HiddenGlobalOffsetX int64
	HiddenGlobalOffsetY int64
	HiddenGlobalOffsetZ int64
}

// BottomScanKernelArgs is the argument layout for the scatter kernel.
// Kernel: bottom_scan(__global uint *in, __global uint *isums, __global uint *out, int n, __local uint *lmem, int shift)
type BottomScanKernelArgs struct {
	In                  driver.GPUPtr
	Isums               driver.GPUPtr
	Out                 driver.GPUPtr
	N                   int32
	Padding             int32
	Lmem                driver.LocalPtr // blockSize * sizeof(uint32)
	Shift               int32
	Padding2            int32
	HiddenGlobalOffsetX int64
	HiddenGlobalOffsetY int64
	HiddenGlobalOffsetZ int64
}

// Benchmark is the Radix Sort benchmark.
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

	hInput  []uint32
	hOutput []uint32

	// dA and dB are ping-pong buffers; dIsums holds per-block histograms.
	dA, dB, dIsums driver.GPUPtr

	useUnifiedMemory   bool
	useLASPMemoryAlloc bool
}

// NewBenchmark creates a new Radix Sort benchmark.
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
	if b.NumElements%b.BlockSize != 0 {
		b.NumElements = (b.NumElements/b.BlockSize + 1) * b.BlockSize
	}

	dataBytes := uint64(b.NumElements * 4)
	numBlocks := b.NumElements / b.BlockSize
	// isums holds 2 buckets (0-bit, 1-bit) × numBlocks entries per pass.
	isumsBytes := uint64(2 * numBlocks * 4)

	if b.useUnifiedMemory {
		b.dA = b.driver.AllocateUnifiedMemory(b.context, dataBytes)
		b.dB = b.driver.AllocateUnifiedMemory(b.context, dataBytes)
		b.dIsums = b.driver.AllocateUnifiedMemory(b.context, isumsBytes)
	} else if b.useLASPMemoryAlloc {
		b.dA = b.driver.AllocateMemoryLASP(b.context, dataBytes, "div4")
		b.dB = b.driver.AllocateMemoryLASP(b.context, dataBytes, "div4")
		b.dIsums = b.driver.AllocateMemoryLASP(b.context, isumsBytes, "div4")
	} else {
		b.dA = b.driver.AllocateMemory(b.context, dataBytes)
		b.dB = b.driver.AllocateMemory(b.context, dataBytes)
		b.dIsums = b.driver.AllocateMemory(b.context, isumsBytes)
	}
}

func (b *Benchmark) exec() {
	b.hInput = make([]uint32, b.NumElements)
	for i := range b.hInput {
		b.hInput[i] = rand.Uint32() //nolint:gosec
	}
	b.driver.MemCopyH2D(b.context, b.dA, b.hInput)

	numBlocks := b.NumElements / b.BlockSize
	// Local memory: blockSize uint32 values per WG.
	lmemSize := uint64(b.BlockSize * 4)
	// top_scan local memory: 2 * numBlocks entries.
	topLmemSize := uint64(2 * numBlocks * 4)

	globalWork := [3]uint32{uint32(b.NumElements), 1, 1}
	localWork := [3]uint16{uint16(b.BlockSize), 1, 1}
	topWork := [3]uint32{uint32(2 * numBlocks), 1, 1}
	topLocal := [3]uint16{uint16(2 * numBlocks), 1, 1}

	src, dst := b.dA, b.dB
	for shift := int32(0); shift < 32; shift++ {
		// 1. reduce: per-block histograms for bit `shift`
		reduceArgs := ReduceKernelArgs{
			In:    src,
			Isums: b.dIsums,
			N:     b.NumElements,
			Lmem:  driver.LocalPtr(lmemSize),
			Shift: shift,
		}
		b.driver.LaunchKernel(b.context, b.kernelReduce, globalWork, localWork, &reduceArgs)

		// 2. top_scan: exclusive scan over the 2*numBlocks block sums
		topArgs := TopScanKernelArgs{
			Isums: b.dIsums,
			N:     2 * numBlocks,
			Lmem:  driver.LocalPtr(topLmemSize),
		}
		b.driver.LaunchKernel(b.context, b.kernelTopScan, topWork, topLocal, &topArgs)

		// 3. bottom_scan: scatter elements to sorted positions
		bottomArgs := BottomScanKernelArgs{
			In:    src,
			Isums: b.dIsums,
			Out:   dst,
			N:     b.NumElements,
			Lmem:  driver.LocalPtr(lmemSize),
			Shift: shift,
		}
		b.driver.LaunchKernel(b.context, b.kernelBottomScan, globalWork, localWork, &bottomArgs)

		src, dst = dst, src
	}

	// After 32 passes the result is in src (last swap).
	b.hOutput = make([]uint32, b.NumElements)
	b.driver.MemCopyD2H(b.context, b.hOutput, src)
}

// Verify checks the GPU result against a CPU sort.
func (b *Benchmark) Verify() {
	cpuRef := make([]uint32, len(b.hInput))
	copy(cpuRef, b.hInput)
	sort.Slice(cpuRef, func(i, j int) bool { return cpuRef[i] < cpuRef[j] })

	passed := true
	for i, want := range cpuRef {
		if b.hOutput[i] != want {
			log.Printf("mismatch at [%d]: GPU %d CPU %d\n", i, b.hOutput[i], want)
			passed = false
			break
		}
	}
	if passed {
		log.Printf("RadixSort Passed!\n")
	} else {
		log.Panic("RadixSort verification failed")
	}
}
