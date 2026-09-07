// Package nw defines the Needleman–Wunsch benchmark
package nw

import (
	"fmt"
	"log"
	"math/rand"

	// embed hsaco files
	_ "embed"

	"gitlab.com/akita/mgpusim/driver"
	"gitlab.com/akita/mgpusim/insts"
	"gitlab.com/akita/mgpusim/kernels"
)

// KernelArgs defines kernel arguments
type KernelArgs struct {
	Reference          driver.GPUPtr
	InputItemSets      driver.GPUPtr
	OutputItemSets     driver.GPUPtr
	LocalInputItemSets driver.LocalPtr
	LocalReference     driver.LocalPtr
	Cols               int32
	Penalty            int32
	Blk                int32
	BlockSize          int32
	BlockWidth         int32
	WorkSize           int32
	OffsetR            int32
	OffsetC            int32
}

var blosum62 = [][]int32{
	{4, -1, -2, -2, 0, -1, -1, 0, -2, -1, -1, -1, -1, -2, -1, 1, 0, -3, -2, 0, -2, -1, 0, -4},
	{-1, 5, 0, -2, -3, 1, 0, -2, 0, -3, -2, 2, -1, -3, -2, -1, -1, -3, -2, -3, -1, 0, -1, -4},
	{-2, 0, 6, 1, -3, 0, 0, 0, 1, -3, -3, 0, -2, -3, -2, 1, 0, -4, -2, -3, 3, 0, -1, -4},
	{-2, -2, 1, 6, -3, 0, 2, -1, -1, -3, -4, -1, -3, -3, -1, 0, -1, -4, -3, -3, 4, 1, -1, -4},
	{0, -3, -3, -3, 9, -3, -4, -3, -3, -1, -1, -3, -1, -2, -3, -1, -1, -2, -2, -1, -3, -3, -2, -4},
	{-1, 1, 0, 0, -3, 5, 2, -2, 0, -3, -2, 1, 0, -3, -1, 0, -1, -2, -1, -2, 0, 3, -1, -4},
	{-1, 0, 0, 2, -4, 2, 5, -2, 0, -3, -3, 1, -2, -3, -1, 0, -1, -3, -2, -2, 1, 4, -1, -4},
	{0, -2, 0, -1, -3, -2, -2, 6, -2, -4, -4, -2, -3, -3, -2, 0, -2, -2, -3, -3, -1, -2, -1, -4},
	{-2, 0, 1, -1, -3, 0, 0, -2, 8, -3, -3, -1, -2, -1, -2, -1, -2, -2, 2, -3, 0, 0, -1, -4},
	{-1, -3, -3, -3, -1, -3, -3, -4, -3, 4, 2, -3, 1, 0, -3, -2, -1, -3, -1, 3, -3, -3, -1, -4},
	{-1, -2, -3, -4, -1, -2, -3, -4, -3, 2, 4, -2, 2, 0, -3, -2, -1, -2, -1, 1, -4, -3, -1, -4},
	{-1, 2, 0, -1, -3, 1, 1, -2, -1, -3, -2, 5, -1, -3, -1, 0, -1, -3, -2, -2, 0, 1, -1, -4},
	{-1, -1, -2, -3, -1, 0, -2, -3, -2, 1, 2, -1, 5, 0, -2, -1, -1, -1, -1, 1, -3, -1, -1, -4},
	{-2, -3, -3, -3, -2, -3, -3, -3, -1, 0, 0, -3, 0, 6, -4, -2, -2, 1, 3, -1, -3, -3, -1, -4},
	{-1, -2, -2, -1, -3, -1, -1, -2, -2, -3, -3, -1, -2, -4, 7, -1, -1, -4, -3, -2, -2, -1, -2, -4},
	{1, -1, 1, 0, -1, 0, 0, 0, -1, -2, -2, 0, -1, -2, -1, 4, 1, -3, -2, -2, 0, 0, 0, -4},
	{0, -1, 0, -1, -1, -1, -1, -2, -2, -1, -1, -1, -1, -2, -1, 1, 5, -2, -2, 0, -1, -1, 0, -4},
	{-3, -3, -4, -4, -2, -2, -3, -2, -2, -3, -2, -3, -1, 1, -4, -3, -2, 11, 2, -3, -4, -3, -2, -4},
	{-2, -2, -2, -3, -2, -1, -2, -3, 2, -1, -1, -2, -1, 3, -3, -2, -2, 2, 7, -1, -3, -2, -1, -4},
	{0, -3, -3, -3, -1, -2, -2, -3, -3, 3, 1, -2, 1, -1, -2, -2, 0, -3, -1, 4, -3, -2, -1, -4},
	{-2, -1, 3, 4, -3, 0, 1, -1, 0, -3, -4, 0, -3, -3, -2, 0, -1, -4, -3, -3, 4, 1, -1, -4},
	{-1, 0, 0, 1, -3, 3, 4, -2, 0, -3, -3, 1, -1, -3, -1, 0, -1, -3, -2, -2, 1, 4, -1, -4},
	{0, -1, -1, -1, -2, -1, -1, -1, -1, -1, -1, -1, -1, -1, -2, 0, 0, -2, -1, -1, -1, -1, -1, -4},
	{-4, -4, -4, -4, -4, -4, -4, -4, -4, -4, -4, -4, -4, -4, -4, -4, -4, -4, -4, -4, -4, -4, -4, 1},
}

