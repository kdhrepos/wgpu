// Package bptree implements Rodinia's B+ tree findK benchmark.
//
// One work-group per query, DEFAULT_ORDER (32) work-items per work-group.
// At each tree level the work-group does an indirect lookup through the
// knode array — same access shape as SpMV's x[col_idx[j]] but on a tree.
package bptree

import (
	_ "embed"
	"log"
	"math"
	"math/rand/v2"

	"gitlab.com/akita/mgpusim/driver"
	"gitlab.com/akita/mgpusim/insts"
	"gitlab.com/akita/mgpusim/kernels"
)

// DefaultOrder matches DEFAULT_ORDER in findK_kernel.cl. Order 32 (was
// 64/256) deepens the tree so each query traverses more internal nodes,
// and — since the index (non-leaf) footprint is ~1/order^2 of the tree —
// a smaller fan-out GROWS the hot+shared band (~4x more index knodes from
// order 64 to 32), surfacing more cross-chiplet fan-in in the miss view.
const DefaultOrder = 32

// KnodeStructSize is the exact byte size of the OpenCL `knode` struct.
//   location(4) + indices[33](132) + keys[33](132) + is_leaf(4) + num_keys(4)
const KnodeStructSize = uint64(276)

// Knode mirrors the OpenCL `knode` struct verbatim (all fields exported so
// driver.MemCopyH2D can copy raw bytes).
type Knode struct {
	Location int32
	Indices  [DefaultOrder + 1]int32
	Keys     [DefaultOrder + 1]int32
	IsLeaf   int32
	NumKeys  int32
}

// KnodeStrideLog2 mirrors KNODE_STRIDE_LOG2 in findK_kernel.cl.
// Per-knode stride in each flat array = 1 << KnodeStrideLog2 ints.
// 2^6 = 64 ints = 256 bytes — the smallest power of two that fits
// the 33 active key/index slots per knode (order=32).  Pow-of-2 stride
// lets the compiler use shifts instead of an integer multiply (which
// would otherwise lower to `v_mad_i64_i32` and fail under the GCN3
// functional emulator).
const KnodeStrideLog2 = 6
const KnodeStride = 1 << KnodeStrideLog2 // 64 ints

// FindKKernelArgs matches the flat-array variant of findK in
// native/kernel/findK_kernel.cl.  Sized to the kernel binary's
// KernargSegmentByteSize of 120 bytes:
//   64 B  explicit args (7 ptrs + Height + KnodesElem)
//   24 B  HiddenGlobalOffsetX/Y/Z
//   32 B  additional implicit args emitted by clang-ocl 4.x for
//         gfx803: HiddenPrintfBuffer, HiddenDefaultQueue,
//         HiddenCompletionAction, HiddenMultigridSyncArg (4 × 8 B).
//         Left zero-initialised (Go zero value) — the findK kernel
//         does not actually use any of these, but the kernarg layout
//         still needs them present so the buffer is the exact size
//         the runtime expects.
type FindKKernelArgs struct {
	KnodesKeys    driver.GPUPtr // flat int32[NumKnodes * KnodeStride]
	KnodesIndices driver.GPUPtr // flat int32[NumKnodes * KnodeStride]
	Records       driver.GPUPtr
	CurrKnode     driver.GPUPtr
	Offset        driver.GPUPtr
	Keys          driver.GPUPtr
	Ans           driver.GPUPtr
	Height        int32
	KnodesElem    int32

	HiddenGlobalOffsetX     int64
	HiddenGlobalOffsetY     int64
	HiddenGlobalOffsetZ     int64
	HiddenPrintfBuffer      int64
	HiddenDefaultQueue      int64
	HiddenCompletionAction  int64
	HiddenMultigridSyncArg  int64
}

// Benchmark is the b+tree benchmark.
type Benchmark struct {
	driver  *driver.Driver
	context *driver.Context
	gpus    []int
	queues  []*driver.CommandQueue
	kernel  *insts.HsaCo

	// Workload size.
	Size  int     // number of records (leaf keys)
	Count int     // number of queries
	ZipfS float64 // query key skew: <=1 -> uniform (default); >1 -> Zipfian s.

	// Tree state (host).
	height   int32
	hKnodes  []Knode
	hRecords []int32
	hKeys    []int32
	hAns     []int32

	// Device buffers.
	knodesKeysD    driver.GPUPtr // flat int32[NumKnodes * KnodeStride]
	knodesIndicesD driver.GPUPtr // flat int32[NumKnodes * KnodeStride]
	recordsD       driver.GPUPtr
	currKnodeD     driver.GPUPtr
	offsetD        driver.GPUPtr
	keysD          driver.GPUPtr
	ansD           driver.GPUPtr

	useUnifiedMemory   bool
	useLASPMemoryAlloc bool
}

