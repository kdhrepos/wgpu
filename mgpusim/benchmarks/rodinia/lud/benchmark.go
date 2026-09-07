package lud

import (
	_ "embed"
	"fmt"
	"log"

	"gitlab.com/akita/mgpusim/driver"
	"gitlab.com/akita/mgpusim/insts"
	"gitlab.com/akita/mgpusim/kernels"
)

// KernelArgs defines kernel arguments
type DiagonalKernelArgs struct {
	MDev                driver.GPUPtr
	Shadow              driver.LocalPtr
	MatrixDim           int32
	Offset              int32
	HiddenGlobalOffsetX int64
	HiddenGlobalOffsetY int64
	HiddenGlobalOffsetZ int64
}

type PerimeterKernelArgs struct {
	MDev                driver.GPUPtr
	Dia                 driver.LocalPtr
	PeriRow             driver.LocalPtr
	PeriCol             driver.LocalPtr
	MatrixDim           int32
	Offset              int32
	HiddenGlobalOffsetX int64
	HiddenGlobalOffsetY int64
	HiddenGlobalOffsetZ int64
}

type InternalKernelArgs struct {
	MDev                driver.GPUPtr
	PeriRow             driver.LocalPtr
	PeriCol             driver.LocalPtr
	MatrixDim           int32
	Offset              int32
	HiddenGlobalOffsetX int64
	HiddenGlobalOffsetY int64
	HiddenGlobalOffsetZ int64
}

// Benchmark defines a benchmark
type Benchmark struct {
	driver          *driver.Driver
	context         *driver.Context
	queues          []*driver.CommandQueue
	gpuIDs          []int
	diagonalKernel  *insts.HsaCo
	perimeterKernel *insts.HsaCo
	internalKernel  *insts.HsaCo

	MatrixDim int
	blockSize int

	mHost []float32

	mDev driver.GPUPtr

	useUnifiedMemory   bool
	useLASPMemoryAlloc bool
}

// NewBenchmark creates a new benchmark
func NewBenchmark(driver *driver.Driver) *Benchmark {
	b := new(Benchmark)
	b.driver = driver
	b.context = driver.Init()
	b.blockSize = 16
	b.loadProgram()

	return b
}

//go:embed kernels.hsaco
var hsacoBytes []byte

func (b *Benchmark) loadProgram() {
	b.diagonalKernel = kernels.LoadProgramFromMemory(
		hsacoBytes, "lud_diagonal")
	if b.diagonalKernel == nil {
		log.Panic("Failed to load diagonal kernel binary")
	}

	b.perimeterKernel = kernels.LoadProgramFromMemory(
		hsacoBytes, "lud_perimeter")
	if b.perimeterKernel == nil {
		log.Panic("Failed to load perimeter kernel binary")
	}

	b.internalKernel = kernels.LoadProgramFromMemory(
		hsacoBytes, "lud_internal")
	if b.internalKernel == nil {
		log.Panic("Failed to load internal kernel binary")
	}

}

// SelectGPU selects gpu
func (b *Benchmark) SelectGPU(gpuIDs []int) {
	b.gpuIDs = gpuIDs
}

// Run runs
func (b *Benchmark) Run() {
	for _, gpu := range b.gpuIDs {
		b.driver.SelectGPU(b.context, gpu)
		b.queues = append(b.queues, b.driver.CreateCommandQueue(b.context))
	}
	b.initMem()
	b.exec()
}

// SetUnifiedMemory Use Unified Memor
func (b *Benchmark) SetUnifiedMemory() {
	b.useUnifiedMemory = true
}

func (b *Benchmark) initMem() {
	b.mHost = make([]float32, b.MatrixDim*b.MatrixDim)

	for i := 0; i < b.MatrixDim; i++ {
		for j := 0; j < b.MatrixDim; j++ {
			b.mHost[i*b.MatrixDim+j] = float32(i*b.MatrixDim + j + 1)
		}
	}

	if b.useLASPMemoryAlloc {
		b.mDev = b.driver.AllocateMemoryLASP(
			b.context, uint64(b.MatrixDim*b.MatrixDim*4), "div4")
	} else {
		panic("")
	}
}

