// Package cc implements the Connected Components benchmark (Pannotia-style).
// OpenCL source: https://github.com/pannotia/pannotia (graph_app/connected_comp/kernel/kernel_cc.cl)
//
// Algorithm (Shiloach-Vishkin parallel union-find):
//
//	Hook    — for each edge (u,v), if comp[u] != comp[v] try to merge by setting
//	          comp[max(comp[u],comp[v])] = min(comp[u],comp[v]) atomically.
//	Compress — path compression: comp[i] = comp[comp[i]] until stable.
//	Repeat until no change is detected via a shared 'changed' flag.
//
// Characteristics: all WGs simultaneously write to the same comp[] array via atomics
// (high inter-WG sharing), and edge accesses follow an irregular CSR pattern.
package cc

import (
	_ "embed"
	"log"

	"gitlab.com/akita/mgpusim/benchmarks/matrix/csr"
	"gitlab.com/akita/mgpusim/driver"
	"gitlab.com/akita/mgpusim/insts"
	"gitlab.com/akita/mgpusim/kernels"
)

// HookKernelArgs is the argument layout for the hook kernel.
type HookKernelArgs struct {
	Row                 driver.GPUPtr
	Col                 driver.GPUPtr
	Comp                driver.GPUPtr // int32 component labels
	Changed             driver.GPUPtr // int32 flag: set to 1 when any merge occurs
	NumNodes            int32
	NumEdges            int32
	Padding             int32
	Pad2                int32
	HiddenGlobalOffsetX int64
	HiddenGlobalOffsetY int64
	HiddenGlobalOffsetZ int64
}

// CompressKernelArgs is the argument layout for the path-compression kernel.
type CompressKernelArgs struct {
	Comp                driver.GPUPtr
	Changed             driver.GPUPtr
	NumNodes            int32
	Padding             int32
	HiddenGlobalOffsetX int64
	HiddenGlobalOffsetY int64
	HiddenGlobalOffsetZ int64
}

// Benchmark is the CC benchmark.
type Benchmark struct {
	driver  *driver.Driver
	context *driver.Context
	gpus    []int
	queues  []*driver.CommandQueue

	kernelHook     *insts.HsaCo
	kernelCompress *insts.HsaCo

	matrix   csr.Matrix
	NumNodes int32
	NumEdges int32

	// Device buffers
	rowD, colD driver.GPUPtr
	compD      driver.GPUPtr // int32 component labels
	changedD   driver.GPUPtr // int32 convergence flag

	// Host result
	hComp []int32

	useUnifiedMemory   bool
	useLASPMemoryAlloc bool
}

// NewBenchmark creates a new CC benchmark.
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
	b.kernelHook = kernels.LoadProgramFromMemory(hsacoBytes, "hook_kernel")
	if b.kernelHook == nil {
		log.Panic("Failed to load hook_kernel")
	}
	b.kernelCompress = kernels.LoadProgramFromMemory(hsacoBytes, "compress_kernel")
	if b.kernelCompress == nil {
		log.Panic("Failed to load compress_kernel")
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

	nodeBytes := uint64(b.NumNodes * 4)
	edgeBytes := uint64(b.NumEdges * 4)

	if b.useUnifiedMemory {
		b.rowD = b.driver.AllocateUnifiedMemory(b.context, nodeBytes)
		b.colD = b.driver.AllocateUnifiedMemory(b.context, edgeBytes)
		b.compD = b.driver.AllocateUnifiedMemory(b.context, nodeBytes)
		b.changedD = b.driver.AllocateUnifiedMemory(b.context, 4)
	} else if b.useLASPMemoryAlloc {
		b.rowD = b.driver.AllocateMemoryLASP(b.context, nodeBytes, "div4")
		b.colD = b.driver.AllocateMemoryLASP(b.context, edgeBytes, "div4")
		b.compD = b.driver.AllocateMemoryLASP(b.context, nodeBytes, "div4")
		b.changedD = b.driver.AllocateMemoryLASP(b.context, 4, "div4")
	} else {
		b.rowD = b.driver.AllocateMemory(b.context, nodeBytes)
		b.colD = b.driver.AllocateMemory(b.context, edgeBytes)
		b.compD = b.driver.AllocateMemory(b.context, nodeBytes)
		b.changedD = b.driver.AllocateMemory(b.context, 4)
	}
}

