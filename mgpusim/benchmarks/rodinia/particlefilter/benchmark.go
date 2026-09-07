// Package particlefilter implements the Particle Filter benchmark from the Rodinia suite.
// OpenCL source: https://github.com/yuhc/gpu-rodinia (opencl/particlefilter/particle_single.cl).
// fp32 variant — kernel symbols and arg order are identical to particle_double.cl.
//
// Algorithm (Sequential Importance Resampling):
//   likelihood_kernel      — for each particle, compute observation likelihood from a
//                            synthetic image. Uses __local buffer for per-WG partial sums.
//   normalize_weights_kernel — global weight reduction: each WG writes a partial sum to
//                             partial_sums[], then aggregates to get total; normalizes
//                             weights and builds the CDF for resampling.
//   find_index_kernel      — systematic resampling: each particle reads the shared CDF
//                            to find its new index (high inter-WG read sharing on CDF).
//
// Characteristics: normalize_weights has all WGs writing to a shared partial_sums[] array
// (high inter-WG sharing); find_index has irregular CDF-pointer-chasing access.
package particlefilter

import (
	_ "embed"
	"log"
	"math"
	"math/rand"

	"gitlab.com/akita/mgpusim/driver"
	"gitlab.com/akita/mgpusim/insts"
	"gitlab.com/akita/mgpusim/kernels"
)

// LikelihoodKernelArgs matches the Rodinia particle_single.cl likelihood_kernel.
type LikelihoodKernelArgs struct {
	ArrayX      driver.GPUPtr   // float32: current particle X positions
	ArrayY      driver.GPUPtr   // float32: current particle Y positions
	Xj          driver.GPUPtr   // float32: proposed X positions
	Yj          driver.GPUPtr   // float32: proposed Y positions
	CDF         driver.GPUPtr   // float32: cumulative distribution function
	Ind         driver.GPUPtr   // int32:   flat index into image for each particle
	ObjXY       driver.GPUPtr   // int32:   object pixel offsets (countOnes × 2)
	Likelihood  driver.GPUPtr   // float32: per-particle likelihood (output)
	I           driver.GPUPtr   // uint8:   image data (IszX × IszY × Nfr)
	U           driver.GPUPtr   // float32: uniform random samples
	Weights     driver.GPUPtr   // float32: particle weights (output)
	Nparticles  int32
	CountOnes   int32
	MaxSize     int32
	K           int32           // current frame index
	IszY        int32
	Nfr         int32
	// No padding here: 11 ptrs (88B) + 6 int32 (24B) = 112B, already
	// 8-aligned, so Seed lands at kernarg byte 112. Matches the HSACO
	// ABI seen in the disasm: seed@0x70, partial_sums@0x78,
	// buffer@0x80, hidden_global_offset@0x88. Args are encoded with
	// binary.Write (packed, no Go struct alignment), so field order +
	// size alone determine the byte offsets.
	Seed        driver.GPUPtr   // @112  int32: per-particle RNG seeds
	PartialSums driver.GPUPtr   // @120  float32: per-WG partial weight sums
	Buffer      driver.LocalPtr // @128  local scratch (4 bytes)
	Padding     int32           // @132  pads hidden offsets to @136 (0x88)
	HiddenGlobalOffsetX int64   // @136
	HiddenGlobalOffsetY int64
	HiddenGlobalOffsetZ int64
}

// NormalizeWeightsKernelArgs matches normalize_weights_kernel.
type NormalizeWeightsKernelArgs struct {
	Weights     driver.GPUPtr   // float32: particle weights (in/out)
	Nparticles  int32
	Padding     int32
	PartialSums driver.GPUPtr   // float32: per-WG partial sums (inter-WG shared)
	CDF         driver.GPUPtr   // float32: CDF output
	U           driver.GPUPtr   // float32: uniform samples
	Seed        driver.GPUPtr   // int32: RNG seeds
	HiddenGlobalOffsetX int64
	HiddenGlobalOffsetY int64
	HiddenGlobalOffsetZ int64
}

// FindIndexKernelArgs matches find_index_kernel.
type FindIndexKernelArgs struct {
	ArrayX     driver.GPUPtr   // float32: current X (in)
	ArrayY     driver.GPUPtr   // float32: current Y (in)
	CDF        driver.GPUPtr   // float32: CDF (shared read across all WGs)
	U          driver.GPUPtr   // float32: uniform samples
	Xj         driver.GPUPtr   // float32: new X positions (out)
	Yj         driver.GPUPtr   // float32: new Y positions (out)
	Weights    driver.GPUPtr   // float32: reset weights (out)
	Nparticles int32
	Padding    int32
	HiddenGlobalOffsetX int64
	HiddenGlobalOffsetY int64
	HiddenGlobalOffsetZ int64
}

