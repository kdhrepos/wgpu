// Package xsbench implements the XSBench Monte Carlo macroscopic cross section
// lookup proxy app (https://github.com/ANL-CESAR/XSBench), event-based method.
//
// The OpenCL device kernel (kernel.cl) is taken verbatim from the upstream
// opencl/ port. The host side (grid construction, materials, verification) is
// re-implemented here in Go, mirroring opencl/GridInit.c, Materials.c and
// Simulation.c.
//
// Characteristics: each work item performs one macroscopic XS lookup, which is
// a binary search on a large unionized energy grid followed by several indirect
// per-nuclide reads. The access pattern is irregular pointer-chasing over a
// large, mostly read-only footprint — the reason XSBench is used as a NUMA /
// remote-memory stress workload.
package xsbench

import (
	_ "embed"
	"log"
	"sort"

	"gitlab.com/akita/mgpusim/driver"
	"gitlab.com/akita/mgpusim/insts"
	"gitlab.com/akita/mgpusim/kernels"
)

//go:embed kernel.hsaco
var hsacoBytes []byte

// Grid types (match kernel.cl).
const (
	gridUnionized = 0
	gridNuclide   = 1
	gridHash      = 2
)

const startingSeed = 1070

// LCG parameters (match kernel.cl / Simulation.c).
const (
	lcgM = uint64(9223372036854775808) // 2^63
	lcgA = uint64(2806196910506780709)
	lcgC = uint64(1)
)

// NuclideGridPoint mirrors the kernel struct. The two trailing Pad fields
// inflate the stride to 32 B (power of two) so the compiler addresses array
// elements with a shift instead of a 64-bit MAD — the i*24 MADs the emu
// mis-executes were the root of the "address smaller than offset" panics
// (same family as bptree's pre-fix bug). FP32 throughout because MGPUSim's
// emu does not implement the FP64 divide opcodes.
type NuclideGridPoint struct {
	Energy       float32
	TotalXS      float32
	ElasticXS    float32
	AbsorptionXS float32
	FissionXS    float32
	NuFissionXS  float32
	Pad0         float32
	Pad1         float32
}

// inputs mirrors the OpenCL "Inputs" struct passed by value as kernel arg 0.
// Pad0 is exported (not a blank `_`) because encoding/binary.Write skips
// unexported fields — leaving the long fields below mis-aligned vs the
// OpenCL ABI and shifting every subsequent kernarg by 4 bytes.
type inputs struct {
	NThreads         int32
	Pad0             int32
	NIsotopes        int64
	NGridpoints      int64
	Lookups          int32
	HM               int32
	GridType         int32
	HashBins         int32
	Particles        int32
	SimulationMethod int32
	BinaryMode       int32
	KernelID         int32
	PlatformID       int32
	DeviceID         int32
}

// KernelArgs is the kernarg layout for macro_xs_lookup_kernel. The blank field
// 8-aligns the pointer block after the int; the trailing three fields are the
// HSA hidden global-offset arguments.
type KernelArgs struct {
	In                   inputs
	MaxNumNucs           int32
	Padding              int32
	NumNucs              driver.GPUPtr
	Concs                driver.GPUPtr
	UnionizedEnergyArray driver.GPUPtr
	IndexGrid            driver.GPUPtr
	NuclideGrid          driver.GPUPtr
	Mats                 driver.GPUPtr
	VerificationArray    driver.GPUPtr
	HiddenGlobalOffsetX  int64
	HiddenGlobalOffsetY  int64
	HiddenGlobalOffsetZ  int64
}

