// Package bc implements the Betweenness Centrality benchmark from the Pannotia suite.
// OpenCL source: https://github.com/pannotia/pannotia (graph_app/bc/kernel/kernel.cl)
//
// Algorithm (Brandes, single-source from vertex 0):
//   Forward pass  — BFS levels to fill d[] (distance) and rho[] (shortest-path count).
//   Backward pass — propagate delta[] dependency scores back toward the source.
//   BC[]          — accumulated over NumSrc sources.
//
// Characteristics: all WGs simultaneously read/write shared rho[], delta[], bc[].
// Irregular edge access through CSR + level-dependent control flow.
package bc

import (
	_ "embed"
	"log"
	"math"

	"gitlab.com/akita/mgpusim/benchmarks/matrix/csr"
	"gitlab.com/akita/mgpusim/driver"
	"gitlab.com/akita/mgpusim/insts"
	"gitlab.com/akita/mgpusim/kernels"
)

// BFSKernelArgs is the argument layout for the forward BFS kernel.
type BFSKernelArgs struct {
	Row                 driver.GPUPtr
	Col                 driver.GPUPtr
	D                   driver.GPUPtr // int32 distance array (-1 = unvisited)
	Rho                 driver.GPUPtr // float32 shortest-path count
	Cont                driver.GPUPtr // int32 continuation flag
	NumNodes            int32
	NumEdges            int32
	Dist                int32 // current BFS depth
	Padding             int32
	HiddenGlobalOffsetX int64
	HiddenGlobalOffsetY int64
	HiddenGlobalOffsetZ int64
}

// BacktrackKernelArgs is the argument layout for the backward accumulation kernel.
type BacktrackKernelArgs struct {
	Row                 driver.GPUPtr
	Col                 driver.GPUPtr
	D                   driver.GPUPtr // int32 distance array
	Rho                 driver.GPUPtr // float32 shortest-path count
	Sigma               driver.GPUPtr // float32 dependency score (delta in Brandes)
	BC                  driver.GPUPtr // float32 betweenness centrality output
	NumNodes            int32
	NumEdges            int32
	Dist                int32 // depth level being processed in backward sweep
	Padding             int32
	HiddenGlobalOffsetX int64
	HiddenGlobalOffsetY int64
	HiddenGlobalOffsetZ int64
}

// CleanIntKernelArgs clears an int32 device array to a given value.
type CleanIntKernelArgs struct {
	Array               driver.GPUPtr
	Val                 int32
	NumElements         int32
	HiddenGlobalOffsetX int64
	HiddenGlobalOffsetY int64
	HiddenGlobalOffsetZ int64
}

// CleanFloatKernelArgs clears a float32 device array to zero.
type CleanFloatKernelArgs struct {
	Array               driver.GPUPtr
	NumElements         int32
	Padding             int32
	HiddenGlobalOffsetX int64
	HiddenGlobalOffsetY int64
	HiddenGlobalOffsetZ int64
}

// Benchmark is the BC benchmark.
type Benchmark struct {
	driver  *driver.Driver
	context *driver.Context
	gpus    []int
	queues  []*driver.CommandQueue

	kernelBFS       *insts.HsaCo
	kernelBacktrack *insts.HsaCo
	kernelCleanInt  *insts.HsaCo
	kernelCleanFloat *insts.HsaCo

	matrix   csr.Matrix
	NumNodes int32
	NumEdges int32
	// NumSrc controls how many source vertices to accumulate BC from.
	// Larger values give better statistical accuracy but take longer.
	NumSrc int32

	// Device buffers
	rowD, colD driver.GPUPtr
	dD         driver.GPUPtr // int32 distance
	rhoD       driver.GPUPtr // float32 sigma (shortest-path count)
	sigmaD     driver.GPUPtr // float32 delta (dependency)
	bcD        driver.GPUPtr // float32 betweenness centrality
	contD      driver.GPUPtr // int32 continuation flag

	// Host result buffer
	hBC []float32

	useUnifiedMemory   bool
	useLASPMemoryAlloc bool
}