//go:embed kernels.hsaco
var hsacoBytes []byte

// NewBenchmark creates a new b+tree benchmark.
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
	b.kernel = kernels.LoadProgramFromMemory(hsacoBytes, "findK")
	if b.kernel == nil {
		log.Panic("Failed to load findK kernel")
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
	if b.Size <= 0 || b.Count <= 0 {
		log.Panicf("bptree: Size=%d, Count=%d must be positive", b.Size, b.Count)
	}

	// 1. Build a balanced B+ tree in BFS order.
	b.hKnodes, b.hRecords, b.height = buildSyntheticTree(b.Size, DefaultOrder)

	// 2. Prepare query buffers. Key range and Verify() semantics are the same
	// for both distributions: keys stay in [0,(Size-1)*10]. Seed fixed (42).
	b.hKeys = make([]int32, b.Count)
	b.hAns = make([]int32, b.Count)
	rng := rand.New(rand.NewPCG(42, 0))
	if b.ZipfS > 1.0 {
		// Skewed keys (opt-in, s>1): rank r frequency ∝ 1/(r+1)^s. Traffic
		// concentrates on the low-rank apex path, which then fits in and
		// caches at L2 — the L1 band never surfaces as shared L2 misses.
		// Kept only for skew studies; NOT the default.
		zipf := rand.NewZipf(rng, b.ZipfS, 1.0, uint64(b.Size-1))
		for i := 0; i < b.Count; i++ {
			b.hKeys[i] = int32(zipf.Uint64()) * 10
			b.hAns[i] = -1
		}
	} else {
		// Uniform keys (default, s<=1.0): each query draws an independent
		// uniform rank, so the ~size/order^2 L1-band knodes (for size=64M,
		// order=64: ~15.6K knodes = 15.5 MB > 4 MB L2) are each hit
		// ~Count/bandKnodes times by queries scattered across the whole
		// stream — so every band knode is touched by ~all chiplets. That
		// band is hot + cross-chiplet-shared AND L2-overflowing (sustained
		// SHARED misses), which is the high-sharing/high-reuse regime we
		// want to induce in the L2-miss view.
		for i := 0; i < b.Count; i++ {
			b.hKeys[i] = int32(rng.IntN(b.Size)) * 10
			b.hAns[i] = -1
		}
	}

	// 3. Allocate device memory.
	numKnodes := len(b.hKnodes)
	flatKnodeInts := uint64(numKnodes) * uint64(KnodeStride)
	flatBytes := flatKnodeInts * 4
	recordsBytes := uint64(len(b.hRecords)) * 4
	queryBytes := uint64(b.Count) * 4

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

	// Allocation order matters for the LASP per-buffer data-partitioning
	// in scripts/params.py.  Order: <guard>, knodes_keys, knodes_indices,
	// records, currKnode, offset, keys, ans  (8 tokens; 1 guard + 7 real).
	//
	// Dummy page-0 guard: consume the first vAddr page (0..0xfff) so no
	// real buffer lands at vAddr 0.  Without this, the address-converter
	// occasionally receives requests with external offsets inside page 0
	// (e.g. 0xfc0, 0xff8) that fail the "external >= Offset" invariant in
	// mem/idealmemcontroller/addressconverter.go.  Almost certainly a
	// cache/prefetch path on the simulator side, but per owner directive
	// we patch this benchmark-side, not the simulator.
	_ = alloc(uint64(1 << 12))
	b.knodesKeysD = alloc(flatBytes)
	b.knodesIndicesD = alloc(flatBytes)
	b.recordsD = alloc(recordsBytes)
	b.currKnodeD = alloc(queryBytes)
	b.offsetD = alloc(queryBytes)
	b.keysD = alloc(queryBytes)
	b.ansD = alloc(queryBytes)
}

