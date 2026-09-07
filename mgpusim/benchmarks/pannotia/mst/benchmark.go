// Package mst implements a Minimum Spanning Tree benchmark (Borůvka's algorithm).
//
// Algorithm: iterative reset → find_min → union → compress until convergence.
//   find_min: each vertex scans its edges; writes min-weight cross-component
//     edge into min_wt[comp[u]] / min_edge[comp[u]] (non-atomic stub).
//   union: component roots merge with chosen destination (non-atomic stub).
//   compress: comp[v] = comp[comp[v]] path flattening.
//
// Sharing: find_min creates a per-component reduction — all vertices of the
// same component contend on min_wt[srccomp]. With Zipf graphs, large hub
// components generate extreme hot spots, unlike CC which has no reduction step.
package mst

import (
	_ "embed"
	"log"
	"math/rand"

	"gitlab.com/akita/mgpusim/benchmarks/matrix/csr"
	"gitlab.com/akita/mgpusim/driver"
	"gitlab.com/akita/mgpusim/insts"
	"gitlab.com/akita/mgpusim/kernels"
)

const intMaxVal = int32(0x7FFFFFFF)

// ResetKernelArgs matches mst_reset argument layout.
type ResetKernelArgs struct {
	MinWt               driver.GPUPtr
	MinEdge             driver.GPUPtr
	Changed             driver.GPUPtr
	NumNodes            int32
	Padding             int32
	HiddenGlobalOffsetX int64
	HiddenGlobalOffsetY int64
	HiddenGlobalOffsetZ int64
}

// FindMinKernelArgs matches mst_find_min argument layout.
type FindMinKernelArgs struct {
	Row                 driver.GPUPtr
	Col                 driver.GPUPtr
	Weight              driver.GPUPtr
	Comp                driver.GPUPtr
	MinWt               driver.GPUPtr
	MinEdge             driver.GPUPtr
	Changed             driver.GPUPtr
	NumNodes            int32
	NumEdges            int32
	HiddenGlobalOffsetX int64
	HiddenGlobalOffsetY int64
	HiddenGlobalOffsetZ int64
}

// UnionKernelArgs matches mst_union argument layout.
type UnionKernelArgs struct {
	Col                 driver.GPUPtr
	Comp                driver.GPUPtr
	MinWt               driver.GPUPtr
	MinEdge             driver.GPUPtr
	Changed             driver.GPUPtr
	NumNodes            int32
	Padding             int32
	HiddenGlobalOffsetX int64
	HiddenGlobalOffsetY int64
	HiddenGlobalOffsetZ int64
}

// CompressKernelArgs matches mst_compress argument layout.
type CompressKernelArgs struct {
	Comp                driver.GPUPtr
	Changed             driver.GPUPtr
	NumNodes            int32
	Padding             int32
	HiddenGlobalOffsetX int64
	HiddenGlobalOffsetY int64
	HiddenGlobalOffsetZ int64
}

// Benchmark is the MST benchmark.
type Benchmark struct {
	driver  *driver.Driver
	context *driver.Context
	gpus    []int
	queues  []*driver.CommandQueue

	kernelReset    *insts.HsaCo
	kernelFindMin  *insts.HsaCo
	kernelUnion    *insts.HsaCo
	kernelCompress *insts.HsaCo

	matrix   csr.Matrix
	NumNodes int32
	NumEdges int32

	rowD, colD, weightD driver.GPUPtr
	compD               driver.GPUPtr
	minWtD, minEdgeD    driver.GPUPtr
	changedD            driver.GPUPtr

	hComp []int32

	useUnifiedMemory   bool
	useLASPMemoryAlloc bool
}

// NewBenchmark creates a new MST benchmark.
func NewBenchmark(d *driver.Driver) *Benchmark {
	b := new(Benchmark)
	b.driver = d
	b.context = d.Init()
	b.loadProgram()
	return b
}

func (b *Benchmark) SelectGPU(gpus []int) { b.gpus = gpus }
func (b *Benchmark) SetUnifiedMemory()    { b.useUnifiedMemory = true }
func (b *Benchmark) SetLASPMemoryAlloc()  { b.useLASPMemoryAlloc = true }

//go:embed kernels.hsaco
var hsacoBytes []byte

