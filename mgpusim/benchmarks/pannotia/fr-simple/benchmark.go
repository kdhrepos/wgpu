// Package frsimple implements fr-simple: a compute-thinned memory-access
// proxy for the Friend Recommendation (fr) benchmark.
//
// fr's cross-chiplet sharing comes from reading the adjacency list N(w) of
// each 1-hop neighbor w of the source vertex u (a hub w is a neighbor of
// many u's, so N(w) is read by many chiplets). fr's O(deg^3) cost is
// dominated by the 2-hop set-intersection, which is cold private traffic
// irrelevant to a memory-system
// comparison but starves the instruction budget so the sharing never
// materialises within a feasible simulation window.
//
// fr-simple keeps the hub-sharing 2-hop traversal (reading N(w) for every
// neighbor w of u) and drops the intersection, cutting per-vertex cost
// from O(deg^3) to O(deg^2). The memory footprint and sharing distribution
// match fr's; only the redundant arithmetic is removed. See
// native/kernel/frsimple_kernel.cl.
package frsimple

import (
	_ "embed"
	"log"
	"sort"

	"gitlab.com/akita/mgpusim/benchmarks/matrix/csr"
	"gitlab.com/akita/mgpusim/driver"
	"gitlab.com/akita/mgpusim/insts"
	"gitlab.com/akita/mgpusim/kernels"
)

// FRSimpleKernelArgs is the argument layout for frsimple_kernel. It mirrors
// fr's FRKernelArgs (same 4 buffers) so LASP data-partitioning tokens match.
type FRSimpleKernelArgs struct {
	Row                 driver.GPUPtr
	Col                 driver.GPUPtr
	FrBest              driver.GPUPtr
	FrScore             driver.GPUPtr
	NumNodes            int32
	NumEdges            int32
	HiddenGlobalOffsetX int64
	HiddenGlobalOffsetY int64
	HiddenGlobalOffsetZ int64
}

// Benchmark is the fr-simple benchmark.
type Benchmark struct {
	driver  *driver.Driver
	context *driver.Context
	gpus    []int
	queues  []*driver.CommandQueue
	kernel  *insts.HsaCo

	matrix   csr.Matrix
	NumNodes int32
	NumEdges int32

	rowD, colD driver.GPUPtr
	frBestD    driver.GPUPtr
	frScoreD   driver.GPUPtr

	hFrBest  []int32
	hFrScore []int32

	useUnifiedMemory   bool
	useLASPMemoryAlloc bool
}

// NewBenchmark creates a new fr-simple benchmark.
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
	b.kernel = kernels.LoadProgramFromMemory(hsacoBytes, "frsimple_kernel")
	if b.kernel == nil {
		log.Panic("Failed to load frsimple_kernel")
	}
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
		b.frBestD = b.driver.AllocateUnifiedMemory(b.context, nodeBytes)
		b.frScoreD = b.driver.AllocateUnifiedMemory(b.context, nodeBytes)
	} else if b.useLASPMemoryAlloc {
		b.rowD = b.driver.AllocateMemoryLASP(b.context, nodeBytes, "div4")
		b.colD = b.driver.AllocateMemoryLASP(b.context, edgeBytes, "div4")
		b.frBestD = b.driver.AllocateMemoryLASP(b.context, nodeBytes, "div4")
		b.frScoreD = b.driver.AllocateMemoryLASP(b.context, nodeBytes, "div4")
	} else {
		b.rowD = b.driver.AllocateMemory(b.context, nodeBytes)
		b.colD = b.driver.AllocateMemory(b.context, edgeBytes)
		b.frBestD = b.driver.AllocateMemory(b.context, nodeBytes)
		b.frScoreD = b.driver.AllocateMemory(b.context, nodeBytes)
	}
}

func (b *Benchmark) exec() {
	b.driver.MemCopyH2D(b.context, b.rowD, b.matrix.RowOffsets)
	b.driver.MemCopyH2D(b.context, b.colD, b.matrix.ColumnNumbers)

	hBestInit := make([]int32, b.NumNodes)
	hScoreInit := make([]int32, b.NumNodes)
	for i := range hBestInit {
		hBestInit[i] = -1
	}
	b.driver.MemCopyH2D(b.context, b.frBestD, hBestInit)
	b.driver.MemCopyH2D(b.context, b.frScoreD, hScoreInit)

	blockSize := int32(64)
	globalSize := b.NumNodes
	if b.NumNodes%blockSize > 0 {
		globalSize = (b.NumNodes/blockSize + 1) * blockSize
	}

	args := FRSimpleKernelArgs{
		Row:      b.rowD,
		Col:      b.colD,
		FrBest:   b.frBestD,
		FrScore:  b.frScoreD,
		NumNodes: b.NumNodes,
		NumEdges: b.NumEdges,
	}
	b.driver.LaunchKernel(b.context, b.kernel,
		[3]uint32{uint32(globalSize), 1, 1},
		[3]uint16{uint16(blockSize), 1, 1},
		&args)

	b.hFrBest = make([]int32, b.NumNodes)
	b.hFrScore = make([]int32, b.NumNodes)
	b.driver.MemCopyD2H(b.context, b.hFrBest, b.frBestD)
	b.driver.MemCopyD2H(b.context, b.hFrScore, b.frScoreD)
}

// Verify checks the GPU output against a sequential CPU implementation of
// the same O(deg^2) 2-hop traversal.
func (b *Benchmark) Verify() {
	cpuBest, cpuScore := b.cpuFRSimple()
	mismatch := 0
	for u := int32(0); u < b.NumNodes; u++ {
		if b.hFrScore[u] != cpuScore[u] || b.hFrBest[u] != cpuBest[u] {
			mismatch++
			if mismatch <= 5 {
				log.Printf("fr-simple mismatch u=%d: GPU(%d,%d) CPU(%d,%d)",
					u, b.hFrBest[u], b.hFrScore[u], cpuBest[u], cpuScore[u])
			}
		}
	}
	if mismatch > 0 {
		log.Panicf("fr-simple Verify FAILED: %d mismatches", mismatch)
	}
	log.Println("fr-simple Passed!")
}

func (b *Benchmark) cpuFRSimple() (best, score []int32) {
	row := b.matrix.RowOffsets
	col := b.matrix.ColumnNumbers
	n := int(b.NumNodes)

	best = make([]int32, n)
	score = make([]int32, n)

	for u := uint32(0); u < uint32(n); u++ {
		bb := int32(-1)
		var sc int32
		for wi := row[u]; wi < row[u+1]; wi++ {
			w := col[wi]
			for vi := row[w]; vi < row[w+1]; vi++ {
				v := col[vi]
				if v == u {
					continue
				}
				sc++
				if int32(v) > bb {
					bb = int32(v)
				}
			}
		}
		best[u] = bb
		score[u] = sc
	}
	return
}
