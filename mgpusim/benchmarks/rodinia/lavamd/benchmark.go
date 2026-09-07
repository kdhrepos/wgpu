// Package lavamd implements rodinia/opencl/lavaMD — short-range N-body force
// computation over a 3D grid of "boxes" of particles. Each home box only
// interacts with its (up to 26) face/edge/corner neighbors, so the working
// set is sparse but heavily reused via LDS.
//
// Differences from upstream:
//   * box_str / nei_str are flattened to three flat global buffers
//     (offsets, nn, nei_numbers) — kernel uses NEI_STRIDE = 32 entries per
//     box (only the first nn are valid). Avoids the non-power-of-two
//     struct strides that confuse MGPUSim's emu address resolver.
//   * FP32 only (FP64 divide opcodes unimplemented in emu).
package lavamd

import (
	_ "embed"
	"log"
	"math"

	"gitlab.com/akita/mgpusim/driver"
	"gitlab.com/akita/mgpusim/insts"
	"gitlab.com/akita/mgpusim/kernels"
)

//go:embed kernel.hsaco
var hsacoBytes []byte

const (
	numberParPerBox = 100
	numberThreads   = 128
	maxNei          = 26
	neiStride       = 32 // power of two, per-box stride for neiNumbers
)

// FourVector mirrors the kernel struct (16 B, power of two stride).
type FourVector struct {
	V, X, Y, Z float32
}

// KernelArgs is the kernarg layout for kernel_gpu_opencl. Field order matches
// the OpenCL kernarg ABI: scalar+int pair (8 B aligned), then six 8-byte
// pointers, then the HSA hidden global-offset trio.
type KernelArgs struct {
	Alpha               float32
	NumberBoxes         int32
	Offsets             driver.GPUPtr
	NN                  driver.GPUPtr
	NeiNumbers          driver.GPUPtr
	Rv                  driver.GPUPtr
	Qv                  driver.GPUPtr
	Fv                  driver.GPUPtr
	HiddenGlobalOffsetX int64
	HiddenGlobalOffsetY int64
	HiddenGlobalOffsetZ int64
}

// Benchmark is the lavaMD benchmark.
type Benchmark struct {
	driver  *driver.Driver
	context *driver.Context
	hsaco   *insts.HsaCo
	gpus    []int

	// Tunable inputs.
	Boxes1D int   // boxes per side; total boxes = Boxes1D^3
	Alpha   float32

	// Host data.
	numberBoxes int32
	offsets     []int64
	nn          []int32
	neiNumbers  []int32
	rv          []FourVector
	qv          []float32
	fv          []FourVector

	// Device pointers.
	gOffsets    driver.GPUPtr
	gNN         driver.GPUPtr
	gNeiNumbers driver.GPUPtr
	gRv         driver.GPUPtr
	gQv         driver.GPUPtr
	gFv         driver.GPUPtr

	useUnifiedMemory   bool
	useLASPMemoryAlloc bool
}

// NewBenchmark creates a new lavaMD benchmark.
func NewBenchmark(driver *driver.Driver) *Benchmark {
	b := new(Benchmark)
	b.driver = driver
	b.context = b.driver.Init()
	b.hsaco = kernels.LoadProgramFromMemory(hsacoBytes, "kernel_gpu_opencl")
	if b.hsaco == nil {
		log.Panic("failed to load lavamd kernel binary")
	}

	b.Boxes1D = 8
	b.Alpha = 0.5

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
	b1 := b.Boxes1D
	nBoxes := b1 * b1 * b1
	b.numberBoxes = int32(nBoxes)

	spaceElem := nBoxes * numberParPerBox

	b.offsets = make([]int64, nBoxes)
	b.nn = make([]int32, nBoxes)
	b.neiNumbers = make([]int32, nBoxes*neiStride)

	nh := 0
	for i := 0; i < b1; i++ {
		for j := 0; j < b1; j++ {
			for k := 0; k < b1; k++ {
				b.offsets[nh] = int64(nh * numberParPerBox)
				var cnt int32
				for l := -1; l < 2; l++ {
					for m := -1; m < 2; m++ {
						for n := -1; n < 2; n++ {
							if l == 0 && m == 0 && n == 0 {
								continue
							}
							ni, nj, nk := i+l, j+m, k+n
							if ni < 0 || nj < 0 || nk < 0 ||
								ni >= b1 || nj >= b1 || nk >= b1 {
								continue
							}
							neiNum := int32(ni*b1*b1 + nj*b1 + nk)
							b.neiNumbers[nh*neiStride+int(cnt)] = neiNum
							cnt++
						}
					}
				}
				b.nn[nh] = cnt
				nh++
			}
		}
	}

	// Deterministic LCG (avoids dragging math/rand into the build cache);
	// the actual values don't matter for the access pattern.
	var seed uint32 = 1
	nextF := func() float32 {
		seed = seed*1103515245 + 12345
		return float32((seed>>16)%10+1) / 10.0
	}

	b.rv = make([]FourVector, spaceElem)
	for i := range b.rv {
		b.rv[i].V = nextF()
		b.rv[i].X = nextF()
		b.rv[i].Y = nextF()
		b.rv[i].Z = nextF()
	}
	b.qv = make([]float32, spaceElem)
	for i := range b.qv {
		b.qv[i] = nextF()
	}
	b.fv = make([]FourVector, spaceElem)

	alloc := func(size uint64) driver.GPUPtr {
		if b.useUnifiedMemory {
			return b.driver.AllocateUnifiedMemory(b.context, size)
		}
		if b.useLASPMemoryAlloc {
			return b.driver.AllocateMemoryLASP(b.context, size, "div4")
		}
		p := b.driver.AllocateMemory(b.context, size)
		b.driver.Distribute(b.context, p, size, b.gpus)
		return p
	}

	b.gOffsets = alloc(uint64(len(b.offsets) * 8))
	b.gNN = alloc(uint64(len(b.nn) * 4))
	b.gNeiNumbers = alloc(uint64(len(b.neiNumbers) * 4))
	b.gRv = alloc(uint64(len(b.rv) * 16))
	b.gQv = alloc(uint64(len(b.qv) * 4))
	b.gFv = alloc(uint64(len(b.fv) * 16))

	b.driver.MemCopyH2D(b.context, b.gOffsets, b.offsets)
	b.driver.MemCopyH2D(b.context, b.gNN, b.nn)
	b.driver.MemCopyH2D(b.context, b.gNeiNumbers, b.neiNumbers)
	b.driver.MemCopyH2D(b.context, b.gRv, b.rv)
	b.driver.MemCopyH2D(b.context, b.gQv, b.qv)
	b.driver.MemCopyH2D(b.context, b.gFv, b.fv)
}