// Benchmark is the Particle Filter benchmark.
type Benchmark struct {
	driver  *driver.Driver
	context *driver.Context
	gpus    []int
	queues  []*driver.CommandQueue

	kernelLikelihood       *insts.HsaCo
	kernelNormalizeWeights *insts.HsaCo
	kernelFindIndex        *insts.HsaCo

	Nparticles int32 // number of particles (default 1024)
	IszX       int32 // image width  (default 128)
	IszY       int32 // image height (default 128)
	Nfr        int32 // number of frames (default 10)

	// Device buffers
	arrayXD, arrayYD   driver.GPUPtr // current positions
	xjD, yjD           driver.GPUPtr // proposed positions
	weightsD           driver.GPUPtr // particle weights
	cdfD               driver.GPUPtr // CDF
	uD                 driver.GPUPtr // uniform random samples
	indD               driver.GPUPtr // image index per particle
	objXYD             driver.GPUPtr // object pixel offsets
	likelihoodD        driver.GPUPtr // per-particle likelihood
	imageD             driver.GPUPtr // synthetic image
	seedD              driver.GPUPtr // RNG seeds
	partialSumsD       driver.GPUPtr // per-WG partial sums

	countOnes  int32 // number of object pixels (synthetic)
	hArrayX    []float32
	hArrayY    []float32

	useUnifiedMemory   bool
	useLASPMemoryAlloc bool
}