// Benchmark is the XSBench benchmark.
type Benchmark struct {
	driver  *driver.Driver
	context *driver.Context
	hsaco   *insts.HsaCo
	gpus    []int

	// Tunable inputs.
	NIsotopes   int64
	NGridpoints int64
	Lookups     int32
	GridType    int32
	HashBins    int32

	// Host data.
	maxNumNucs           int32
	numNucs              []int32
	mats                 []int32
	concs                []float32
	nuclideGrid          []NuclideGridPoint
	unionizedEnergyArray []float32
	indexGrid            []int32

	// Device pointers.
	gNumNucs              driver.GPUPtr
	gConcs                driver.GPUPtr
	gMats                 driver.GPUPtr
	gUnionizedEnergyArray driver.GPUPtr
	gIndexGrid            driver.GPUPtr
	gNuclideGrid          driver.GPUPtr
	gVerification         driver.GPUPtr

	useUnifiedMemory   bool
	useLASPMemoryAlloc bool
}

// NewBenchmark creates a new XSBench benchmark. Defaults to H-M Small
// (68 isotopes): H-M Large (355) builds a ~5.7 GB unionized index grid on the
// host, which is impractical here — use -grid-type=nuclide for Large.
func NewBenchmark(driver *driver.Driver) *Benchmark {
	b := new(Benchmark)
	b.driver = driver
	b.context = b.driver.Init()
	b.hsaco = kernels.LoadProgramFromMemory(hsacoBytes, "macro_xs_lookup_kernel")

	b.NIsotopes = 68
	b.NGridpoints = 11303
	b.Lookups = 500000
	b.GridType = gridUnionized
	b.HashBins = 10000

	return b
}

// SetGridType selects the lookup grid type by name.
func (b *Benchmark) SetGridType(name string) {
	switch name {
	case "unionized":
		b.GridType = gridUnionized
	case "nuclide":
		b.GridType = gridNuclide
	case "hash":
		b.GridType = gridHash
	default:
		log.Fatalf("unknown grid type %q (expected unionized/nuclide/hash)\n", name)
	}
}

// SelectGPU selects the GPUs to run on.
func (b *Benchmark) SelectGPU(gpus []int) { b.gpus = gpus }

// SetUnifiedMemory uses unified memory.
func (b *Benchmark) SetUnifiedMemory() { b.useUnifiedMemory = true }

// SetLASPMemoryAlloc uses the LASP memory allocator.
func (b *Benchmark) SetLASPMemoryAlloc() { b.useLASPMemoryAlloc = true }

// Run runs the benchmark.
func (b *Benchmark) Run() {
	b.driver.SelectGPU(b.context, b.gpus[0])
	b.initMem()
	b.exec()
}

func (b *Benchmark) initMem() {
	b.gridInit()

	// Both buffers may be empty for some grid types; allocate at least one
	// element so the kernel always receives a valid (if unused) pointer.
	if len(b.unionizedEnergyArray) == 0 {
		b.unionizedEnergyArray = make([]float32, 1)
	}
	if len(b.indexGrid) == 0 {
		b.indexGrid = make([]int32, 1)
	}

	alloc := func(size uint64) driver.GPUPtr {
		if b.useUnifiedMemory {
			return b.driver.AllocateUnifiedMemory(b.context, size)
		}
		if b.useLASPMemoryAlloc {
			return b.driver.AllocateMemoryLASP(b.context, size, "div4")
		}
		p := b.driver.AllocateMemory(b.context, size)
		b.driver.Distribute(b.context, p, size, b.gpus)
		return p
	}

	b.gNumNucs = alloc(uint64(len(b.numNucs) * 4))
	b.gConcs = alloc(uint64(len(b.concs) * 4))
	b.gMats = alloc(uint64(len(b.mats) * 4))
	b.gUnionizedEnergyArray = alloc(uint64(len(b.unionizedEnergyArray) * 4))
	b.gIndexGrid = alloc(uint64(len(b.indexGrid) * 4))
	b.gNuclideGrid = alloc(uint64(len(b.nuclideGrid) * 32))
	b.gVerification = alloc(uint64(int(b.Lookups) * 4))

	b.driver.MemCopyH2D(b.context, b.gNumNucs, b.numNucs)
	b.driver.MemCopyH2D(b.context, b.gConcs, b.concs)
	b.driver.MemCopyH2D(b.context, b.gMats, b.mats)
	b.driver.MemCopyH2D(b.context, b.gUnionizedEnergyArray, b.unionizedEnergyArray)
	b.driver.MemCopyH2D(b.context, b.gIndexGrid, b.indexGrid)
	b.driver.MemCopyH2D(b.context, b.gNuclideGrid, b.nuclideGrid)
}

