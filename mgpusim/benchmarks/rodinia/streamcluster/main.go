// Package streamcluster implements the Rodinia streamcluster (online
// k-medians) `pgain_kernel`.  For each candidate facility x the kernel
// computes, for every point, the cost gain of opening x as a new center
// vs. closing whichever center the point is currently assigned to.
//
// The original Rodinia host (native/streamcluster.cpp, pFL()) randomly
// shuffles a candidate list and invokes pgain_kernel once per candidate.
// This port mirrors that loop with a configurable -iter knob so the
// simulator workload size is independent of the host scaffolding.
//
// Kernel: native/Kernels.cl
//
// __kernel void pgain_kernel(
//     __global Point_Struct *p,             // {float weight; long assign; float cost;} per point
//     __global float        *coord_d,       // column-major [dim x num] coordinates
//     __global float        *work_mem_d,    // per-point scratch, length num*(K+1)
//     __global int          *center_table_d,// per-point center index
//     __global char         *switch_membership_d, // per-point reassignment flag
//     __local  float        *coord_s,       // dim floats (broadcast of point x's coord)
//     int num, int dim, long x, int K)
//
// Sharing characteristics: every WG broadcasts `coord_d[i*num + x]` for the
// candidate x (read-only sharing), then issues independent reads at
// `coord_d[i*num + tid]` and a scattered RMW into `work_mem_d[base + ct[assign]]`.
// LASP classification: dominant pattern is per-thread streaming over coord_d
// + indirect scatter into work_mem_d -> Row 6/7 mix -> kernel-wide.
package streamcluster

import (
	_ "embed"
	"encoding/binary"
	"log"
	"math"
	"math/rand"

	"gitlab.com/akita/mgpusim/driver"
	"gitlab.com/akita/mgpusim/insts"
	"gitlab.com/akita/mgpusim/kernels"
)

// Point_Struct in Kernels.cl:
//   typedef struct { float weight; long assign; float cost; } Point_Struct;
// With OpenCL alignment (long aligned to 8) the struct occupies 24 bytes:
//   [ 0.. 3] weight (float)
//   [ 4.. 7] padding
//   [ 8..15] assign (long)
//   [16..19] cost   (float)
//   [20..23] tail padding
// We marshal this manually via []byte so the driver's binary.Write path
// (which rejects structs containing unexported padding fields) is happy.
const pointStructSize = 24

// PgainKernelArgs matches the pgain_kernel signature.
//
// IMPORTANT — explicit padding fields:
//   driver.MemCopyH2D marshals this struct via encoding/binary.Write,
//   which serialises fields in declaration order WITHOUT Go's implicit
//   alignment padding.  The AMDGPU kernel ABI, however, expects each
//   scalar at its naturally-aligned offset in the kernarg buffer.  We
//   therefore emit explicit `Padding*` fields wherever Go would
//   otherwise insert implicit padding, so the encoded byte stream
//   matches the kernel-side layout exactly.
//
// Kernel-side offsets (verified against kernels.disasm s_load_* probes):
//   0   P                (8B, GPUPtr)
//   8   Coord            (8B, GPUPtr)
//  16   WorkMem          (8B, GPUPtr)
//  24   CenterTable      (8B, GPUPtr)
//  32   SwitchMembership (8B, GPUPtr)
//  40   CoordS           (4B, __local ptr)
//  44   Num              (4B, int32)
//  48   Dim              (4B, int32)
//  52   Padding to 8-align X
//  56   X                (8B, int64)            <-- s_load_dwordx2 ... 0x38
//  64   K                (4B, int32)            <-- s_load_dword   ... 0x40
//  68   Padding to 8-align Hidden*
//  72   HiddenGlobalOffsetX (8B, int64)         <-- s_load_dwordx2 ... 0x48
//  80   HiddenGlobalOffsetY (8B, int64)
//  88   HiddenGlobalOffsetZ (8B, int64)
//  96   end
type PgainKernelArgs struct {
	P                   driver.GPUPtr
	Coord               driver.GPUPtr
	WorkMem             driver.GPUPtr
	CenterTable         driver.GPUPtr
	SwitchMembership    driver.GPUPtr
	CoordS              driver.LocalPtr // __local float * (Dim floats)
	Num                 int32
	Dim                 int32
	PaddingDimX         int32 // 4B explicit pad: 8-aligns X
	X                   int64
	K                   int32
	PaddingK            int32 // 4B explicit pad: 8-aligns HiddenGlobalOffsetX
	HiddenGlobalOffsetX int64
	HiddenGlobalOffsetY int64
	HiddenGlobalOffsetZ int64
}

// Benchmark holds the streamcluster benchmark state.
type Benchmark struct {
	driver  *driver.Driver
	context *driver.Context
	gpus    []int
	queues  []*driver.CommandQueue

	kernel *insts.HsaCo

	// Configurable parameters set from the sample main.
	Num  int32 // number of points
	Dim  int32 // dimensionality
	K    int32 // number of currently-open centers
	Iter int32 // number of candidate facilities (= kernel launches)

	// Device buffers (allocation order matters for LASP data-partitioning).
	pD                driver.GPUPtr // Point_Struct[Num]
	coordD            driver.GPUPtr // float[Dim * Num]
	workMemD          driver.GPUPtr // float[Num * (K+1)]
	centerTableD      driver.GPUPtr // int32[Num]
	switchMembershipD driver.GPUPtr // char[Num]

	useUnifiedMemory   bool
	useLASPMemoryAlloc bool
}