func (b *Benchmark) exec() {
	// Diagnostic print — REMOVE once panic is resolved.
	// Helps match panic "external=0x???" to the buffer/offset that
	// triggered it.  Also confirms kernarg-segment size matches our
	// struct (catches ABI-padding regressions).
	log.Printf("bptree.diag KernargSegmentByteSize=%d",
		b.kernel.KernargSegmentByteSize)
	log.Printf("bptree.diag alloc map:")
	log.Printf("  knodesKeysD    = 0x%x", uint64(b.knodesKeysD))
	log.Printf("  knodesIndicesD = 0x%x", uint64(b.knodesIndicesD))
	log.Printf("  recordsD       = 0x%x", uint64(b.recordsD))
	log.Printf("  currKnodeD     = 0x%x", uint64(b.currKnodeD))
	log.Printf("  offsetD        = 0x%x", uint64(b.offsetD))
	log.Printf("  keysD          = 0x%x", uint64(b.keysD))
	log.Printf("  ansD           = 0x%x", uint64(b.ansD))

	// Flatten the host-side Knode struct array into two int32 flat
	// arrays with power-of-2 per-knode stride.  Active slots 0..DefaultOrder
	// copy the corresponding fields; padding slots DefaultOrder+1..(KnodeStride-1)
	// are filled with sentinel values that match the existing per-knode
	// padding semantics (keys = MaxInt32 "above any key", indices = 0).
	numKnodes := len(b.hKnodes)
	flatKeys := make([]int32, numKnodes*KnodeStride)
	flatIndices := make([]int32, numKnodes*KnodeStride)
	for i := range flatKeys {
		flatKeys[i] = math.MaxInt32
		// flatIndices already 0-initialised.
	}
	for k := 0; k < numKnodes; k++ {
		base := k * KnodeStride
		for i := 0; i < DefaultOrder+1; i++ {
			flatKeys[base+i] = b.hKnodes[k].Keys[i]
			flatIndices[base+i] = b.hKnodes[k].Indices[i]
		}
	}

	// H2D for static buffers.
	b.driver.MemCopyH2D(b.context, b.knodesKeysD, flatKeys)
	b.driver.MemCopyH2D(b.context, b.knodesIndicesD, flatIndices)
	b.driver.MemCopyH2D(b.context, b.recordsD, b.hRecords)
	b.driver.MemCopyH2D(b.context, b.keysD, b.hKeys)
	b.driver.MemCopyH2D(b.context, b.ansD, b.hAns)

	// Each query starts at the root (knode 0).
	zeros := make([]int32, b.Count)
	b.driver.MemCopyH2D(b.context, b.currKnodeD, zeros)
	b.driver.MemCopyH2D(b.context, b.offsetD, zeros)

	args := FindKKernelArgs{
		KnodesKeys:    b.knodesKeysD,
		KnodesIndices: b.knodesIndicesD,
		Records:       b.recordsD,
		CurrKnode:     b.currKnodeD,
		Offset:        b.offsetD,
		Keys:          b.keysD,
		Ans:           b.ansD,
		Height:        b.height,
		KnodesElem:    int32(numKnodes),
	}

	localSize := [3]uint16{uint16(DefaultOrder), 1, 1}
	globalSize := [3]uint32{uint32(b.Count) * uint32(DefaultOrder), 1, 1}

	b.driver.LaunchKernel(b.context, b.kernel, globalSize, localSize, &args)

	// D2H.
	b.driver.MemCopyD2H(b.context, b.hAns, b.ansD)
}

// Verify runs the same probe sequentially on the host and compares.
func (b *Benchmark) Verify() {
	mismatches := 0
	for i := 0; i < b.Count; i++ {
		key := b.hKeys[i]
		want := int32(-1)
		// records[j] = j*10 (see buildSyntheticTree). The tree stores keys
		// equal to records[j], indices pointing back to record j. So a
		// probe with key=j*10 must return value=j*10.
		if int(key)/10 >= 0 && int(key)/10 < b.Size {
			want = key // records[j*10/10] == j*10
		}
		if b.hAns[i] != want {
			if mismatches < 5 {
				log.Printf("bptree: query %d key=%d got %d want %d\n",
					i, key, b.hAns[i], want)
			}
			mismatches++
		}
	}
	if mismatches != 0 {
		log.Panicf("bptree: %d/%d queries mismatched", mismatches, b.Count)
	}
	log.Printf("bptree Passed! %d queries\n", b.Count)
}