func (b *Benchmark) makeInputs() inputs {
	return inputs{
		NThreads:         1,
		NIsotopes:        b.NIsotopes,
		NGridpoints:      b.NGridpoints,
		Lookups:          b.Lookups,
		GridType:         b.GridType,
		HashBins:         b.HashBins,
		SimulationMethod: 2, // EVENT_BASED
	}
}

func (b *Benchmark) exec() {
	queues := make([]*driver.CommandQueue, len(b.gpus))

	const wg = 256
	nGPU := len(b.gpus)
	numWi := roundUp(int(b.Lookups), wg*nGPU)
	wiPerGPU := numWi / nGPU

	in := b.makeInputs()

	for i, gpu := range b.gpus {
		b.driver.SelectGPU(b.context, gpu)
		queues[i] = b.driver.CreateCommandQueue(b.context)

		kernArg := KernelArgs{
			In:                  in,
			MaxNumNucs:          b.maxNumNucs,
			NumNucs:             b.gNumNucs,
			Concs:               b.gConcs,
			UnionizedEnergyArray: b.gUnionizedEnergyArray,
			IndexGrid:           b.gIndexGrid,
			NuclideGrid:         b.gNuclideGrid,
			Mats:                b.gMats,
			VerificationArray:   b.gVerification,
			HiddenGlobalOffsetX: int64(i * wiPerGPU),
		}

		b.driver.EnqueueLaunchKernel(
			queues[i],
			b.hsaco,
			[3]uint32{uint32(wiPerGPU), 1, 1},
			[3]uint16{wg, 1, 1},
			&kernArg,
		)
	}

	for i := range b.gpus {
		b.driver.DrainCommandQueue(queues[i])
	}
}

func roundUp(n, multiple int) int {
	if n%multiple == 0 {
		return n
	}
	return (n/multiple + 1) * multiple
}

////////////////////////////////////////////////////////////////////////////////
// Host-side grid construction (mirrors GridInit.c / Materials.c)
////////////////////////////////////////////////////////////////////////////////