// NewBenchmark creates a new Particle Filter benchmark.
func NewBenchmark(d *driver.Driver) *Benchmark {
	b := new(Benchmark)
	b.driver = d
	b.context = d.Init()
	b.Nparticles = 1024
	b.IszX = 128
	b.IszY = 128
	b.Nfr = 10
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
	b.kernelLikelihood = kernels.LoadProgramFromMemory(hsacoBytes, "likelihood_kernel")
	if b.kernelLikelihood == nil {
		log.Panic("Failed to load likelihood_kernel")
	}
	b.kernelNormalizeWeights = kernels.LoadProgramFromMemory(hsacoBytes, "normalize_weights_kernel")
	if b.kernelNormalizeWeights == nil {
		log.Panic("Failed to load normalize_weights_kernel")
	}
	b.kernelFindIndex = kernels.LoadProgramFromMemory(hsacoBytes, "find_index_kernel")
	if b.kernelFindIndex == nil {
		log.Panic("Failed to load find_index_kernel")
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

func (b *Benchmark) alloc(size uint64) driver.GPUPtr {
	if b.useUnifiedMemory {
		return b.driver.AllocateUnifiedMemory(b.context, size)
	} else if b.useLASPMemoryAlloc {
		return b.driver.AllocateMemoryLASP(b.context, size, "div4")
	}
	return b.driver.AllocateMemory(b.context, size)
}

func (b *Benchmark) initMem() {
	n := b.Nparticles
	imgSize := b.IszX * b.IszY * b.Nfr
	// Use a simple 8-pixel cross as the tracked object.
	b.countOnes = 8
	maxSize := b.IszX * b.IszY * b.Nfr

	b.arrayXD = b.alloc(uint64(n * 4))   // float32
	b.arrayYD = b.alloc(uint64(n * 4))
	b.xjD = b.alloc(uint64(n * 4))
	b.yjD = b.alloc(uint64(n * 4))
	b.weightsD = b.alloc(uint64(n * 4))
	b.cdfD = b.alloc(uint64(n * 4))
	b.uD = b.alloc(uint64(n * 4))
	// likelihood_kernel writes ind[i*countOnes + y] for every particle i
	// and object pixel y, so this buffer holds Nparticles*countOnes ints,
	// not Nparticles. Under-sizing it sends ind[] writes/reads past the
	// allocation into unmapped pages (wild address -> converter panic).
	b.indD = b.alloc(uint64(n) * uint64(b.countOnes) * 4) // int32, n*countOnes
	b.objXYD = b.alloc(uint64(b.countOnes * 2 * 4)) // int32 pairs
	b.likelihoodD = b.alloc(uint64(n * 4))
	b.imageD = b.alloc(uint64(imgSize))   // uint8
	b.seedD = b.alloc(uint64(n * 4))      // int32
	// Enough partial sums for one per WG (ceil(n/256) WGs).
	numBlocks := (n + 255) / 256
	b.partialSumsD = b.alloc(uint64(numBlocks * 4))

	// Initialize host data
	hX := make([]float32, n)
	hY := make([]float32, n)
	hW := make([]float32, n)
	hSeed := make([]int32, n)
	hU := make([]float32, n)
	for i := int32(0); i < n; i++ {
		hX[i] = float32(b.IszX/2) + float32(rand.NormFloat64()*5) //nolint:gosec
		hY[i] = float32(b.IszY/2) + float32(rand.NormFloat64()*5)
		hW[i] = 1.0 / float32(n)
		hSeed[i] = rand.Int31() //nolint:gosec
		hU[i] = rand.Float32()  //nolint:gosec
	}
	// Object pixel offsets (simple cross pattern)
	hObjXY := []int32{0, -1, 0, 1, -1, 0, 1, 0, 0, -2, 0, 2, -2, 0, 2, 0}

	// Synthetic binary image
	hImage := make([]uint8, imgSize)
	cx, cy := b.IszX/2, b.IszY/2
	for f := int32(0); f < b.Nfr; f++ {
		for _, dy := range []int32{-1, 0, 1} {
			for _, dx := range []int32{-1, 0, 1} {
				px := cx + dx
				py := cy + dy
				if px >= 0 && px < b.IszX && py >= 0 && py < b.IszY {
					hImage[f*b.IszX*b.IszY+py*b.IszX+px] = 1
				}
			}
		}
	}

	b.driver.MemCopyH2D(b.context, b.arrayXD, hX)
	b.driver.MemCopyH2D(b.context, b.arrayYD, hY)
	b.driver.MemCopyH2D(b.context, b.weightsD, hW)
	b.driver.MemCopyH2D(b.context, b.seedD, hSeed)
	b.driver.MemCopyH2D(b.context, b.uD, hU)
	b.driver.MemCopyH2D(b.context, b.objXYD, hObjXY)
	b.driver.MemCopyH2D(b.context, b.imageD, hImage)

	_ = maxSize
}

func (b *Benchmark) exec() {
	n := b.Nparticles
	blockSize := int32(256)
	numBlocks := (n + blockSize - 1) / blockSize
	globalSize := numBlocks * blockSize
	lmemSize := uint64(blockSize * 4) // float32 per thread

	gw := [3]uint32{uint32(globalSize), 1, 1}
	lw := [3]uint16{uint16(blockSize), 1, 1}

	for k := int32(0); k < b.Nfr; k++ {
		// 1. Likelihood
		likArgs := LikelihoodKernelArgs{
			ArrayX:      b.arrayXD,
			ArrayY:      b.arrayYD,
			Xj:          b.xjD,
			Yj:          b.yjD,
			CDF:         b.cdfD,
			Ind:         b.indD,
			ObjXY:       b.objXYD,
			Likelihood:  b.likelihoodD,
			I:           b.imageD,
			U:           b.uD,
			Weights:     b.weightsD,
			Nparticles:  n,
			CountOnes:   b.countOnes,
			MaxSize:     b.IszX * b.IszY * b.Nfr,
			K:           k,
			IszY:        b.IszY,
			Nfr:         b.Nfr,
			Seed:        b.seedD,
			PartialSums: b.partialSumsD,
			Buffer:      driver.LocalPtr(lmemSize),
		}
		b.driver.LaunchKernel(b.context, b.kernelLikelihood, gw, lw, &likArgs)

		// 2. Normalize weights
		normArgs := NormalizeWeightsKernelArgs{
			Weights:     b.weightsD,
			Nparticles:  n,
			PartialSums: b.partialSumsD,
			CDF:         b.cdfD,
			U:           b.uD,
			Seed:        b.seedD,
		}
		b.driver.LaunchKernel(b.context, b.kernelNormalizeWeights, gw, lw, &normArgs)

		// 3. Resample
		findArgs := FindIndexKernelArgs{
			ArrayX:     b.arrayXD,
			ArrayY:     b.arrayYD,
			CDF:        b.cdfD,
			U:          b.uD,
			Xj:         b.xjD,
			Yj:         b.yjD,
			Weights:    b.weightsD,
			Nparticles: n,
		}
		b.driver.LaunchKernel(b.context, b.kernelFindIndex, gw, lw, &findArgs)

		// Swap buffers
		b.arrayXD, b.xjD = b.xjD, b.arrayXD
		b.arrayYD, b.yjD = b.yjD, b.arrayYD
	}

	b.hArrayX = make([]float32, n)
	b.hArrayY = make([]float32, n)
	b.driver.MemCopyD2H(b.context, b.hArrayX, b.arrayXD)
	b.driver.MemCopyD2H(b.context, b.hArrayY, b.arrayYD)
}

// Verify checks that the mean particle position is near the tracked object center.
func (b *Benchmark) Verify() {
	cx := float64(b.IszX / 2)
	cy := float64(b.IszY / 2)

	var sumX, sumY float64
	for i := range b.hArrayX {
		sumX += float64(b.hArrayX[i])
		sumY += float64(b.hArrayY[i])
	}
	n := float64(b.Nparticles)
	meanX := sumX / n
	meanY := sumY / n

	distX := math.Abs(meanX - cx)
	distY := math.Abs(meanY - cy)

	// Loose tolerance: mean should stay within 20 pixels of object center.
	if distX > 20 || distY > 20 {
		log.Panicf("ParticleFilter: mean position (%.1f, %.1f) too far from object (%.1f, %.1f)",
			meanX, meanY, cx, cy)
	}
	log.Printf("ParticleFilter Passed! mean=(%.2f, %.2f) object=(%.1f, %.1f)\n",
		meanX, meanY, cx, cy)
}
