// Package raytracing implements a simple OpenCL ray tracer benchmark.
//
// The OpenCL kernel is adapted from https://github.com/alexgeek/gpu-raytracer
// (kernels/trace.cl). The scene (10 primitives: 2 planes + 8 spheres) is
// generated entirely inside the kernel, so there is no input buffer; the only
// device buffer is the output framebuffer (4 floats per pixel). This makes the
// workload compute-bound with a small, streaming write footprint.
package raytracing

import (
	_ "embed"
	"log"

	"gitlab.com/akita/mgpusim/driver"
	"gitlab.com/akita/mgpusim/insts"
	"gitlab.com/akita/mgpusim/kernels"
)

//go:embed kernels.hsaco
var hsacoBytes []byte

// KernelArgs is the argument layout for pixel_kernel. The trailing three
// HiddenGlobalOffset fields are the HSA hidden global-offset arguments.
type KernelArgs struct {
	Framebuffer         driver.GPUPtr
	Width               uint32
	Height              uint32
	Time                float32
	Padding             uint32
	HiddenGlobalOffsetX int64
	HiddenGlobalOffsetY int64
	HiddenGlobalOffsetZ int64
}

// Benchmark is the ray tracing benchmark.
type Benchmark struct {
	driver  *driver.Driver
	context *driver.Context
	hsaco   *insts.HsaCo
	gpus    []int

	Width  int
	Height int
	Time   float32

	gFramebuffer driver.GPUPtr

	useUnifiedMemory   bool
	useLASPMemoryAlloc bool
}

// NewBenchmark creates a new ray tracing benchmark.
func NewBenchmark(driver *driver.Driver) *Benchmark {
	b := new(Benchmark)
	b.driver = driver
	b.context = b.driver.Init()
	b.hsaco = kernels.LoadProgramFromMemory(hsacoBytes, "pixel_kernel")
	b.Width = 256
	b.Height = 256
	return b
}

// SelectGPU selects the GPUs to run on.
func (b *Benchmark) SelectGPU(gpus []int) { b.gpus = gpus }

// SetUnifiedMemory uses unified memory.
func (b *Benchmark) SetUnifiedMemory() { b.useUnifiedMemory = true }

// SetLASPMemoryAlloc uses the LASP memory allocator.
func (b *Benchmark) SetLASPMemoryAlloc() { b.useLASPMemoryAlloc = true }

// Run runs the benchmark.
func (b *Benchmark) Run() {
	b.driver.SelectGPU(b.context, b.gpus[0])
	b.initMem()
	b.exec()
}

func (b *Benchmark) initMem() {
	size := uint64(b.Width * b.Height * 4 * 4)

	if b.useUnifiedMemory {
		b.gFramebuffer = b.driver.AllocateUnifiedMemory(b.context, size)
	} else if b.useLASPMemoryAlloc {
		b.gFramebuffer = b.driver.AllocateMemoryLASP(b.context, size, "div4")
	} else {
		b.gFramebuffer = b.driver.AllocateMemory(b.context, size)
		b.driver.Distribute(b.context, b.gFramebuffer, size, b.gpus)
	}
}

func (b *Benchmark) exec() {
	queues := make([]*driver.CommandQueue, len(b.gpus))
	rowsPerGPU := b.Height / len(b.gpus)

	for i, gpu := range b.gpus {
		b.driver.SelectGPU(b.context, gpu)
		queues[i] = b.driver.CreateCommandQueue(b.context)

		kernArg := KernelArgs{
			b.gFramebuffer,
			uint32(b.Width),
			uint32(b.Height),
			b.Time,
			0,
			0, int64(i * rowsPerGPU), 0,
		}

		b.driver.EnqueueLaunchKernel(
			queues[i],
			b.hsaco,
			[3]uint32{uint32(b.Width), uint32(rowsPerGPU), 1},
			[3]uint16{16, 16, 1},
			&kernArg,
		)
	}

	for i := range b.gpus {
		b.driver.DrainCommandQueue(queues[i])
	}
}

// Verify performs a sanity check on the framebuffer: every channel value must
// lie in the clamped [0,1] range and the image must not be entirely black.
// This is a range/non-empty check, not a bit-exact comparison, because the
// floating-point trace is not reproduced on the host.
func (b *Benchmark) Verify() {
	out := make([]float32, b.Width*b.Height*4)
	b.driver.MemCopyD2H(b.context, out, b.gFramebuffer)

	nonzero := 0
	for i, v := range out {
		if v < 0 || v > 1.0001 {
			log.Fatalf("pixel value %f at index %d is outside [0,1]\n", v, i)
		}
		if v > 0 {
			nonzero++
		}
	}
	if nonzero == 0 {
		log.Fatalf("framebuffer is entirely zero — kernel produced no output\n")
	}

	log.Printf("Passed!\n")
}