func (b *Benchmark) gridInit() {
	nIso := int(b.NIsotopes)
	nGrid := int(b.NGridpoints)
	lenNuclideGrid := nIso * nGrid

	seed := uint64(42)
	b.nuclideGrid = make([]NuclideGridPoint, lenNuclideGrid)
	for i := range b.nuclideGrid {
		b.nuclideGrid[i].Energy = lcgRandomFloat(&seed)
		b.nuclideGrid[i].TotalXS = lcgRandomFloat(&seed)
		b.nuclideGrid[i].ElasticXS = lcgRandomFloat(&seed)
		b.nuclideGrid[i].AbsorptionXS = lcgRandomFloat(&seed)
		b.nuclideGrid[i].FissionXS = lcgRandomFloat(&seed)
		b.nuclideGrid[i].NuFissionXS = lcgRandomFloat(&seed)
	}
	for i := 0; i < nIso; i++ {
		s := b.nuclideGrid[i*nGrid : (i+1)*nGrid]
		sort.Slice(s, func(a, c int) bool { return s[a].Energy < s[c].Energy })
	}

	b.numNucs = loadNumNucs(b.NIsotopes)
	b.mats, b.maxNumNucs = loadMats(b.numNucs, b.NIsotopes)
	b.concs = loadConcs(b.numNucs, b.maxNumNucs)

	switch b.GridType {
	case gridUnionized:
		lenUEG := lenNuclideGrid
		b.unionizedEnergyArray = make([]float32, lenUEG)
		for i := 0; i < lenUEG; i++ {
			b.unionizedEnergyArray[i] = b.nuclideGrid[i].Energy
		}
		sort.Slice(b.unionizedEnergyArray, func(a, b2 int) bool {
			return b.unionizedEnergyArray[a] < b.unionizedEnergyArray[b2]
		})

		b.indexGrid = make([]int32, lenUEG*nIso)
		idxLow := make([]int32, nIso)
		energyHigh := make([]float32, nIso)
		for i := 0; i < nIso; i++ {
			energyHigh[i] = b.nuclideGrid[i*nGrid+1].Energy
		}
		for e := 0; e < lenUEG; e++ {
			ue := b.unionizedEnergyArray[e]
			for i := 0; i < nIso; i++ {
				if ue < energyHigh[i] {
					b.indexGrid[e*nIso+i] = idxLow[i]
				} else if int(idxLow[i]) == nGrid-2 {
					b.indexGrid[e*nIso+i] = idxLow[i]
				} else {
					idxLow[i]++
					b.indexGrid[e*nIso+i] = idxLow[i]
					energyHigh[i] = b.nuclideGrid[i*nGrid+int(idxLow[i])+1].Energy
				}
			}
		}
	case gridHash:
		bins := int(b.HashBins)
		b.indexGrid = make([]int32, bins*nIso)
		du := float32(1.0) / float32(b.HashBins)
		for e := 0; e < bins; e++ {
			energy := float32(e) * du
			for i := 0; i < nIso; i++ {
				b.indexGrid[e*nIso+i] = int32(gridSearchNuclide(
					int64(nGrid), energy, b.nuclideGrid[i*nGrid:], 0, int64(nGrid-1)))
			}
		}
	case gridNuclide:
		// No acceleration structures; the kernel searches in-place.
	}
}

func loadNumNucs(nIsotopes int64) []int32 {
	n := make([]int32, 12)
	if nIsotopes == 68 {
		n[0] = 34
	} else {
		n[0] = 321
	}
	n[1], n[2], n[3], n[4], n[5] = 5, 4, 4, 27, 21
	n[6], n[7], n[8], n[9] = 21, 21, 21, 21
	n[10], n[11] = 9, 9
	return n
}

func loadMats(numNucs []int32, nIsotopes int64) ([]int32, int32) {
	var maxNumNucs int32
	for _, v := range numNucs {
		if v > maxNumNucs {
			maxNumNucs = v
		}
	}
	mats := make([]int32, 12*int(maxNumNucs))

	mats0Sml := []int32{58, 59, 60, 61, 40, 42, 43, 44, 45, 46, 1, 2, 3, 7,
		8, 9, 10, 29, 57, 47, 48, 0, 62, 15, 33, 34, 52, 53,
		54, 55, 56, 18, 23, 41}
	mats0Lrg := make([]int32, 321)
	copy(mats0Lrg, mats0Sml)
	for i := 0; i < 321-34; i++ {
		mats0Lrg[34+i] = int32(68 + i)
	}
	mats1 := []int32{63, 64, 65, 66, 67}
	mats2 := []int32{24, 41, 4, 5}
	mats3 := []int32{24, 41, 4, 5}
	mats4 := []int32{19, 20, 21, 22, 35, 36, 37, 38, 39, 25, 27, 28, 29,
		30, 31, 32, 26, 49, 50, 51, 11, 12, 13, 14, 6, 16, 17}
	matsRefl := []int32{24, 41, 4, 5, 19, 20, 21, 22, 35, 36, 37, 38, 39, 25,
		49, 50, 51, 11, 12, 13, 14}
	mats10 := []int32{24, 41, 4, 5, 63, 64, 65, 66, 67}
	mats11 := []int32{24, 41, 4, 5, 63, 64, 65, 66, 67}

	copyMat := func(m int, src []int32) {
		copy(mats[m*int(maxNumNucs):], src[:numNucs[m]])
	}
	if nIsotopes == 68 {
		copyMat(0, mats0Sml)
	} else {
		copyMat(0, mats0Lrg)
	}
	copyMat(1, mats1)
	copyMat(2, mats2)
	copyMat(3, mats3)
	copyMat(4, mats4)
	copyMat(5, matsRefl)
	copyMat(6, matsRefl)
	copyMat(7, matsRefl)
	copyMat(8, matsRefl)
	copyMat(9, matsRefl)
	copyMat(10, mats10)
	copyMat(11, mats11)

	return mats, maxNumNucs
}