// NewBenchmark creates a new BC benchmark.
func NewBenchmark(d *driver.Driver) *Benchmark {
	b := new(Benchmark)
	b.driver = d
	b.context = d.Init()
	b.NumSrc = 1
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
	b.kernelBFS = kernels.LoadProgramFromMemory(hsacoBytes, "bfs_kernel")
	if b.kernelBFS == nil {
		log.Panic("Failed to load bfs_kernel")
	}
	b.kernelBacktrack = kernels.LoadProgramFromMemory(hsacoBytes, "backtrack_kernel")
	if b.kernelBacktrack == nil {
		log.Panic("Failed to load backtrack_kernel")
	}
	b.kernelCleanInt = kernels.LoadProgramFromMemory(hsacoBytes, "clean_1d_array")
	if b.kernelCleanInt == nil {
		log.Panic("Failed to load clean_1d_array")
	}
	b.kernelCleanFloat = kernels.LoadProgramFromMemory(hsacoBytes, "clean_bc")
	if b.kernelCleanFloat == nil {
		log.Panic("Failed to load clean_bc")
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
		b.dD = b.driver.AllocateUnifiedMemory(b.context, nodeBytes)
		b.rhoD = b.driver.AllocateUnifiedMemory(b.context, nodeBytes)
		b.sigmaD = b.driver.AllocateUnifiedMemory(b.context, nodeBytes)
		b.bcD = b.driver.AllocateUnifiedMemory(b.context, nodeBytes)
		b.contD = b.driver.AllocateUnifiedMemory(b.context, 4)
	} else if b.useLASPMemoryAlloc {
		b.rowD = b.driver.AllocateMemoryLASP(b.context, nodeBytes, "div4")
		b.colD = b.driver.AllocateMemoryLASP(b.context, edgeBytes, "div4")
		b.dD = b.driver.AllocateMemoryLASP(b.context, nodeBytes, "div4")
		b.rhoD = b.driver.AllocateMemoryLASP(b.context, nodeBytes, "div4")
		b.sigmaD = b.driver.AllocateMemoryLASP(b.context, nodeBytes, "div4")
		b.bcD = b.driver.AllocateMemoryLASP(b.context, nodeBytes, "div4")
		b.contD = b.driver.AllocateMemoryLASP(b.context, 4, "div4")
	} else {
		b.rowD = b.driver.AllocateMemory(b.context, nodeBytes)
		b.colD = b.driver.AllocateMemory(b.context, edgeBytes)
		b.dD = b.driver.AllocateMemory(b.context, nodeBytes)
		b.rhoD = b.driver.AllocateMemory(b.context, nodeBytes)
		b.sigmaD = b.driver.AllocateMemory(b.context, nodeBytes)
		b.bcD = b.driver.AllocateMemory(b.context, nodeBytes)
		b.contD = b.driver.AllocateMemory(b.context, 4)
	}

	// Copy static graph structure
	b.driver.MemCopyH2D(b.context, b.rowD, b.matrix.RowOffsets)
	b.driver.MemCopyH2D(b.context, b.colD, b.matrix.ColumnNumbers)

	// Initialize BC to zero (accumulated across all sources)
	hBC := make([]float32, b.NumNodes)
	b.driver.MemCopyH2D(b.context, b.bcD, hBC)
}