// ----------------------------------------------------------------------------
// Synthetic tree builder. Produces a balanced B+ tree in BFS order so that
// every node's children indices are contiguous in the resulting array.
// ----------------------------------------------------------------------------

type syntheticNode struct {
	isLeaf   bool
	keys     []int32
	children []*syntheticNode
}

// subtreeMin returns the smallest key in n's subtree = the first key of the
// leftmost descendant leaf. B+ tree separators must be the subtree minimum of
// the right child, NOT child.keys[0] (which, for an interior child, is that
// child's first SEPARATOR = the min of its *second* grandchild, skipping the
// first grandchild's whole range).
func subtreeMin(n *syntheticNode) int32 {
	for !n.isLeaf {
		n = n.children[0]
	}
	return n.keys[0]
}

// buildSyntheticTree constructs a B+ tree over `size` records (record j has
// value j*10, key j*10). Returns the BFS-flattened knode array, the records
// array, and the number of interior levels (tree height).
func buildSyntheticTree(size, order int) ([]Knode, []int32, int32) {
	// Records: record j stores j*10. Key == value for easy verification.
	records := make([]int32, size)
	for i := range records {
		records[i] = int32(i * 10)
	}

	leafCapacity := order - 1
	if leafCapacity <= 0 {
		log.Panic("bptree: order must be >= 2")
	}

	// Leaves.
	leaves := make([]*syntheticNode, 0, (len(records)+leafCapacity-1)/leafCapacity)
	for start := 0; start < len(records); start += leafCapacity {
		end := start + leafCapacity
		if end > len(records) {
			end = len(records)
		}
		keys := make([]int32, end-start)
		copy(keys, records[start:end])
		leaves = append(leaves, &syntheticNode{isLeaf: true, keys: keys})
	}

	// Build interior levels bottom-up.
	levels := 0
	currentLevel := leaves
	for len(currentLevel) > 1 {
		nextLevel := make([]*syntheticNode, 0, (len(currentLevel)+order-1)/order)
		for start := 0; start < len(currentLevel); start += order {
			end := start + order
			if end > len(currentLevel) {
				end = len(currentLevel)
			}
			children := currentLevel[start:end]
			seps := make([]int32, 0, len(children)-1)
			for i := 1; i < len(children); i++ {
				seps = append(seps, subtreeMin(children[i]))
			}
			nextLevel = append(nextLevel, &syntheticNode{
				keys:     seps,
				children: children,
			})
		}
		currentLevel = nextLevel
		levels++
	}

	// BFS flatten.
	root := currentLevel[0]
	queue := []*syntheticNode{root}
	flat := make([]*syntheticNode, 0)
	for len(queue) > 0 {
		n := queue[0]
		queue = queue[1:]
		flat = append(flat, n)
		if !n.isLeaf {
			queue = append(queue, n.children...)
		}
	}

	location := make(map[*syntheticNode]int32, len(flat))
	for i, n := range flat {
		location[n] = int32(i)
	}

	// Emit knode array.
	knodes := make([]Knode, len(flat))
	recordIdx := 0
	for i, n := range flat {
		k := &knodes[i]
		k.Location = int32(i)

		// Sentinels: keys[0] = -INF, keys[actualKeys+1..] = +INF.
		for j := range k.Keys {
			k.Keys[j] = math.MaxInt32
		}
		k.Keys[0] = math.MinInt32

		if n.isLeaf {
			actualKeys := len(n.keys)
			k.IsLeaf = 1
			k.NumKeys = int32(actualKeys + 2)
			for j, key := range n.keys {
				k.Keys[j+1] = key
				k.Indices[j+1] = int32(recordIdx)
				recordIdx++
			}
		} else {
			childCount := len(n.children)
			actualKeys := childCount - 1
			k.IsLeaf = 0
			k.NumKeys = int32(actualKeys + 2)
			for j, key := range n.keys {
				k.Keys[j+1] = key
			}
			for j, child := range n.children {
				k.Indices[j] = location[child]
			}
		}
	}

	return knodes, records, int32(levels)
}