func (b *Benchmark) exec() {
	args := KernelArgs{
		Alpha:       b.Alpha,
		NumberBoxes: b.numberBoxes,
		Offsets:     b.gOffsets,
		NN:          b.gNN,
		NeiNumbers:  b.gNeiNumbers,
		Rv:          b.gRv,
		Qv:          b.gQv,
		Fv:          b.gFv,
	}

	globalSize := [3]uint32{uint32(int(b.numberBoxes) * numberThreads), 1, 1}
	localSize := [3]uint16{numberThreads, 1, 1}

	b.driver.LaunchKernel(b.context, b.hsaco, globalSize, localSize, &args)
}

// Verify runs the same force kernel on the CPU and compares against the GPU
// output. Tolerant of FP32 rounding (relative error 1e-3).
func (b *Benchmark) Verify() {
	gpuFv := make([]FourVector, len(b.fv))
	b.driver.MemCopyD2H(b.context, gpuFv, b.gFv)

	cpuFv := make([]FourVector, len(b.fv))
	a2 := 2 * b.Alpha * b.Alpha

	for bx := 0; bx < int(b.numberBoxes); bx++ {
		firstI := int(b.offsets[bx])
		nn := int(b.nn[bx])

		for k := 0; k < 1+nn; k++ {
			var pointer int
			if k == 0 {
				pointer = bx
			} else {
				pointer = int(b.neiNumbers[bx*neiStride+(k-1)])
			}
			firstJ := int(b.offsets[pointer])

			for wtx := 0; wtx < numberParPerBox; wtx++ {
				rA := b.rv[firstI+wtx]
				for j := 0; j < numberParPerBox; j++ {
					rB := b.rv[firstJ+j]
					qB := b.qv[firstJ+j]
					r2 := rA.V + rB.V - (rA.X*rB.X + rA.Y*rB.Y + rA.Z*rB.Z)
					u2 := a2 * r2
					vij := float32(math.Exp(float64(-u2)))
					fs := 2 * vij
					dx := rA.X - rB.X
					dy := rA.Y - rB.Y
					dz := rA.Z - rB.Z
					cpuFv[firstI+wtx].V += qB * vij
					cpuFv[firstI+wtx].X += qB * (fs * dx)
					cpuFv[firstI+wtx].Y += qB * (fs * dy)
					cpuFv[firstI+wtx].Z += qB * (fs * dz)
				}
			}
		}
	}

	mismatches := 0
	for i := range cpuFv {
		if !closeEnough(cpuFv[i].V, gpuFv[i].V) ||
			!closeEnough(cpuFv[i].X, gpuFv[i].X) ||
			!closeEnough(cpuFv[i].Y, gpuFv[i].Y) ||
			!closeEnough(cpuFv[i].Z, gpuFv[i].Z) {
			if mismatches < 10 {
				log.Printf("mismatch at %d: cpu=(%g,%g,%g,%g) gpu=(%g,%g,%g,%g)\n",
					i, cpuFv[i].V, cpuFv[i].X, cpuFv[i].Y, cpuFv[i].Z,
					gpuFv[i].V, gpuFv[i].X, gpuFv[i].Y, gpuFv[i].Z)
			}
			mismatches++
		}
	}
	if mismatches > 0 {
		log.Fatalf("Verification failed: %d / %d particles mismatched\n",
			mismatches, len(cpuFv))
	}
	log.Printf("Passed!\n")
}

func closeEnough(a, b float32) bool {
	d := a - b
	if d < 0 {
		d = -d
	}
	ref := a
	if ref < 0 {
		ref = -ref
	}
	if ref < 1e-3 {
		return d < 1e-3
	}
	return d/ref < 1e-3
}
