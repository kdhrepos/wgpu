// Package kcore implements the K-Core Decomposition benchmark.
// OpenCL kernel: native/kernel/kcore_kernel.cl (written for this suite)
//
// Algorithm (iterative vertex peeling):
//   1. init_degree_kernel  — set degree[v] = CSR out-degree for each vertex.
//   2. peel_kernel         — mark vertices with degree < K as to_remove, set changed=1.
//   3. update_degree_kernel — for each removed vertex u, decrement degree[w]
//                            for every still-active neighbor w.
//   Repeat 2-3 until changed == 0.
//
// Characteristics: update_degree_kernel has all WGs simultaneously issuing
// atomic_sub to the shared degree[] array (high inter-WG sharing); peel_kernel
// uses atomic_or on a single shared changed flag; edge accesses via CSR are
// irregular.
package kcore

import (
	_ "embed"
	"log"

	"gitlab.com/akita/mgpusim/benchmarks/matrix/csr"
	"gitlab.com/akita/mgpusim/driver"
	"gitlab.com/akita/mgpusim/insts"
	"gitlab.com/akita/mgpusim/kernels"
)

// InitDegreeKernelArgs is the argument layout for init_degree_kernel.
type InitDegreeKernelArgs struct {
	Row                 driver.GPUPtr
	Active              driver.GPUPtr // int32: 1 = alive
	Degree              driver.GPUPtr // int32: current effective degree
	NumNodes            int32
	Padding             int32
	HiddenGlobalOffsetX int64
	HiddenGlobalOffsetY int64
	HiddenGlobalOffsetZ int64
}

// PeelKernelArgs is the argument layout for peel_kernel.
type PeelKernelArgs struct {
	Degree              driver.GPUPtr
	Active              driver.GPUPtr
	ToRemove            driver.GPUPtr // int32: 1 = queued for removal this round
	Changed             driver.GPUPtr // int32: atomic flag, 1 if any vertex was removed
	K                   int32
	NumNodes            int32
	HiddenGlobalOffsetX int64
	HiddenGlobalOffsetY int64
	HiddenGlobalOffsetZ int64
}

// UpdateDegreeKernelArgs is the argument layout for update_degree_kernel.
type UpdateDegreeKernelArgs struct {
	Row                 driver.GPUPtr
	Col                 driver.GPUPtr
	Active              driver.GPUPtr
	Degree              driver.GPUPtr
	ToRemove            driver.GPUPtr
	NumNodes            int32
	Padding             int32
	HiddenGlobalOffsetX int64
	HiddenGlobalOffsetY int64
	HiddenGlobalOffsetZ int64
}

// Benchmark is the K-Core benchmark.
type Benchmark struct {
	driver  *driver.Driver
	context *driver.Context
	gpus    []int
	queues  []*driver.CommandQueue

	kernelInitDegree   *insts.HsaCo
	kernelPeel         *insts.HsaCo
	kernelUpdateDegree *insts.HsaCo

	matrix   csr.Matrix
	NumNodes int32
	NumEdges int32
	K        int32 // target core number (default 2)

	// Device buffers
	rowD, colD   driver.GPUPtr
	activeD      driver.GPUPtr
	degreeD      driver.GPUPtr
	toRemoveD    driver.GPUPtr
	changedD     driver.GPUPtr

	// Host result
	hActive []int32

	useUnifiedMemory   bool
	useLASPMemoryAlloc bool
}

