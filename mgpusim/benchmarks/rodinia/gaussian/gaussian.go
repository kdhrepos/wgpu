// Package gaussian implements the Gaussian benchmark.
package gaussian

import (
	"math"

	_ "embed"

	"gitlab.com/akita/mgpusim/driver"
	"gitlab.com/akita/mgpusim/insts"
	"gitlab.com/akita/mgpusim/kernels"
)

// KernelArgs defines kernel arguments
type Kernel1Args struct {
	MDev                driver.GPUPtr
	ADev                driver.GPUPtr
	Size                int32
	T                   int32
	HiddenGlobalOffsetX int64
	HiddenGlobalOffsetY int64
	HiddenGlobalOffsetZ int64
}

type Kernel2Args struct {
	MDev                driver.GPUPtr
	ADev                driver.GPUPtr
	BDev                driver.GPUPtr
	Size                int32
	T                   int32
	HiddenGlobalOffsetX int64
	HiddenGlobalOffsetY int64
	HiddenGlobalOffsetZ int64
}

// Benchmark defines a benchmark
type Benchmark struct {
	driver  *driver.Driver
	context *driver.Context
	queue   *driver.CommandQueue
	gpus    []int
	kernel1 *insts.HsaCo
	kernel2 *insts.HsaCo

	Size int

	mHost []float32
	aHost []float32
	bHost []float32

	mDev driver.GPUPtr
	aDev driver.GPUPtr
	bDev driver.GPUPtr

	useUnifiedMemory   bool
	useLASPMemoryAlloc bool
}

//go:embed kernels.hsaco
var hsacoBytes []byte

// NewBenchmark returns a benchmark
func NewBenchmark(driver *driver.Driver) *Benchmark {
	b := new(Benchmark)

	b.driver = driver
	b.context = b.driver.Init()
	b.queue = driver.CreateCommandQueue(b.context)

	b.kernel1 = kernels.LoadProgramFromMemory(hsacoBytes, "Fan1")
	b.kernel2 = kernels.LoadProgramFromMemory(hsacoBytes, "Fan2")

	return b
}

// SelectGPU select GPU
func (b *Benchmark) SelectGPU(gpus []int) {
	b.gpus = gpus
}

// SetUnifiedMemory uses Unified Memory
func (b *Benchmark) SetUnifiedMemory() {
	b.useUnifiedMemory = true
}

func (b *Benchmark) SetLASPMemoryAlloc() {
	b.useLASPMemoryAlloc = true
}

func (b *Benchmark) CreateMatrix(size int) []float32 {
	coe := make([]float32, 2*size-1)
	lamda := float32(-0.01)

	for i := 0; i < size; i++ {
		coe_i := float32(10) * float32(math.Exp(float64(lamda*float32(i))))
		coe[size-1+i] = coe_i
		coe[size-1-i] = coe_i
	}

	m := make([]float32, size*size)
	for i := 0; i < size; i++ {
		for j := 0; j < size; j++ {
			m[i*size+j] = coe[size-1-i+j]
		}
	}
	return m
}

// Run runs
func (b *Benchmark) Run() {
	b.driver.SelectGPU(b.context, b.gpus[0])
	b.initMem()
	b.exec()
}

func (b *Benchmark) initMem() {
	b.mHost = make([]float32, b.Size*b.Size)
	b.aHost = b.CreateMatrix(b.Size)
	b.bHost = make([]float32, b.Size*b.Size)

	for i := 0; i < b.Size*b.Size; i++ {
		b.bHost[i] = float32(i)
	}

	for i := 0; i < b.Size*b.Size; i++ {
		b.mHost[i] = float32(0)
	}

	if b.useUnifiedMemory {
		b.mDev = b.driver.AllocateUnifiedMemory(
			b.context, uint64(b.Size*b.Size*4))
		b.aDev = b.driver.AllocateUnifiedMemory(
			b.context, uint64(b.Size*b.Size*4))
		b.bDev = b.driver.AllocateUnifiedMemory(
			b.context, uint64(b.Size*b.Size*4))
	} else if b.useLASPMemoryAlloc {
		b.mDev = b.driver.AllocateMemoryLASP(
			b.context, uint64(b.Size*b.Size*4), "div4")
		b.aDev = b.driver.AllocateMemoryLASP(
			b.context, uint64(b.Size*b.Size*4), "div4")
		b.bDev = b.driver.AllocateMemoryLASP(
			b.context, uint64(b.Size*b.Size*4), "div4")
	} else {
		b.mDev = b.driver.AllocateMemory(
			b.context, uint64(b.Size*b.Size*4))
		b.aDev = b.driver.AllocateMemory(
			b.context, uint64(b.Size*b.Size*4))
		b.bDev = b.driver.AllocateMemory(
			b.context, uint64(b.Size*b.Size*4))
	}

	b.driver.MemCopyH2D(b.context, b.aDev, b.aHost)
	b.driver.MemCopyH2D(b.context, b.bDev, b.bHost)
}
func (b *Benchmark) exec() {
	globalWorkSizeKern1 := [3]uint32{uint32(b.Size), 1, 1}
	globalWorkSizeKern2 := [3]uint32{uint32(b.Size), uint32(b.Size), 1}
	localWorkSizeKern1 := [3]uint16{16, 1, 1}
	localWorkSizeKern2 := [3]uint16{16, 16, 1}

	globalWorkSizeKern1[0] = uint32(math.Ceil(
		float64(globalWorkSizeKern1[0])/
			float64(uint32(localWorkSizeKern1[0])))) *
		uint32(localWorkSizeKern1[0])
	globalWorkSizeKern2[0] = uint32(math.Ceil(
		float64(globalWorkSizeKern2[0])/
			float64(uint32(localWorkSizeKern2[0])))) *
		uint32(localWorkSizeKern2[0])
	globalWorkSizeKern2[1] = uint32(math.Ceil(
		float64(globalWorkSizeKern2[1])/
			float64(uint32(localWorkSizeKern2[1])))) *
		uint32(localWorkSizeKern2[1])

	for t := 0; t < b.Size-1; t++ {
		kern1Arg := Kernel1Args{
			b.mDev,
			b.aDev,
			int32(b.Size),
			int32(t),
			0, 0, 0,
		}

		kern2Arg := Kernel2Args{
			b.mDev,
			b.aDev,
			b.bDev,
			int32(b.Size),
			int32(t),
			0, 0, 0,
		}

		b.driver.LaunchKernel(
			b.context,
			b.kernel1,
			globalWorkSizeKern1,
			localWorkSizeKern1,
			&kern1Arg,
		)

		b.driver.LaunchKernel(
			b.context,
			b.kernel2,
			globalWorkSizeKern2,
			localWorkSizeKern2,
			&kern2Arg,
		)
	}

	b.driver.MemCopyD2H(b.context, b.mHost, b.mDev)
	b.driver.MemCopyD2H(b.context, b.bHost, b.bDev)
	b.driver.MemCopyD2H(b.context, b.aHost, b.aDev)
}

// Verify verifies
func (b *Benchmark) Verify() {
}