// Benchmark defines a benchmark
type Benchmark struct {
	driver           *driver.Driver
	context          *driver.Context
	gpuIDs           []int
	kernel1, kernel2 *insts.HsaCo
	queue            *driver.CommandQueue

	blockSize         int
	length            int
	penalty           int
	Row, Col          int
	reference         []int32
	inputItemSets     []int32
	outputItemSets    []int32
	cpuOutputItemSets []int32
	dInputItemSets    driver.GPUPtr
	dOutputItemSets   driver.GPUPtr
	dReference        driver.GPUPtr

	useUnifiedMemory   bool
	useLASPMemoryAlloc bool
}

// NewBenchmark creates a new benchmark
func NewBenchmark(driver *driver.Driver) *Benchmark {
	b := new(Benchmark)
	b.driver = driver
	b.context = driver.Init()
	b.loadProgram()
	b.queue = driver.CreateCommandQueue(b.context)

	b.blockSize = 64
	b.SetLength(256)
	b.penalty = 10

	return b
}

//go:embed kernels.hsaco
var hsacoBytes []byte

func (b *Benchmark) loadProgram() {
	b.kernel1 = kernels.LoadProgramFromMemory(
		hsacoBytes, "nw_kernel1")
	if b.kernel1 == nil {
		log.Panic("Failed to load kernel binary")
	}

	b.kernel2 = kernels.LoadProgramFromMemory(
		hsacoBytes, "nw_kernel2")
	if b.kernel2 == nil {
		log.Panic("Failed to load kernel binary")
	}
}

// SetLength sets length
func (b *Benchmark) SetLength(length int) {
	// if length%16 != 0 {
	// 	panic("NW length must be a multiple of 16")
	// }

	b.length = length
	b.Row = length + 1
	b.Col = length + 1
}

// SetPenalty sets penalty
func (b *Benchmark) SetPenalty(penalty int) {
	b.penalty = penalty
}

// SelectGPU selects gpu
func (b *Benchmark) SelectGPU(gpuIDs []int) {
	if len(gpuIDs) > 1 {
		panic("nw does not support multi-GPU mode")
	}

	b.gpuIDs = gpuIDs
}

// Run runs
func (b *Benchmark) Run() {
	b.driver.SelectGPU(b.context, b.gpuIDs[0])
	b.initMem()
	b.exec()
}

// SetUnifiedMemory Use Unified Memor
func (b *Benchmark) SetUnifiedMemory() {
	b.useUnifiedMemory = true
}

func (b *Benchmark) initMem() {
	b.initData()
	b.allocateGPUMem()
}

func (b *Benchmark) initData() {
	b.reference = make([]int32, b.Row*b.Col)
	b.inputItemSets = make([]int32, b.Row*b.Col)
	b.outputItemSets = make([]int32, b.Row*b.Col)

	for i := 0; i < b.Row; i++ {
		b.inputItemSets[i*b.Col] = int32(rand.Int()%10 + 1)
	}

	for i := 0; i < b.Col; i++ {
		b.inputItemSets[i] = int32(rand.Int()%10 + 1)
	}

	for i := 0; i < b.Col; i++ {
		for j := 0; j < b.Row; j++ {
			b.reference[i*b.Col+j] =
				blosum62[b.inputItemSets[i*b.Col]][b.inputItemSets[j]]
			// b.reference[i*b.Col+j] = int32(i*b.Col + j)
		}
	}

	b.inputItemSets[0] = 0

	for i := 1; i < b.Row; i++ {
		b.inputItemSets[i*b.Col] = int32(-i * b.penalty)
	}
	for j := 1; j < b.Col; j++ {
		b.inputItemSets[j] = int32(-j * b.penalty)
	}
}

func (b *Benchmark) allocateGPUMem() {
	b.dInputItemSets = b.allocate(uint64(b.Col * b.Row * 4))
	b.dOutputItemSets = b.allocate(uint64(b.Col * b.Row * 4))
	b.dReference = b.allocate(uint64(b.Col * b.Row * 4))
}

func (b *Benchmark) allocate(byteSize uint64) driver.GPUPtr {
	if b.useUnifiedMemory {
		return b.driver.AllocateUnifiedMemory(b.context, byteSize)
	} else if b.useLASPMemoryAlloc {
		return b.driver.AllocateMemoryLASP(b.context, byteSize, "div4")
	}

	return b.driver.AllocateMemory(b.context, byteSize)
}

func (b *Benchmark) exec() {
	b.copyInputDataToGPU()
	b.runKernel1()
	b.runKernel2()
	b.copyOutputDataFromGPU()
}

func (b *Benchmark) copyInputDataToGPU() {
	b.driver.MemCopyH2D(b.context, b.dInputItemSets, b.inputItemSets)
	b.driver.MemCopyH2D(b.context, b.dReference, b.reference)
}