// NewBenchmark creates a new K-Core benchmark.
func NewBenchmark(d *driver.Driver) *Benchmark {
	b := new(Benchmark)
	b.driver = d
	b.context = d.Init()
	b.K = 2
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
	b.kernelInitDegree = kernels.LoadProgramFromMemory(hsacoBytes, "init_degree_kernel")
	if b.kernelInitDegree == nil {
		log.Panic("Failed to load init_degree_kernel")
	}
	b.kernelPeel = kernels.LoadProgramFromMemory(hsacoBytes, "peel_kernel")
	if b.kernelPeel == nil {
		log.Panic("Failed to load peel_kernel")
	}
	b.kernelUpdateDegree = kernels.LoadProgramFromMemory(hsacoBytes, "update_degree_kernel")
	if b.kernelUpdateDegree == nil {
		log.Panic("Failed to load update_degree_kernel")
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
	b.matrix = csr.MakeMatrixGenerator(uint32(b.NumNodes), uint32(b.NumEdges)).GenerateMatrix()

	nodeBytes := uint64(b.NumNodes * 4)
	edgeBytes := uint64(b.NumEdges * 4)

	b.rowD = b.alloc(nodeBytes)
	b.colD = b.alloc(edgeBytes)
	b.activeD = b.alloc(nodeBytes)
	b.degreeD = b.alloc(nodeBytes)
	b.toRemoveD = b.alloc(nodeBytes)
	b.changedD = b.alloc(4)

	b.driver.MemCopyH2D(b.context, b.rowD, b.matrix.RowOffsets)
	b.driver.MemCopyH2D(b.context, b.colD, b.matrix.ColumnNumbers)

	hZero := make([]int32, b.NumNodes)
	b.driver.MemCopyH2D(b.context, b.toRemoveD, hZero)
}

func (b *Benchmark) exec() {
	blockSize := int32(128)
	nodeGlobal := b.NumNodes
	if b.NumNodes%blockSize > 0 {
		nodeGlobal = (b.NumNodes/blockSize + 1) * blockSize
	}
	gw := [3]uint32{uint32(nodeGlobal), 1, 1}
	lw := [3]uint16{uint16(blockSize), 1, 1}

	// 1. Initialize degrees
	initArgs := InitDegreeKernelArgs{
		Row:      b.rowD,
		Active:   b.activeD,
		Degree:   b.degreeD,
		NumNodes: b.NumNodes,
	}
	b.driver.LaunchKernel(b.context, b.kernelInitDegree, gw, lw, &initArgs)

	peelArgs := PeelKernelArgs{
		Degree:   b.degreeD,
		Active:   b.activeD,
		ToRemove: b.toRemoveD,
		Changed:  b.changedD,
		K:        b.K,
		NumNodes: b.NumNodes,
	}
	updateArgs := UpdateDegreeKernelArgs{
		Row:      b.rowD,
		Col:      b.colD,
		Active:   b.activeD,
		Degree:   b.degreeD,
		ToRemove: b.toRemoveD,
		NumNodes: b.NumNodes,
	}

	// 2. Iterative peeling
	changed := int32(1)
	for changed != 0 {
		changed = 0
		b.driver.MemCopyH2D(b.context, b.changedD, &changed)

		b.driver.LaunchKernel(b.context, b.kernelPeel, gw, lw, &peelArgs)
		b.driver.LaunchKernel(b.context, b.kernelUpdateDegree, gw, lw, &updateArgs)

		b.driver.MemCopyD2H(b.context, &changed, b.changedD)
	}

	b.hActive = make([]int32, b.NumNodes)
	b.driver.MemCopyD2H(b.context, b.hActive, b.activeD)
}

// Verify compares the GPU k-core membership with a CPU sequential peeling.
func (b *Benchmark) Verify() {
	cpuActive := b.cpuKCore()

	passed := true
	for i := int32(0); i < b.NumNodes; i++ {
		if b.hActive[i] != cpuActive[i] {
			log.Printf("mismatch at vertex %d: GPU active=%d CPU active=%d\n",
				i, b.hActive[i], cpuActive[i])
			passed = false
		}
	}
	if passed {
		coreSize := int32(0)
		for _, a := range b.hActive {
			coreSize += a
		}
		log.Printf("K-Core (k=%d) Passed! core size=%d / %d vertices\n",
			b.K, coreSize, b.NumNodes)
	} else {
		log.Panic("K-Core verification failed")
	}
}

// cpuKCore runs sequential iterative peeling to find the k-core membership.
func (b *Benchmark) cpuKCore() []int32 {
	row := b.matrix.RowOffsets
	col := b.matrix.ColumnNumbers
	n := int(b.NumNodes)

	active := make([]int32, n)
	degree := make([]int32, n)
	for i := 0; i < n; i++ {
		active[i] = 1
		degree[i] = int32(row[i+1] - row[i])
	}

	changed := true
	for changed {
		changed = false
		for u := 0; u < n; u++ {
			if active[u] == 0 {
				continue
			}
			if degree[u] < b.K {
				active[u] = 0
				changed = true
				for e := row[u]; e < row[u+1]; e++ {
					w := col[e]
					if active[w] == 1 {
						degree[w]--
					}
				}
			}
		}
	}
	return active
}