func (b *Benchmark) loadProgram() {
	load := func(name string) *insts.HsaCo {
		k := kernels.LoadProgramFromMemory(hsacoBytes, name)
		if k == nil {
			log.Panicf("Failed to load %s", name)
		}
		return k
	}
	b.kernelReset = load("mst_reset")
	b.kernelFindMin = load("mst_find_min")
	b.kernelUnion = load("mst_union")
	b.kernelCompress = load("mst_compress")
}

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

	alloc := func(size uint64) driver.GPUPtr {
		if b.useUnifiedMemory {
			return b.driver.AllocateUnifiedMemory(b.context, size)
		} else if b.useLASPMemoryAlloc {
			return b.driver.AllocateMemoryLASP(b.context, size, "div4")
		}
		return b.driver.AllocateMemory(b.context, size)
	}

	b.rowD = alloc(nodeBytes)
	b.colD = alloc(edgeBytes)
	b.weightD = alloc(edgeBytes)
	b.compD = alloc(nodeBytes)
	b.minWtD = alloc(nodeBytes)
	b.minEdgeD = alloc(nodeBytes)
	b.changedD = alloc(4)
}

func (b *Benchmark) exec() {
	// Upload CSR structure
	b.driver.MemCopyH2D(b.context, b.rowD, b.matrix.RowOffsets)
	b.driver.MemCopyH2D(b.context, b.colD, b.matrix.ColumnNumbers)

	// Generate random int32 weights per edge
	hWeight := make([]int32, b.NumEdges)
	for i := range hWeight {
		hWeight[i] = rand.Int31()
	}
	b.driver.MemCopyH2D(b.context, b.weightD, hWeight)

	// Initialize comp[i] = i
	hComp := make([]int32, b.NumNodes)
	for i := int32(0); i < b.NumNodes; i++ {
		hComp[i] = i
	}
	b.driver.MemCopyH2D(b.context, b.compD, hComp)

	blockSize := int32(64)
	nodeGlobal := b.NumNodes
	if b.NumNodes%blockSize > 0 {
		nodeGlobal = (b.NumNodes/blockSize + 1) * blockSize
	}
	nodeGW := [3]uint32{uint32(nodeGlobal), 1, 1}
	nodeLW := [3]uint16{uint16(blockSize), 1, 1}

	resetArgs := ResetKernelArgs{
		MinWt:    b.minWtD,
		MinEdge:  b.minEdgeD,
		Changed:  b.changedD,
		NumNodes: b.NumNodes,
	}
	findMinArgs := FindMinKernelArgs{
		Row:      b.rowD,
		Col:      b.colD,
		Weight:   b.weightD,
		Comp:     b.compD,
		MinWt:    b.minWtD,
		MinEdge:  b.minEdgeD,
		Changed:  b.changedD,
		NumNodes: b.NumNodes,
		NumEdges: b.NumEdges,
	}
	unionArgs := UnionKernelArgs{
		Col:      b.colD,
		Comp:     b.compD,
		MinWt:    b.minWtD,
		MinEdge:  b.minEdgeD,
		Changed:  b.changedD,
		NumNodes: b.NumNodes,
	}
	compressArgs := CompressKernelArgs{
		Comp:     b.compD,
		Changed:  b.changedD,
		NumNodes: b.NumNodes,
	}

	changed := int32(1)
	for changed != 0 {
		b.driver.LaunchKernel(b.context, b.kernelReset, nodeGW, nodeLW, &resetArgs)
		b.driver.LaunchKernel(b.context, b.kernelFindMin, nodeGW, nodeLW, &findMinArgs)
		b.driver.LaunchKernel(b.context, b.kernelUnion, nodeGW, nodeLW, &unionArgs)
		b.driver.LaunchKernel(b.context, b.kernelCompress, nodeGW, nodeLW, &compressArgs)
		b.driver.MemCopyD2H(b.context, &changed, b.changedD)
	}

	b.hComp = make([]int32, b.NumNodes)
	b.driver.MemCopyD2H(b.context, b.hComp, b.compD)
}

// Verify counts connected components in the GPU result.
// MST correctness (optimal weight) is not checked due to non-atomic stubs.
func (b *Benchmark) Verify() {
	roots := make(map[int32]struct{})
	for _, c := range b.hComp {
		roots[c] = struct{}{}
	}
	log.Printf("MST done. Components found: %d (expect 1 for connected graph)\n", len(roots))
}