func loadConcs(numNucs []int32, maxNumNucs int32) []float32 {
	seed := uint64(startingSeed) * uint64(startingSeed)
	concs := make([]float32, 12*int(maxNumNucs))
	for i := 0; i < 12; i++ {
		for j := 0; j < int(numNucs[i]); j++ {
			concs[i*int(maxNumNucs)+j] = lcgRandomFloat(&seed)
		}
	}
	return concs
}

////////////////////////////////////////////////////////////////////////////////
// Verification: re-run every lookup on the host and compare (mirrors kernel.cl)
////////////////////////////////////////////////////////////////////////////////

// Verify recomputes each lookup's result on the CPU using the same LCG and
// lookup logic as the kernel, and compares against the device output.
func (b *Benchmark) Verify() {
	gpuVer := make([]int32, b.Lookups)
	b.driver.MemCopyD2H(b.context, gpuVer, b.gVerification)

	mismatches := 0
	for i := 0; i < int(b.Lookups); i++ {
		exp := b.computeLookup(i)
		if gpuVer[i] != exp {
			if mismatches < 10 {
				log.Printf("mismatch at %d: expected %d, got %d\n", i, exp, gpuVer[i])
			}
			mismatches++
		}
	}

	if mismatches > 0 {
		log.Fatalf("Verification failed: %d / %d lookups mismatched\n",
			mismatches, b.Lookups)
	}
	log.Printf("Passed!\n")
}

func (b *Benchmark) computeLookup(i int) int32 {
	seed := uint64(startingSeed)
	seed = fastForwardLCG(seed, uint64(2*i))

	pEnergy := lcgRandomFloat(&seed)
	mat := pickMat(&seed)

	macro := b.calculateMacroXS(pEnergy, mat)

	max := float32(-1.0)
	maxIdx := 0
	for j := 0; j < 5; j++ {
		if macro[j] > max {
			max = macro[j]
			maxIdx = j
		}
	}
	return int32(maxIdx + 1)
}

func (b *Benchmark) calculateMacroXS(pEnergy float32, mat int) [5]float32 {
	var macro [5]float32
	nIso := b.NIsotopes
	nGrid := b.NGridpoints
	maxNum := int64(b.maxNumNucs)

	idx := int64(-1)
	switch b.GridType {
	case gridUnionized:
		idx = gridSearch(nIso*nGrid, pEnergy, b.unionizedEnergyArray)
	case gridHash:
		idx = int64(pEnergy * float32(b.HashBins))
	}

	for j := int64(0); j < int64(b.numNucs[mat]); j++ {
		pNuc := b.mats[int64(mat)*maxNum+j]
		conc := b.concs[int64(mat)*maxNum+j]
		xs := b.calculateMicroXS(pEnergy, int(pNuc), idx)
		for k := 0; k < 5; k++ {
			macro[k] += xs[k] * conc
		}
	}
	return macro
}