func (b *Benchmark) exec() {
	// Initialize comp[i] = i
	hComp := make([]int32, b.NumNodes)
	for i := int32(0); i < b.NumNodes; i++ {
		hComp[i] = i
	}
	b.driver.MemCopyH2D(b.context, b.compD, hComp)
	b.driver.MemCopyH2D(b.context, b.rowD, b.matrix.RowOffsets)
	b.driver.MemCopyH2D(b.context, b.colD, b.matrix.ColumnNumbers)

	hookBlockSize := int32(64)
	edgeGlobal := b.NumEdges
	if b.NumEdges%hookBlockSize > 0 {
		edgeGlobal = (b.NumEdges/hookBlockSize + 1) * hookBlockSize
	}
	hookGW := [3]uint32{uint32(edgeGlobal), 1, 1}
	hookLW := [3]uint16{uint16(hookBlockSize), 1, 1}

	compressBlockSize := int32(64)
	nodeGlobal := b.NumNodes
	if b.NumNodes%compressBlockSize > 0 {
		nodeGlobal = (b.NumNodes/compressBlockSize + 1) * compressBlockSize
	}
	compressGW := [3]uint32{uint32(nodeGlobal), 1, 1}
	compressLW := [3]uint16{uint16(compressBlockSize), 1, 1}

	hookArgs := HookKernelArgs{
		Row:      b.rowD,
		Col:      b.colD,
		Comp:     b.compD,
		Changed:  b.changedD,
		NumNodes: b.NumNodes,
		NumEdges: b.NumEdges,
	}
	compressArgs := CompressKernelArgs{
		Comp:     b.compD,
		Changed:  b.changedD,
		NumNodes: b.NumNodes,
	}

	changed := int32(1)
	for changed != 0 {
		changed = 0
		b.driver.MemCopyH2D(b.context, b.changedD, &changed)

		b.driver.LaunchKernel(b.context, b.kernelHook, hookGW, hookLW, &hookArgs)
		b.driver.LaunchKernel(b.context, b.kernelCompress, compressGW, compressLW, &compressArgs)

		b.driver.MemCopyD2H(b.context, &changed, b.changedD)
	}

	b.hComp = make([]int32, b.NumNodes)
	b.driver.MemCopyD2H(b.context, b.hComp, b.compD)
}

// Verify checks the result against a sequential CPU union-find.
func (b *Benchmark) Verify() {
	cpuComp := b.cpuCC()

	// Normalize both: map raw labels to canonical component IDs by rank.
	gpuNorm := normalize(b.hComp)
	cpuNorm := normalize(cpuComp)

	passed := true
	for i := int32(0); i < b.NumNodes; i++ {
		if gpuNorm[i] != cpuNorm[i] {
			log.Printf("mismatch at node %d: GPU comp %d CPU comp %d\n",
				i, gpuNorm[i], cpuNorm[i])
			passed = false
		}
	}
	if passed {
		log.Printf("CC Passed!\n")
	} else {
		log.Panic("CC verification failed")
	}
}

// cpuCC runs sequential union-find on the CSR graph.
func (b *Benchmark) cpuCC() []int32 {
	row := b.matrix.RowOffsets
	col := b.matrix.ColumnNumbers
	n := int(b.NumNodes)

	parent := make([]int32, n)
	for i := range parent {
		parent[i] = int32(i)
	}

	var find func(int32) int32
	find = func(x int32) int32 {
		if parent[x] != x {
			parent[x] = find(parent[x])
		}
		return parent[x]
	}
	union := func(a, b int32) {
		ra, rb := find(a), find(b)
		if ra != rb {
			if ra < rb {
				parent[rb] = ra
			} else {
				parent[ra] = rb
			}
		}
	}

	for u := 0; u < n; u++ {
		for e := row[u]; e < row[u+1]; e++ {
			union(int32(u), int32(col[e]))
		}
	}
	// Path-compress all
	for i := 0; i < n; i++ {
		find(int32(i))
	}
	return parent
}

// normalize remaps component labels to a stable canonical form (sorted by first occurrence).
func normalize(comp []int32) []int32 {
	remap := make(map[int32]int32)
	out := make([]int32, len(comp))
	next := int32(0)
	for i, c := range comp {
		root := c
		// resolve to root if it's a direct label
		_ = root
		if _, ok := remap[c]; !ok {
			remap[c] = next
			next++
		}
		out[i] = remap[c]
	}
	return out
}
