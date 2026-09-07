// Package embeddingbag implements an EmbeddingBag (sum pooling) benchmark.
//
// It mirrors the dominant kernel of DLRM's sparse path: for each of B bags,
// gather L embedding rows from a [V x D] table and reduce them along the
// bag axis, producing a [B x D] output. The kernel itself is small but the
// access pattern is the interesting bit — a data-dependent random gather
// over `table`, which stresses cross-chiplet RDMA on wafer-scale meshes.
package embeddingbag

import (
	"log"
	"math"
	"math/rand"

	// embed hsaco files
	_ "embed"

	"gitlab.com/akita/mgpusim/driver"
	"gitlab.com/akita/mgpusim/insts"
	"gitlab.com/akita/mgpusim/kernels"
)

// KernelArgs mirrors the EmbeddingBagSum kernel signature.
// Layout must match the AMD GCN ABI: scalar args first (packed, naturally
// aligned), then 8-byte pointers, then the three hidden global offsets.
type KernelArgs struct {
	B       uint32
	D       uint32
	Indices driver.GPUPtr
	Offsets driver.GPUPtr
	Table   driver.GPUPtr
	Out     driver.GPUPtr

	HiddenGlobalOffsetX int64
	HiddenGlobalOffsetY int64
	HiddenGlobalOffsetZ int64
}

// Benchmark holds host- and device-side state for one EmbeddingBag run.
type Benchmark struct {
	driver  *driver.Driver
	context *driver.Context
	gpus    []int
	hsaco   *insts.HsaCo

	// Problem shape.
	B int // number of bags (batch size)
	D int // embedding dimension
	V int // vocab size (number of rows in the embedding table)
	L int // fixed bag length (entries per bag) — kept fixed for simplicity

	// Distribution of the gather indices.
	//   false -> uniform over [0, V)
	//   true  -> Zipfian (s = 1.1); a small head of rows takes most of the load,
	//           which is the realistic case for production DLRM workloads and
	//           the one that creates per-chiplet hotspots under LASP.
	Zipf bool

	// Host buffers.
	indices []int32
	offsets []int32
	table   []float32
	out     []float32

	// Device buffers.
	gIndices driver.GPUPtr
	gOffsets driver.GPUPtr
	gTable   driver.GPUPtr
	gOut     driver.GPUPtr

	useUnifiedMemory   bool
	useLASPMemoryAlloc bool
}

//go:embed kernels.hsaco
var hsacoBytes []byte

// NewBenchmark returns a Benchmark with reasonable defaults; callers should
// override the shape fields (B, D, V, L) before calling Run().
func NewBenchmark(d *driver.Driver) *Benchmark {
	b := new(Benchmark)
	b.driver = d
	b.context = d.Init()
	b.hsaco = kernels.LoadProgramFromMemory(hsacoBytes, "EmbeddingBagSum")

	// Sensible defaults — a small DLRM-shaped problem that fits per-chiplet
	// memory budgets and still runs in a reasonable amount of simulated time.
	b.B = 1024
	b.D = 128
	b.V = 1 << 20 // 1M rows
	b.L = 32
	return b
}

// SelectGPU picks which simulated GPUs the kernel is launched on.
func (b *Benchmark) SelectGPU(gpus []int) { b.gpus = gpus }

// SetUnifiedMemory enables the unified-memory allocation path.
func (b *Benchmark) SetUnifiedMemory() { b.useUnifiedMemory = true }

// SetLASPMemoryAlloc enables the LASP allocator.
func (b *Benchmark) SetLASPMemoryAlloc() { b.useLASPMemoryAlloc = true }

// Run launches the benchmark on the currently-selected GPUs.
func (b *Benchmark) Run() {
	b.driver.SelectGPU(b.context, b.gpus[0])
	b.initMem()
	b.exec()
}

