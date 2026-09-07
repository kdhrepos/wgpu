// Package tc implements a Triangle Counting benchmark.
// OpenCL kernel: native/kernel/tc_kernel.cl (written for this suite)
//
// Algorithm (vertex-centric merge-based set intersection):
//   For each vertex u, for each neighbor v > u, count |N(u) ∩ N(v)| using a
//   sorted-list merge. Each triangle is counted exactly once.
//   The per-vertex count is written to tc_per_vertex[], and atomic_add
//   accumulates into a single global counter.
//
// Characteristics: highly irregular memory access — pointer-chasing through
//   CSR neighbor lists of arbitrary length. The atomic_add to total_count is
//   a single hot inter-WG shared write; tc_per_vertex[] write is irregular
//   (each WG writes to its own vertex range).
//
// Prerequisite: neighbor lists must be sorted ascending. initMem() sorts them
//   on the host before the H2D copy.
package tc

import (
	_ "embed"
	"log"
	"sort"

	"gitlab.com/akita/mgpusim/benchmarks/matrix/csr"
	"gitlab.com/akita/mgpusim/driver"
	"gitlab.com/akita/mgpusim/insts"
	"gitlab.com/akita/mgpusim/kernels"
)

// TCKernelArgs is the argument layout for tc_kernel.
type TCKernelArgs struct {
	Row                 driver.GPUPtr // int32 CSR row offsets (NumNodes+1 entries)
	Col                 driver.GPUPtr // int32 CSR column indices (NumEdges entries)
	TcPerVertex         driver.GPUPtr // int32 per-vertex triangle count (output)
	TotalCount          driver.GPUPtr // int32 global triangle count (atomic output)
	NumNodes            int32
	NumEdges            int32
	HiddenGlobalOffsetX int64
	HiddenGlobalOffsetY int64
	HiddenGlobalOffsetZ int64
}

// Benchmark is the Triangle Counting benchmark.
type Benchmark struct {
	driver  *driver.Driver
	context *driver.Context
	gpus    []int
	queues  []*driver.CommandQueue
	kernel  *insts.HsaCo

	matrix   csr.Matrix
	NumNodes int32
	NumEdges int32

	// Device buffers
	rowD, colD       driver.GPUPtr
	tcPerVertexD     driver.GPUPtr
	totalCountD      driver.GPUPtr

	// Host results
	hTcPerVertex []int32
	hTotalCount  int32

	useUnifiedMemory   bool
	useLASPMemoryAlloc bool
}

// NewBenchmark creates a new Triangle Counting benchmark.
func NewBenchmark(d *driver.Driver) *Benchmark {
	b := new(Benchmark)
	b.driver = d
	b.context = d.Init()
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
	b.kernel = kernels.LoadProgramFromMemory(hsacoBytes, "tc_kernel")
	if b.kernel == nil {
		log.Panic("Failed to load tc_kernel")
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
	b.matrix = csr.MakeMatrixGenerator(uint32(b.NumNodes), uint32(b.NumEdges)).GenerateMatrix()

	// Sort each adjacency list so the merge-based intersection works correctly.
	row := b.matrix.RowOffsets
	col := b.matrix.ColumnNumbers
	for u := uint32(0); u < uint32(b.NumNodes); u++ {
		neighbors := col[row[u]:row[u+1]]
		sort.Slice(neighbors, func(i, j int) bool { return neighbors[i] < neighbors[j] })
	}

	nodeBytes := uint64(b.NumNodes * 4)
	edgeBytes := uint64(b.NumEdges * 4)

	if b.useUnifiedMemory {
		b.rowD = b.driver.AllocateUnifiedMemory(b.context, nodeBytes)
		b.colD = b.driver.AllocateUnifiedMemory(b.context, edgeBytes)
		b.tcPerVertexD = b.driver.AllocateUnifiedMemory(b.context, nodeBytes)
		b.totalCountD = b.driver.AllocateUnifiedMemory(b.context, 4)
	} else if b.useLASPMemoryAlloc {
		b.rowD = b.driver.AllocateMemoryLASP(b.context, nodeBytes, "div4")
		b.colD = b.driver.AllocateMemoryLASP(b.context, edgeBytes, "div4")
		b.tcPerVertexD = b.driver.AllocateMemoryLASP(b.context, nodeBytes, "div4")
		b.totalCountD = b.driver.AllocateMemoryLASP(b.context, 4, "div4")
	} else {
		b.rowD = b.driver.AllocateMemory(b.context, nodeBytes)
		b.colD = b.driver.AllocateMemory(b.context, edgeBytes)
		b.tcPerVertexD = b.driver.AllocateMemory(b.context, nodeBytes)
		b.totalCountD = b.driver.AllocateMemory(b.context, 4)
	}
}

func (b *Benchmark) exec() {
	b.driver.MemCopyH2D(b.context, b.rowD, b.matrix.RowOffsets)
	b.driver.MemCopyH2D(b.context, b.colD, b.matrix.ColumnNumbers)

	hZero := int32(0)
	b.driver.MemCopyH2D(b.context, b.totalCountD, &hZero)
	hTcZero := make([]int32, b.NumNodes)
	b.driver.MemCopyH2D(b.context, b.tcPerVertexD, hTcZero)

	blockSize := int32(64)
	globalSize := b.NumNodes
	if b.NumNodes%blockSize > 0 {
		globalSize = (b.NumNodes/blockSize + 1) * blockSize
	}

	args := TCKernelArgs{
		Row:          b.rowD,
		Col:          b.colD,
		TcPerVertex:  b.tcPerVertexD,
		TotalCount:   b.totalCountD,
		NumNodes:     b.NumNodes,
		NumEdges:     b.NumEdges,
	}
	b.driver.LaunchKernel(b.context, b.kernel,
		[3]uint32{uint32(globalSize), 1, 1},
		[3]uint16{uint16(blockSize), 1, 1},
		&args)

	b.hTcPerVertex = make([]int32, b.NumNodes)
	b.driver.MemCopyD2H(b.context, b.hTcPerVertex, b.tcPerVertexD)
	b.driver.MemCopyD2H(b.context, &b.hTotalCount, b.totalCountD)
}

// Verify checks the GPU triangle count against a CPU sequential count.
func (b *Benchmark) Verify() {
	cpuTotal := b.cpuTC()

	if b.hTotalCount != cpuTotal {
		log.Panicf("TC mismatch: GPU %d  CPU %d", b.hTotalCount, cpuTotal)
	}
	log.Printf("TC Passed! triangles=%d\n", b.hTotalCount)
}

// cpuTC counts triangles sequentially using the same merge-based approach.
func (b *Benchmark) cpuTC() int32 {
	row := b.matrix.RowOffsets
	col := b.matrix.ColumnNumbers
	var total int32

	for u := uint32(0); u < uint32(b.NumNodes); u++ {
		for ei := row[u]; ei < row[u+1]; ei++ {
			v := col[ei]
			if v <= u {
				continue
			}
			pi, pj := row[u], row[v]
			for pi < row[u+1] && pj < row[v+1] {
				a, bb := col[pi], col[pj]
				if a == bb {
					total++
					pi++
					pj++
				} else if a < bb {
					pi++
				} else {
					pj++
				}
			}
		}
	}
	return total
}