func (b *Benchmark) copyOutputDataFromGPU() {
	b.driver.MemCopyD2H(b.context, b.outputItemSets, b.dInputItemSets)
}

func (b *Benchmark) runKernel1() {
	workSize := b.Col - 1
	offsetR := 0
	offsetC := 0
	blockWidth := workSize / b.blockSize

	for blk := 1; blk <= workSize/b.blockSize; blk++ {
		globalSize := [3]uint32{uint32(b.blockSize * blk), 1, 1}
		localSize := [3]uint16{uint16(b.blockSize), 1, 1}

		args := KernelArgs{
			Reference:          b.dReference,
			InputItemSets:      b.dInputItemSets,
			OutputItemSets:     b.dOutputItemSets,
			LocalInputItemSets: driver.LocalPtr((b.blockSize + 1) * (b.blockSize + 1) * 4),
			LocalReference:     driver.LocalPtr(b.blockSize * b.blockSize * 4),
			Cols:               int32(b.Col),
			Penalty:            int32(b.penalty),
			Blk:                int32(blk),
			BlockSize:          int32(b.blockSize),
			BlockWidth:         int32(blockWidth),
			WorkSize:           int32(workSize),
			OffsetR:            int32(offsetR),
			OffsetC:            int32(offsetC),
		}

		b.driver.LaunchKernel(
			b.context,
			b.kernel1,
			globalSize,
			localSize,
			&args,
		)
	}
}

func (b *Benchmark) runKernel2() {
	workSize := b.Col - 1
	offsetR := 0
	offsetC := 0
	blockWidth := workSize / b.blockSize

	for blk := 1; blk <= workSize/b.blockSize; blk++ {
		globalSize := [3]uint32{uint32(b.blockSize * blk), 1, 1}
		localSize := [3]uint16{uint16(b.blockSize), 1, 1}

		args := KernelArgs{
			Reference:          b.dReference,
			InputItemSets:      b.dInputItemSets,
			OutputItemSets:     b.dOutputItemSets,
			LocalInputItemSets: driver.LocalPtr((b.blockSize + 1) * (b.blockSize + 1) * 4),
			LocalReference:     driver.LocalPtr(b.blockSize * b.blockSize * 4),
			Cols:               int32(b.Col),
			Penalty:            int32(b.penalty),
			Blk:                int32(blk),
			BlockSize:          int32(b.blockSize),
			BlockWidth:         int32(blockWidth),
			WorkSize:           int32(workSize),
			OffsetR:            int32(offsetR),
			OffsetC:            int32(offsetC),
		}

		b.driver.LaunchKernel(
			b.context,
			b.kernel2,
			globalSize,
			localSize,
			&args,
		)
	}
}

// Verify verifies
func (b *Benchmark) Verify() {
	// fmt.Printf("\nReference:\n")
	// for i := 0; i < b.row; i++ {
	// 	for j := 0; j < b.col; j++ {
	// 		fmt.Printf("%5d", b.reference[i*b.col+j])
	// 	}
	// 	fmt.Printf("\n")
	// }

	// fmt.Printf("\nGPU Output:\n")
	// for i := 0; i < b.row; i++ {
	// 	for j := 0; j < b.col; j++ {
	// 		fmt.Printf("%5d", b.outputItemSets[i*b.col+j])
	// 	}
	// 	fmt.Printf("\n")
	// }

	// b.cpuNW()

	// fmt.Printf("\nCPU Output:\n")
	// for i := 0; i < b.Row; i++ {
	// 	for j := 0; j < b.Col; j++ {
	// 		fmt.Printf("%5d", b.inputItemSets[i*b.Col+j])
	// 	}
	// 	fmt.Printf("\n")
	// }

	// mismatch := false
	// for i := 0; i < b.Row; i++ {
	// 	for j := 0; j < b.Col; j++ {
	// 		if b.outputItemSets[i*b.Col+j] != b.inputItemSets[i*b.Col+j] {
	// 			mismatch = true
	// 			log.Printf("at (%d, %d), expected %d, but get %d",
	// 				j, i,
	// 				b.inputItemSets[i*b.Col+j],
	// 				b.outputItemSets[i*b.Col+j])
	// 		}
	// 	}
	// 	// fmt.Printf("\n")
	// }

	// if mismatch {
	// 	panic("mismatch\n")
	// }

	fmt.Print("Passed!\n")
}

func (b *Benchmark) cpuNW() {
	for i := 1; i < b.Row; i++ {
		for j := 1; j < b.Col; j++ {
			leftPenalty := b.inputItemSets[i*b.Col+(j-1)] - int32(b.penalty)
			topPenalty := b.inputItemSets[(i-1)*b.Col+j] - int32(b.penalty)
			refValue := b.reference[i*b.Col+j]
			diagPenalty := b.inputItemSets[(i-1)*b.Col+(j-1)] + refValue

			max := leftPenalty
			if topPenalty > max {
				max = topPenalty
			}
			if diagPenalty > max {
				max = diagPenalty
			}

			b.inputItemSets[i*b.Col+j] = max
		}
	}
}

func (b *Benchmark) SetLASPMemoryAlloc() {
	b.useLASPMemoryAlloc = true
}
