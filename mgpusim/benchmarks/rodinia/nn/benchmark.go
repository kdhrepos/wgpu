// Package nn implements Rodinia's k-Nearest-Neighbor distance benchmark.
//
// One work-item per record computes Euclidean distance from a fixed target
// (TargetLat, TargetLng). Top-k selection is intentionally not on the GPU —
// upstream Rodinia also defers that to the host. The kernel itself is
// embarrassingly parallel; we ship it as a stride-access baseline for
// SpMV/PageRank-style indirect comparisons.
package nn

import (
	_ "embed"
	"log"
	"math"
	"math/rand/v2"

	"gitlab.com/akita/mgpusim/driver"
	"gitlab.com/akita/mgpusim/insts"
	"gitlab.com/akita/mgpusim/kernels"
)

// LatLong matches the OpenCL struct latLong { float lat; float lng; }.
type LatLong struct {
	Lat float32
	Lng float32
}

// NNKernelArgs is the kernel argument layout. Pointer args go first so no
// padding is required ahead of the trailing scalars.
type NNKernelArgs struct {
	Locations  driver.GPUPtr
	Distances  driver.GPUPtr
	NumRecords int32
	Lat        float32
	Lng        float32
	Padding    int32

	HiddenGlobalOffsetX int64
	HiddenGlobalOffsetY int64
	HiddenGlobalOffsetZ int64
}

// Benchmark is the nn benchmark.
type Benchmark struct {
	driver  *driver.Driver
	context *driver.Context
	gpus    []int
	queues  []*driver.CommandQueue
	kernel  *insts.HsaCo

	// Workload.
	NumRecords int
	TargetLat  float32
	TargetLng  float32

	// Host buffers.
	hLocations []LatLong
	hDistances []float32

	// Device buffers.
	locationsD driver.GPUPtr
	distancesD driver.GPUPtr

	useUnifiedMemory   bool
	useLASPMemoryAlloc bool
}

//go:embed kernels.hsaco
var hsacoBytes []byte

// NewBenchmark creates a new nn benchmark.
func NewBenchmark(d *driver.Driver) *Benchmark {
	b := new(Benchmark)
	b.driver = d
	b.context = d.Init()
	b.loadProgram()
	return b
}

// SelectGPU configures which GPU(s) the benchmark runs on.
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

func (b *Benchmark) loadProgram() {
	b.kernel = kernels.LoadProgramFromMemory(hsacoBytes, "NearestNeighbor")
	if b.kernel == nil {
		log.Panic("Failed to load NearestNeighbor kernel")
	}
}

// Run executes the benchmark end-to-end.
func (b *Benchmark) Run() {
	for _, gpu := range b.gpus {
		b.driver.SelectGPU(b.context, gpu)
		b.queues = append(b.queues, b.driver.CreateCommandQueue(b.context))
	}
	b.initMem()
	b.exec()
}

func (b *Benchmark) initMem() {
	if b.NumRecords <= 0 {
		log.Panicf("nn: NumRecords=%d must be positive", b.NumRecords)
	}

	// Generate synthetic locations in a [-90, +90] x [-180, +180] envelope.
	b.hLocations = make([]LatLong, b.NumRecords)
	for i := range b.hLocations {
		b.hLocations[i] = LatLong{
			Lat: (rand.Float32()*2 - 1) * 90,
			Lng: (rand.Float32()*2 - 1) * 180,
		}
	}
	b.hDistances = make([]float32, b.NumRecords)

	locBytes := uint64(b.NumRecords) * 8  // sizeof(LatLong)
	distBytes := uint64(b.NumRecords) * 4 // sizeof(float)

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

	b.locationsD = alloc(locBytes)
	b.distancesD = alloc(distBytes)
}

func (b *Benchmark) exec() {
	b.driver.MemCopyH2D(b.context, b.locationsD, b.hLocations)

	args := NNKernelArgs{
		Locations:  b.locationsD,
		Distances:  b.distancesD,
		NumRecords: int32(b.NumRecords),
		Lat:        b.TargetLat,
		Lng:        b.TargetLng,
	}

	const blockSize = 256
	globalSize := uint32(b.NumRecords)
	if globalSize%blockSize != 0 {
		globalSize = ((globalSize / blockSize) + 1) * blockSize
	}

	b.driver.LaunchKernel(b.context, b.kernel,
		[3]uint32{globalSize, 1, 1},
		[3]uint16{blockSize, 1, 1},
		&args)

	b.driver.MemCopyD2H(b.context, b.hDistances, b.distancesD)
}

// Verify recomputes the distance for every record on the host and compares.
func (b *Benchmark) Verify() {
	mismatches := 0
	for i, loc := range b.hLocations {
		dlat := float64(b.TargetLat - loc.Lat)
		dlng := float64(b.TargetLng - loc.Lng)
		want := float32(math.Sqrt(dlat*dlat + dlng*dlng))
		got := b.hDistances[i]
		if math.Abs(float64(got-want)) > 1e-3 {
			if mismatches < 5 {
				log.Printf("nn: record %d got %f want %f\n", i, got, want)
			}
			mismatches++
		}
	}
	if mismatches != 0 {
		log.Panicf("nn: %d/%d records mismatched", mismatches, b.NumRecords)
	}
	log.Printf("nn Passed! %d records\n", b.NumRecords)
}