// NewBenchmark creates a streamcluster benchmark with safe defaults.
func NewBenchmark(d *driver.Driver) *Benchmark {
	b := new(Benchmark)
	b.driver = d
	b.context = d.Init()
	b.Num = 65536
	b.Dim = 32
	b.K = 64
	b.Iter = 16
	b.loadProgram()
	return b
}

// SelectGPU pins this benchmark to the given GPU(s).
func (b *Benchmark) SelectGPU(gpus []int) { b.gpus = gpus }

// SetUnifiedMemory switches to unified memory allocation.
func (b *Benchmark) SetUnifiedMemory() { b.useUnifiedMemory = true }

// SetLASPMemoryAlloc switches to LASP-aware memory allocation.
func (b *Benchmark) SetLASPMemoryAlloc() { b.useLASPMemoryAlloc = true }

//go:embed kernels.hsaco
var hsacoBytes []byte

func (b *Benchmark) loadProgram() {
	b.kernel = kernels.LoadProgramFromMemory(hsacoBytes, "pgain_kernel")
	if b.kernel == nil {
		log.Panic("Failed to load pgain_kernel")
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

func (b *Benchmark) alloc(size uint64) driver.GPUPtr {
	if b.useUnifiedMemory {
		return b.driver.AllocateUnifiedMemory(b.context, size)
	} else if b.useLASPMemoryAlloc {
		return b.driver.AllocateMemoryLASP(b.context, size, "div4")
	}
	return b.driver.AllocateMemory(b.context, size)
}

func (b *Benchmark) initMem() {
	n := int(b.Num)
	d := int(b.Dim)
	k := int(b.K)

	pBytes := uint64(n) * pointStructSize
	coordBytes := uint64(n) * uint64(d) * 4
	workMemBytes := uint64(n) * uint64(k+1) * 4
	centerTableBytes := uint64(n) * 4
	switchMembershipBytes := uint64(n) // char = 1 B

	b.pD = b.alloc(pBytes)
	b.coordD = b.alloc(coordBytes)
	b.workMemD = b.alloc(workMemBytes)
	b.centerTableD = b.alloc(centerTableBytes)
	b.switchMembershipD = b.alloc(switchMembershipBytes)

	rng := rand.New(rand.NewSource(42))

	// pD : Point_Struct[Num] marshalled as raw bytes.
	points := make([]byte, pBytes)
	for i := 0; i < n; i++ {
		off := i * pointStructSize
		binary.LittleEndian.PutUint32(points[off:off+4],
			math.Float32bits(1.0)) // weight
		binary.LittleEndian.PutUint64(points[off+8:off+16],
			uint64(int64(i%k))) // assign
		binary.LittleEndian.PutUint32(points[off+16:off+20],
			math.Float32bits(rng.Float32()*100.0)) // cost
	}
	b.driver.MemCopyH2D(b.context, b.pD, points)

	// coordD : random floats, column-major [dim x num].
	coords := make([]float32, n*d)
	for i := range coords {
		coords[i] = rng.Float32()
	}
	b.driver.MemCopyH2D(b.context, b.coordD, coords)

	// workMemD : zero-init scratch.
	workMem := make([]float32, n*(k+1))
	b.driver.MemCopyH2D(b.context, b.workMemD, workMem)

	// centerTableD : map each point id to (id mod K) as a placeholder.
	centerTable := make([]int32, n)
	for i := range centerTable {
		centerTable[i] = int32(i % k)
	}
	b.driver.MemCopyH2D(b.context, b.centerTableD, centerTable)

	// switchMembershipD : cleared each round.
	switchMembership := make([]byte, n)
	b.driver.MemCopyH2D(b.context, b.switchMembershipD, switchMembership)
}

func (b *Benchmark) exec() {
	blockSize := uint32(256)
	nGlobal := uint32(b.Num)
	if nGlobal%blockSize != 0 {
		nGlobal = ((nGlobal / blockSize) + 1) * blockSize
	}
	gw := [3]uint32{nGlobal, 1, 1}
	lw := [3]uint16{uint16(blockSize), 1, 1}

	lmemSize := uint64(b.Dim) * 4

	args := PgainKernelArgs{
		P:                b.pD,
		Coord:            b.coordD,
		WorkMem:          b.workMemD,
		CenterTable:      b.centerTableD,
		SwitchMembership: b.switchMembershipD,
		CoordS:           driver.LocalPtr(lmemSize),
		Num:              b.Num,
		Dim:              b.Dim,
		K:                b.K,
	}

	// Mirror Rodinia's pFL loop: one launch per candidate facility x.
	for i := int32(0); i < b.Iter; i++ {
		args.X = int64(i) % int64(b.Num)
		b.driver.LaunchKernel(b.context, b.kernel, gw, lw, &args)
	}
}

// Verify is a stub: streamcluster is intended as a sim-only workload here.
// A reference CPU pgain (Rodinia native/streamcluster.cpp::pgain) could be
// transliterated for correctness checking but is not required for memory
// subsystem characterisation.
func (b *Benchmark) Verify() {
	log.Printf("streamcluster: Verify not implemented (sim-only benchmark).")
}