func (b *Benchmark) exec() {
	blockSize := int32(64)
	nodeGlobal := b.NumNodes
	if b.NumNodes%blockSize > 0 {
		nodeGlobal = (b.NumNodes/blockSize + 1) * blockSize
	}
	nodeGW := [3]uint32{uint32(nodeGlobal), 1, 1}
	nodeLW := [3]uint16{uint16(blockSize), 1, 1}

	for src := int32(0); src < b.NumSrc; src++ {
		// --- clean d[], rho[], sigma[] ---
		cleanIntArgs := CleanIntKernelArgs{
			Array:       b.dD,
			Val:         -1,
			NumElements: b.NumNodes,
		}
		b.driver.LaunchKernel(b.context, b.kernelCleanInt, nodeGW, nodeLW, &cleanIntArgs)

		cleanFloatRhoArgs := CleanFloatKernelArgs{
			Array:       b.rhoD,
			NumElements: b.NumNodes,
		}
		b.driver.LaunchKernel(b.context, b.kernelCleanFloat, nodeGW, nodeLW, &cleanFloatRhoArgs)

		cleanFloatSigmaArgs := CleanFloatKernelArgs{
			Array:       b.sigmaD,
			NumElements: b.NumNodes,
		}
		b.driver.LaunchKernel(b.context, b.kernelCleanFloat, nodeGW, nodeLW, &cleanFloatSigmaArgs)

		// Initialize source: d[src] = 0, rho[src] = 1.0
		dSrc := int32(0)
		rhoSrc := float32(1.0)
		b.driver.MemCopyH2D(b.context, b.dD+driver.GPUPtr(src*4), &dSrc)
		b.driver.MemCopyH2D(b.context, b.rhoD+driver.GPUPtr(src*4), &rhoSrc)

		// --- forward BFS ---
		maxDist := int32(0)
		cont := int32(1)
		bfsArgs := BFSKernelArgs{
			Row:      b.rowD,
			Col:      b.colD,
			D:        b.dD,
			Rho:      b.rhoD,
			Cont:     b.contD,
			NumNodes: b.NumNodes,
			NumEdges: b.NumEdges,
		}
		for cont != 0 {
			cont = 0
			b.driver.MemCopyH2D(b.context, b.contD, &cont)
			bfsArgs.Dist = maxDist
			b.driver.LaunchKernel(b.context, b.kernelBFS, nodeGW, nodeLW, &bfsArgs)
			b.driver.MemCopyD2H(b.context, &cont, b.contD)
			if cont != 0 {
				maxDist++
			}
		}

		// --- backward pass ---
		btArgs := BacktrackKernelArgs{
			Row:      b.rowD,
			Col:      b.colD,
			D:        b.dD,
			Rho:      b.rhoD,
			Sigma:    b.sigmaD,
			BC:       b.bcD,
			NumNodes: b.NumNodes,
			NumEdges: b.NumEdges,
		}
		for d := maxDist; d >= 0; d-- {
			btArgs.Dist = d
			b.driver.LaunchKernel(b.context, b.kernelBacktrack, nodeGW, nodeLW, &btArgs)
		}
	}

	// Copy result back
	b.hBC = make([]float32, b.NumNodes)
	b.driver.MemCopyD2H(b.context, b.hBC, b.bcD)
}

// Verify runs Brandes BC on the CPU for source vertex 0 and compares.
func (b *Benchmark) Verify() {
	cpuBC := b.cpuBC()

	passed := true
	for i := int32(0); i < b.NumNodes; i++ {
		diff := math.Abs(float64(b.hBC[i]-cpuBC[i]))
		if diff > 1e-3 {
			log.Printf("mismatch at node %d: GPU %.4f CPU %.4f\n", i, b.hBC[i], cpuBC[i])
			passed = false
		}
	}
	if passed {
		log.Printf("BC Passed!\n")
	} else {
		log.Panic("BC verification failed")
	}
}

// cpuBC computes betweenness centrality on the CPU (Brandes algorithm, source=0).
func (b *Benchmark) cpuBC() []float32 {
	n := int(b.NumNodes)
	row := b.matrix.RowOffsets
	col := b.matrix.ColumnNumbers

	bc := make([]float32, n)
	sigma := make([]float32, n)
	d := make([]int32, n)
	delta := make([]float32, n)
	stack := make([]int, 0, n)
	pred := make([][]int, n)

	for s := 0; s < int(b.NumSrc); s++ {
		for i := range sigma {
			sigma[i] = 0
			d[i] = -1
			delta[i] = 0
			pred[i] = pred[i][:0]
		}
		sigma[s] = 1
		d[s] = 0
		stack = stack[:0]

		queue := []int{s}
		for len(queue) > 0 {
			v := queue[0]
			queue = queue[1:]
			stack = append(stack, v)
			for e := row[v]; e < row[v+1]; e++ {
				w := int(col[e])
				if d[w] < 0 {
					queue = append(queue, w)
					d[w] = d[v] + 1
				}
				if d[w] == d[v]+1 {
					sigma[w] += sigma[v]
					pred[w] = append(pred[w], v)
				}
			}
		}

		for len(stack) > 0 {
			w := stack[len(stack)-1]
			stack = stack[:len(stack)-1]
			for _, v := range pred[w] {
				delta[v] += (sigma[v] / sigma[w]) * (1 + delta[w])
			}
			if w != s {
				bc[w] += delta[w]
			}
		}
	}
	return bc
}