func (b *Benchmark) initMem() {
	nnz := b.B * b.L

	tableBytes := uint64(b.V) * uint64(b.D) * 4
	outBytes := uint64(b.B) * uint64(b.D) * 4
	idxBytes := uint64(nnz) * 4
	offBytes := uint64(b.B+1) * 4

	switch {
	case b.useUnifiedMemory:
		b.gIndices = b.driver.AllocateUnifiedMemory(b.context, idxBytes)
		b.gOffsets = b.driver.AllocateUnifiedMemory(b.context, offBytes)
		b.gTable = b.driver.AllocateUnifiedMemory(b.context, tableBytes)
		b.gOut = b.driver.AllocateUnifiedMemory(b.context, outBytes)
	case b.useLASPMemoryAlloc:
		b.gIndices = b.driver.AllocateMemoryLASP(b.context, idxBytes, "div4")
		b.gOffsets = b.driver.AllocateMemoryLASP(b.context, offBytes, "div4")
		b.gTable = b.driver.AllocateMemoryLASP(b.context, tableBytes, "div4")
		b.gOut = b.driver.AllocateMemoryLASP(b.context, outBytes, "div4")
	default:
		b.gIndices = b.driver.AllocateMemory(b.context, idxBytes)
		b.gOffsets = b.driver.AllocateMemory(b.context, offBytes)
		b.gTable = b.driver.AllocateMemory(b.context, tableBytes)
		b.gOut = b.driver.AllocateMemory(b.context, outBytes)

		b.driver.Distribute(b.context, b.gIndices, idxBytes, b.gpus)
		b.driver.Distribute(b.context, b.gOffsets, offBytes, b.gpus)
		b.driver.Distribute(b.context, b.gTable, tableBytes, b.gpus)
		b.driver.Distribute(b.context, b.gOut, outBytes, b.gpus)
	}

	b.fillInputs(nnz)

	b.driver.MemCopyH2D(b.context, b.gIndices, b.indices)
	b.driver.MemCopyH2D(b.context, b.gOffsets, b.offsets)
	b.driver.MemCopyH2D(b.context, b.gTable, b.table)
}

// fillInputs populates host-side input arrays. Deterministic seed so that
// reruns at the same shape produce identical results — important for the
// -verify path and for sweeping flags without input drift.
func (b *Benchmark) fillInputs(nnz int) {
	rng := rand.New(rand.NewSource(1))

	// Fixed-length bags: offsets[i] = i*L. Variable-length bags can be added
	// later if needed by writing a non-uniform offsets array here.
	b.offsets = make([]int32, b.B+1)
	for i := 0; i <= b.B; i++ {
		b.offsets[i] = int32(i * b.L)
	}

	b.indices = make([]int32, nnz)
	if b.Zipf {
		// math/rand's Zipf gives values in [0, imax]. s>1 controls skew; 1.1
		// is the canonical "moderate skew" used in DLRM-flavoured benchmarks.
		z := rand.NewZipf(rng, 1.1, 1.0, uint64(b.V-1))
		for i := 0; i < nnz; i++ {
			b.indices[i] = int32(z.Uint64())
		}
	} else {
		for i := 0; i < nnz; i++ {
			b.indices[i] = int32(rng.Intn(b.V))
		}
	}

	// Embedding table — small float pattern, just deterministic enough for
	// -verify to recompute on the host without storing reference outputs.
	b.table = make([]float32, b.V*b.D)
	for r := 0; r < b.V; r++ {
		base := r * b.D
		for d := 0; d < b.D; d++ {
			b.table[base+d] = float32(r%127)*0.01 + float32(d%31)*0.001
		}
	}

	b.out = make([]float32, b.B*b.D)
}

func (b *Benchmark) exec() {
	queues := make([]*driver.CommandQueue, len(b.gpus))

	// Split work along the batch (B) axis across GPUs.
	bPerGPU := b.B / len(b.gpus)

	for i, gpu := range b.gpus {
		b.driver.SelectGPU(b.context, gpu)
		q := b.driver.CreateCommandQueue(b.context)
		queues[i] = q

		args := KernelArgs{
			B:       uint32(b.B),
			D:       uint32(b.D),
			Indices: b.gIndices,
			Offsets: b.gOffsets,
			Table:   b.gTable,
			Out:     b.gOut,

			HiddenGlobalOffsetX: int64(bPerGPU * i),
			HiddenGlobalOffsetY: 0,
			HiddenGlobalOffsetZ: 0,
		}

		log.Printf("Launching kernel: embedding_bag (gpu=%d, bRange=[%d,%d), D=%d)",
			gpu, bPerGPU*i, bPerGPU*(i+1), b.D)

		b.driver.EnqueueLaunchKernel(
			q,
			b.hsaco,
			[3]uint32{uint32(bPerGPU), uint32(b.D), 1},
			[3]uint16{8, 8, 1},
			&args,
		)
	}

	for _, q := range queues {
		b.driver.DrainCommandQueue(q)
	}

	b.driver.MemCopyD2H(b.context, b.out, b.gOut)
}

// Verify recomputes the expected output on the host and compares element by
// element. Tolerance is set to absorb FP add-order differences across the
// reduction (sum of L=32 ~O(1) values).
func (b *Benchmark) Verify() {
	const eps = 1e-3
	for bi := 0; bi < b.B; bi++ {
		start := int(b.offsets[bi])
		end := int(b.offsets[bi+1])
		for d := 0; d < b.D; d++ {
			var acc float32
			for i := start; i < end; i++ {
				row := int(b.indices[i])
				acc += b.table[row*b.D+d]
			}
			got := b.out[bi*b.D+d]
			if math.Abs(float64(got-acc)) > eps {
				log.Panicf("mismatch at (b=%d, d=%d): want %f got %f",
					bi, d, acc, got)
			}
		}
	}
	log.Printf("Passed!\n")
}
