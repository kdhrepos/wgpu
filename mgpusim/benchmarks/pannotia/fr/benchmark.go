// Package fr implements the Friend Recommendation benchmark from Pannotia.
//
// For each vertex u, the kernel finds the non-adjacent vertex v with the
// highest number of common friends (|N(u) ∩ N(v)|). This is a 2-hop
// neighbourhood expansion followed by a sorted-list intersection, giving
// similar irregular memory-access patterns to Triangle Counting but with an
// additional adjacency-check (binary search) per candidate.
package fr

import (
	_ "embed"
	"log"
	"sort"

	"gitlab.com/akita/mgpusim/benchmarks/matrix/csr"
	"gitlab.com/akita/mgpusim/driver"
	"gitlab.com/akita/mgpusim/insts"
	"gitlab.com/akita/mgpusim/kernels"
)

// FRKernelArgs is the argument layout for fr_kernel.
type FRKernelArgs struct {
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

// Benchmark is the Friend Recommendation benchmark.
type Benchmark struct {
	driver  *driver.Driver
	context *driver.Context
	gpus    []int
	queues  []*driver.CommandQueue
	kernel  *insts.HsaCo

	matrix   csr.Matrix
	NumNodes int32
	NumEdges int32

	rowD, colD   driver.GPUPtr
	frBestD      driver.GPUPtr
	frScoreD     driver.GPUPtr

	hFrBest  []int32
	hFrScore []int32

	useUnifiedMemory   bool
	useLASPMemoryAlloc bool
}

// NewBenchmark creates a new Friend Recommendation benchmark.
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
	b.kernel = kernels.LoadProgramFromMemory(hsacoBytes, "fr_kernel")
	if b.kernel == nil {
		log.Panic("Failed to load fr_kernel")
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
		globalSize = (b.NumNodes/blockSize+1) * blockSize
	}

	args := FRKernelArgs{
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

// Verify checks GPU output against a sequential CPU implementation.
func (b *Benchmark) Verify() {
	cpuBest, cpuScore := b.cpuFR()
	mismatch := 0
	for u := int32(0); u < b.NumNodes; u++ {
		if b.hFrScore[u] != cpuScore[u] || b.hFrBest[u] != cpuBest[u] {
			mismatch++
			if mismatch <= 5 {
				log.Printf("FR mismatch u=%d: GPU(%d,%d) CPU(%d,%d)",
					u, b.hFrBest[u], b.hFrScore[u], cpuBest[u], cpuScore[u])
			}
		}
	}
	if mismatch > 0 {
		log.Panicf("FR Verify FAILED: %d mismatches", mismatch)
	}
	log.Println("FR Passed!")
}

func (b *Benchmark) cpuFR() (bestV, bestScore []int32) {
	row := b.matrix.RowOffsets
	col := b.matrix.ColumnNumbers
	n := int(b.NumNodes)

	bestV = make([]int32, n)
	bestScore = make([]int32, n)
	for i := range bestV {
		bestV[i] = -1
	}

	isAdj := func(u, v uint32) bool {
		lo, hi := row[u], row[u+1]
		for lo < hi {
			mid := (lo + hi) / 2
			c := col[mid]
			if c == v {
				return true
			} else if c < v {
				lo = mid + 1
			} else {
				hi = mid
			}
		}
		return false
	}

	countCommon := func(u, v uint32) int32 {
		var score int32
		pi, pj := row[u], row[v]
		for pi < row[u+1] && pj < row[v+1] {
			a, bb := col[pi], col[pj]
			if a == bb {
				score++
				pi++
				pj++
			} else if a < bb {
				pi++
			} else {
				pj++
			}
		}
		return score
	}

	for u := uint32(0); u < uint32(n); u++ {
		for wi := row[u]; wi < row[u+1]; wi++ {
			w := col[wi]
			for vi := row[w]; vi < row[w+1]; vi++ {
				v := col[vi]
				if v == u || isAdj(u, v) {
					continue
				}
				score := countCommon(u, v)
				if score > bestScore[u] || (score == bestScore[u] && (bestV[u] < 0 || int32(v) < bestV[u])) {
					bestScore[u] = score
					bestV[u] = int32(v)
				}
			}
		}
	}
	return
}
