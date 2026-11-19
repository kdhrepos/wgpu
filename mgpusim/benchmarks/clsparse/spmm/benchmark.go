// Package spmm implements sparse-matrix * dense-matrix multiplication
// (SpMM: C = A_sparse * B_dense) ported from clSPARSE (AMD, Apache-2.0)
// kernel src/library/kernels/csrmm_general.cl (csrmv_batched). The device
// kernel is upstream-verbatim; only the host harness is MGPUSim-specific.
package spmm

import (
	_ "embed"
	"log"
	"math/rand"

	"gitlab.com/akita/mgpusim/benchmarks/matrix/csr"
	"gitlab.com/akita/mgpusim/driver"
	"gitlab.com/akita/mgpusim/insts"
	"gitlab.com/akita/mgpusim/kernels"
)

// subwaveSize / wgSize mirror the SUBWAVE_SIZE / WG_SIZE macros compiled into
// spmm.cl. The launch grid must agree with them.
const (
	subwaveSize = 8
	wgSize      = 256
)

//go:embed spmm.hsaco
var hsacoBytes []byte

// KernelArgs matches the csrmv_batched signature. Explicit padding fields keep
// every pointer 8-byte aligned so binary.Write reproduces the HSA kernarg ABI
// (offsets in the trailing comments). INDEX_TYPE=int32, SIZE_TYPE=uint32,
// VALUE_TYPE=float32.
type KernelArgs struct {
	NumRows   int32         // 0
	Pad0      int32         // 4
	Alpha     driver.GPUPtr // 8
	OffAlpha  uint32        // 16
	Pad1      int32         // 20
	RowOffset driver.GPUPtr // 24
	Col       driver.GPUPtr // 32
	Val       driver.GPUPtr // 40
	DenseB    driver.GPUPtr // 48
	LdB       int32         // 56
	OffB      uint32        // 60
	Beta      driver.GPUPtr // 64
	OffBeta   uint32        // 72
	Pad2      int32         // 76
	DenseC    driver.GPUPtr // 80
	NumRowsC  int32         // 88
	NumColsC  int32         // 92
	LdC       int32         // 96
	OffC      uint32        // 100

	HiddenGlobalOffsetX int64 // 104
	HiddenGlobalOffsetY int64 // 112
	HiddenGlobalOffsetZ int64 // 120
}

// Benchmark sets up the SpMM run.
type Benchmark struct {
	driver  *driver.Driver
	context *driver.Context
	gpus    []int
	queues  []*driver.CommandQueue

	useUnifiedMemory   bool
	useLASPMemoryAlloc bool

	kernel *insts.HsaCo

	// Dim = number of rows/cols of the square sparse matrix A (M = K = Dim).
	// N = number of dense columns of B/C. Sparsity = nnz / Dim^2.
	Dim      int32
	N        int32
	Sparsity float64

	nItems int32
	matrix csr.Matrix
	denseB []float32
	out    []float32

	dVal    driver.GPUPtr
	dCols   driver.GPUPtr
	dRowD   driver.GPUPtr
	dDenseB driver.GPUPtr
	dDenseC driver.GPUPtr
	dAlpha  driver.GPUPtr
	dBeta   driver.GPUPtr
}

// NewBenchmark creates a new SpMM benchmark.
func NewBenchmark(gpuDriver *driver.Driver) *Benchmark {
	b := new(Benchmark)
	b.driver = gpuDriver
	b.context = b.driver.Init()
	b.loadProgram()
	return b
}

// SelectGPU selects the GPUs to run on.
func (b *Benchmark) SelectGPU(gpus []int) { b.gpus = gpus }

// SetUnifiedMemory switches allocation to unified memory.
func (b *Benchmark) SetUnifiedMemory() { b.useUnifiedMemory = true }

// SetLASPMemoryAlloc switches allocation to the LASP allocator.
func (b *Benchmark) SetLASPMemoryAlloc() { b.useLASPMemoryAlloc = true }

func (b *Benchmark) loadProgram() {
	b.kernel = kernels.LoadProgramFromMemory(hsacoBytes, "csrmv_batched")
	if b.kernel == nil {
		log.Panic("Failed to load kernel binary")
	}
}

// Run runs the benchmark.
func (b *Benchmark) Run() {
	for _, gpu := range b.gpus {
		b.driver.SelectGPU(b.context, gpu)
		b.queues = append(b.queues, b.driver.CreateCommandQueue(b.context))
	}
	b.initMem()
	b.exec()
}