func (b *Benchmark) calculateMicroXS(pEnergy float32, nuc int, idx int64) [5]float32 {
	nIso := b.NIsotopes
	nGrid := b.NGridpoints
	base := int64(nuc) * nGrid

	var lowIdx int64
	switch b.GridType {
	case gridNuclide:
		nIdx := gridSearchNuclide(nGrid, pEnergy, b.nuclideGrid[base:], 0, nGrid-1)
		if nIdx == nGrid-1 {
			lowIdx = base + nIdx - 1
		} else {
			lowIdx = base + nIdx
		}
	case gridUnionized:
		gp := int64(b.indexGrid[idx*nIso+int64(nuc)])
		if gp == nGrid-1 {
			lowIdx = base + gp - 1
		} else {
			lowIdx = base + gp
		}
	default: // hash
		uLow := int64(b.indexGrid[idx*nIso+int64(nuc)])
		var uHigh int64
		if idx == int64(b.HashBins)-1 {
			uHigh = nGrid - 1
		} else {
			uHigh = int64(b.indexGrid[(idx+1)*nIso+int64(nuc)]) + 1
		}
		eLow := b.nuclideGrid[base+uLow].Energy
		eHigh := b.nuclideGrid[base+uHigh].Energy
		var lower int64
		if pEnergy <= eLow {
			lower = 0
		} else if pEnergy >= eHigh {
			lower = nGrid - 1
		} else {
			lower = gridSearchNuclide(nGrid, pEnergy, b.nuclideGrid[base:], uLow, uHigh)
		}
		if lower == nGrid-1 {
			lowIdx = base + lower - 1
		} else {
			lowIdx = base + lower
		}
	}

	low := &b.nuclideGrid[lowIdx]
	high := &b.nuclideGrid[lowIdx+1]
	f := (high.Energy - pEnergy) / (high.Energy - low.Energy)

	return [5]float32{
		high.TotalXS - f*(high.TotalXS-low.TotalXS),
		high.ElasticXS - f*(high.ElasticXS-low.ElasticXS),
		high.AbsorptionXS - f*(high.AbsorptionXS-low.AbsorptionXS),
		high.FissionXS - f*(high.FissionXS-low.FissionXS),
		high.NuFissionXS - f*(high.NuFissionXS-low.NuFissionXS),
	}
}

func gridSearch(n int64, quarry float32, a []float32) int64 {
	lower := int64(0)
	upper := n - 1
	length := upper - lower
	for length > 1 {
		mid := lower + length/2
		if a[mid] > quarry {
			upper = mid
		} else {
			lower = mid
		}
		length = upper - lower
	}
	return lower
}

func gridSearchNuclide(n int64, quarry float32, a []NuclideGridPoint, low, high int64) int64 {
	lower := low
	upper := high
	length := upper - lower
	for length > 1 {
		mid := lower + length/2
		if a[mid].Energy > quarry {
			upper = mid
		} else {
			lower = mid
		}
		length = upper - lower
	}
	return lower
}

func pickMat(seed *uint64) int {
	dist := [12]float32{0.140, 0.052, 0.275, 0.134, 0.154, 0.064,
		0.066, 0.055, 0.008, 0.015, 0.025, 0.013}
	roll := lcgRandomFloat(seed)
	for i := 0; i < 12; i++ {
		running := float32(0.0)
		for j := i; j > 0; j-- {
			running += dist[j]
		}
		if roll < running {
			return i
		}
	}
	return 0
}

// lcgRandomFloat normalises the 63-bit LCG state to float32 [0,1) using the
// high 31 bits and a u32->f32 convert scaled by 2^-31. Mirrors kernel.cl
// bit-for-bit and avoids the (float)(uint64) cast, whose device lowering
// (v_lshlrev_b64-based u64->f32) the MGPUSim emu mis-executes — see kernel.cl.
func lcgRandomFloat(seed *uint64) float32 {
	*seed = (lcgA*(*seed) + lcgC) % lcgM
	return float32(uint32(*seed>>32)) * (1.0 / 2147483648.0)
}

func fastForwardLCG(seed, n uint64) uint64 {
	a := lcgA
	c := lcgC
	n = n % lcgM

	aNew := uint64(1)
	cNew := uint64(0)
	for n > 0 {
		if n&1 != 0 {
			aNew *= a
			cNew = cNew*a + c
		}
		c *= a + 1
		a *= a
		n >>= 1
	}
	return (aNew*seed + cNew) % lcgM
}