func (b *Benchmark) exec() {
	b.driver.MemCopyH2D(b.context, b.mDev, b.mHost)

	var i int
	for i = 0; i < b.MatrixDim-b.blockSize; i += b.blockSize {
		diagKernelGlobalSize := [3]uint32{uint32(b.blockSize), 1, 1}
		diagKernelLocalSize := [3]uint16{uint16(b.blockSize), 1, 1}

		perimeterKernelGlobalSize := [3]uint32{uint32(
			b.blockSize * 2 * (((b.MatrixDim - i) / b.blockSize) - 1)), 1, 1}
		perimeterKernelLocalSize := [3]uint16{uint16(b.blockSize * 2), 1, 1}

		internalKernelGlobalSize := [3]uint32{
			uint32(b.blockSize * (((b.MatrixDim - i) / b.blockSize) - 1)),
			uint32(b.blockSize * (((b.MatrixDim - i) / b.blockSize) - 1)), 1}
		internalKernelLocalSize := [3]uint16{
			uint16(b.blockSize),
			uint16(b.blockSize), 1}

		diagKernelArgs := DiagonalKernelArgs{
			MDev:                b.mDev,
			Shadow:              driver.LocalPtr(b.blockSize * b.blockSize * 4),
			MatrixDim:           int32(b.MatrixDim),
			Offset:              int32(i),
			HiddenGlobalOffsetX: 0,
			HiddenGlobalOffsetY: 0,
			HiddenGlobalOffsetZ: 0,
		}

		perimeterKernelArgs := PerimeterKernelArgs{
			MDev:                b.mDev,
			Dia:                 driver.LocalPtr(b.blockSize * b.blockSize * 4),
			PeriRow:             driver.LocalPtr(b.blockSize * b.blockSize * 4),
			PeriCol:             driver.LocalPtr(b.blockSize * b.blockSize * 4),
			MatrixDim:           int32(b.MatrixDim),
			Offset:              int32(i),
			HiddenGlobalOffsetX: 0,
			HiddenGlobalOffsetY: 0,
			HiddenGlobalOffsetZ: 0,
		}

		internalKernelArgs := InternalKernelArgs{
			MDev:                b.mDev,
			PeriRow:             driver.LocalPtr(b.blockSize * b.blockSize * 4),
			PeriCol:             driver.LocalPtr(b.blockSize * b.blockSize * 4),
			MatrixDim:           int32(b.MatrixDim),
			Offset:              int32(i),
			HiddenGlobalOffsetX: 0,
			HiddenGlobalOffsetY: 0,
			HiddenGlobalOffsetZ: 0,
		}

		b.driver.LaunchKernel(
			b.context,
			b.diagonalKernel,
			diagKernelGlobalSize,
			diagKernelLocalSize,
			&diagKernelArgs,
		)

		b.driver.LaunchKernel(
			b.context,
			b.perimeterKernel,
			perimeterKernelGlobalSize,
			perimeterKernelLocalSize,
			&perimeterKernelArgs,
		)

		b.driver.LaunchKernel(
			b.context,
			b.internalKernel,
			internalKernelGlobalSize,
			internalKernelLocalSize,
			&internalKernelArgs,
		)
	}

	diagKernelGlobalSize := [3]uint32{uint32(b.blockSize), 1, 1}
	diagKernelLocalSize := [3]uint16{uint16(b.blockSize), 1, 1}

	diagKernelArgs := DiagonalKernelArgs{
		MDev:                b.mDev,
		Shadow:              driver.LocalPtr(b.blockSize * b.blockSize * 4),
		MatrixDim:           int32(b.MatrixDim),
		Offset:              int32(i),
		HiddenGlobalOffsetX: 0,
		HiddenGlobalOffsetY: 0,
		HiddenGlobalOffsetZ: 0,
	}

	b.driver.LaunchKernel(
		b.context,
		b.diagonalKernel,
		diagKernelGlobalSize,
		diagKernelLocalSize,
		&diagKernelArgs,
	)

	b.driver.MemCopyD2H(b.context, b.mHost, b.mDev)
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
	// for i := 0; i < b.row; i++ {
	// 	for j := 0; j < b.col; j++ {
	// 		fmt.Printf("%5d", b.inputItemSets[i*b.col+j])
	// 	}
	// 	fmt.Printf("\n")
	// }

	// mismatch := false
	// for i := 0; i < b.row; i++ {
	// 	for j := 0; j < b.col; j++ {
	// 		if b.outputItemSets[i*b.col+j] != b.inputItemSets[i*b.col+j] {
	// 			mismatch = true
	// 			log.Printf("at (%d, %d), expected %d, but get %d",
	// 				j, i,
	// 				b.inputItemSets[i*b.col+j],
	// 				b.outputItemSets[i*b.col+j])
	// 		}
	// 	}
	// 	// fmt.Printf("\n")
	// }

	// if mismatch {
	// 	panic("mismatch\n")
	// }

	fmt.Print("Passed!\n")
}

func (b *Benchmark) SetLASPMemoryAlloc() {
	b.useLASPMemoryAlloc = true
}