func (b *Benchmark) initMem() {
	b.nItems = int32(float64(b.Dim) * float64(b.Dim) * b.Sparsity)
	log.Printf("SpMM: Dim=%d N=%d nnz=%d\n", b.Dim, b.N, b.nItems)

	// A: square sparse matrix (Dim x Dim), Zipf column skew via -zipf-s so hub
	// columns -> hot, cross-chiplet-shared rows of B (the gathered operand).
	b.matrix = csr.
		MakeMatrixGenerator(uint32(b.Dim), uint32(b.nItems)).
		GenerateMatrix()

	// B: dense (Dim x N), row-major.
	b.denseB = make([]float32, int64(b.Dim)*int64(b.N))
	for i := range b.denseB {
		b.denseB[i] = rand.Float32()
	}
	b.out = make([]float32, int64(b.Dim)*int64(b.N))

	alpha := []float32{1}
	beta := []float32{0}

	valBytes := uint64(b.nItems) * 4
	colBytes := uint64(b.nItems) * 4
	rowBytes := uint64(b.Dim+1) * 4
	bBytes := uint64(b.Dim) * uint64(b.N) * 4
	cBytes := bBytes

	alloc := func(n uint64) driver.GPUPtr {
		switch {
		case b.useUnifiedMemory:
			return b.driver.AllocateUnifiedMemory(b.context, n)
		case b.useLASPMemoryAlloc:
			return b.driver.AllocateMemoryLASP(b.context, n, "div4")
		default:
			return b.driver.AllocateMemory(b.context, n)
		}
	}

	b.dVal = alloc(valBytes)
	b.dCols = alloc(colBytes)
	b.dRowD = alloc(rowBytes)
	b.dDenseB = alloc(bBytes)
	b.dDenseC = alloc(cBytes)
	b.dAlpha = alloc(4)
	b.dBeta = alloc(4)

	b.driver.MemCopyH2D(b.context, b.dVal, b.matrix.Values)
	b.driver.MemCopyH2D(b.context, b.dCols, b.matrix.ColumnNumbers)
	b.driver.MemCopyH2D(b.context, b.dRowD, b.matrix.RowOffsets)
	b.driver.MemCopyH2D(b.context, b.dDenseB, b.denseB)
	b.driver.MemCopyH2D(b.context, b.dDenseC, b.out)
	b.driver.MemCopyH2D(b.context, b.dAlpha, alpha)
	b.driver.MemCopyH2D(b.context, b.dBeta, beta)
}

func (b *Benchmark) exec() {
	args := KernelArgs{
		NumRows:   b.Dim,
		Alpha:     b.dAlpha,
		RowOffset: b.dRowD,
		Col:       b.dCols,
		Val:       b.dVal,
		DenseB:    b.dDenseB,
		LdB:       b.N,
		Beta:      b.dBeta,
		DenseC:    b.dDenseC,
		NumRowsC:  b.Dim,
		NumColsC:  b.N,
		LdC:       b.N,
	}

	// One subwave per row; the kernel loops over the N dense columns internally.
	globalX := roundUp(uint32(b.Dim)*subwaveSize, wgSize)
	globalSize := [3]uint32{globalX, 1, 1}
	localSize := [3]uint16{wgSize, 1, 1}

	b.driver.LaunchKernel(b.context, b.kernel, globalSize, localSize, &args)

	b.driver.MemCopyD2H(b.context, b.out, b.dDenseC)
}

// Verify checks the GPU result against a CPU SpMM.
func (b *Benchmark) Verify() {
	n := int(b.N)
	cpu := make([]float32, int64(b.Dim)*int64(b.N))
	for i := int32(0); i < b.Dim; i++ {
		start := b.matrix.RowOffsets[i]
		end := b.matrix.RowOffsets[i+1]
		for c := 0; c < n; c++ {
			t := float32(0)
			for j := start; j < end; j++ {
				col := b.matrix.ColumnNumbers[j]
				t += b.matrix.Values[j] * b.denseB[int(col)*n+c]
			}
			cpu[int(i)*n+c] = t
		}
	}

	mismatch := false
	for idx := range cpu {
		if b.out[idx] != cpu[idx] {
			mismatch = true
			log.Printf("mismatch at %d: expected %f, got %f\n",
				idx, cpu[idx], b.out[idx])
			break
		}
	}
	if mismatch {
		panic("Mismatch!")
	}
	log.Printf("Passed!\n")
}

func roundUp(v, mult uint32) uint32 {
	if v%mult == 0 {
		return v
	}
	return (v/mult + 1) * mult
}
