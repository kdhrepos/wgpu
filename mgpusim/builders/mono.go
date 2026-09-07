package builders

import (
	"fmt"
	"math"

	"gitlab.com/akita/akita"
	"gitlab.com/akita/lmf"
	"gitlab.com/akita/mem"
	"gitlab.com/akita/mem/cache/writeback"
	"gitlab.com/akita/mem/idealmemcontroller"
	"gitlab.com/akita/mem/vm/mmu"
	"gitlab.com/akita/mem/vm/tlb"
	"gitlab.com/akita/mgpusim"
	"gitlab.com/akita/mgpusim/rdma"
)

// MonoGPUBuilder builds a single monolithic chiplet whose CU /
// L1V / L2 / DRAM resources scale with (gridSize[0] * gridSize[1]).
// The xDim/yDim flags become a resource multiplier rather than a
// topology coordinate — there is no inter-chiplet NoC or RDMA traffic.
type MonoGPUBuilder struct {
	*CommonBuilder
	gridSize [2]int
}

func MakeMonoGPUBuilder() MonoGPUBuilder {
	cbp := CommonBuilder{}
	b := MonoGPUBuilder{CommonBuilder: &cbp}
	b.SetDefaultCommonBuilderParams()
	return b
}

func (b *MonoGPUBuilder) WithGridSize(xDim, yDim int) {
	b.gridSize[0] = xDim
	b.gridSize[1] = yDim
}

func (b *MonoGPUBuilder) buildMemBanks(
	chiplet *Chiplet,
) {
	l2Builder := writeback.MakeBuilder().
		WithEngine(b.engine).
		WithFreq(b.freq).
		WithLog2BlockSize(b.log2CacheLineSize).
		WithWayAssociativity(16).
		WithTotalByteSize(128 * mem.KB).
		WithNumBanks(int(b.numMemoryBankPerChiplet)).
		WithNumMSHREntry(32).
		WithNumMSHRMaxMerge(8).
		WithNumReqPerCycle(5).
		WithChipletId(int(chiplet.ChipletID))

	for i := uint64(0); i < b.numMemoryBankPerChiplet; i++ {
		dramName := fmt.Sprintf("%s.DRAM_%d", chiplet.name, i)
		// XOR-hash routing requires totalMem capacity (identity offset)
		// and bankStripe-sized storage units to avoid numBanks× blowup.
		dram := idealmemcontroller.New(
			dramName, b.engine, b.memoryPerBank)
		addrConverter := idealmemcontroller.DualStripeConverter{
			ChipletStripe:      b.chipletInterleavingSize(),
			BankStripe:         1 << b.log2MemoryBankInterleavingSize,
			NumChiplets:        b.numChiplets,
			NumBanksPerChiplet: b.numMemoryBankPerChiplet,
			CurrentChiplet:     chiplet.ChipletID,
			CurrentLocalBank:   i,
			Offset:             b.memAddrOffset,
		}
		dram.AddressConverter = addrConverter

		b.drams = append(b.drams, dram)
		b.gpu.MemoryControllers = append(b.gpu.MemoryControllers, dram)
		chiplet.DRAMs = append(chiplet.DRAMs, dram)

		cacheName := fmt.Sprintf("%s.L2_%d", chiplet.name, i)
		l2 := l2Builder.
			WithBankId(int(i)).
			Build(cacheName)

		b.l2Caches = append(b.l2Caches, l2)
		b.gpu.L2Caches = append(b.gpu.L2Caches, l2)
		chiplet.L2Caches = append(chiplet.L2Caches, l2)
	}
}

func (b *MonoGPUBuilder) buildChipRDMAEngine(
	chiplet *Chiplet,
) {
	chiplet.chipRdmaEngine = rdma.NewRDMAEngine(
		fmt.Sprintf("%s.ChipRDMA", chiplet.name),
		b.engine,
		b.numReqPerCycle,
	)

	translationModuleFinderForRDMA := lmf.NewStripedLowModuleFinder(
		b.memAddrOffset,
		uint64(b.numMemoryBankPerChiplet),
		1<<b.log2MemoryBankInterleavingSize,
	)

	for _, dram := range chiplet.DRAMs {
		translationModuleFinderForRDMA.LowModules = append(
			translationModuleFinderForRDMA.LowModules,
			dram.ToTop)
	}

	chiplet.chipRdmaEngine.SetTranslationModuleFinder(translationModuleFinderForRDMA)
}

// buildMMU and buildL2TLB scale the per-chiplet MMU + L2 TLB capacity by
// xDim*yDim. CommonBuilder builds one MMU (16 in-flight PTWs) and one L2
// TLB (4 reqs/cycle, 64 sets × 8 ways) per chiplet, which becomes a 36×
// bottleneck for mono since 1152 L1 TLBs funnel into a single L2 TLB.
// Mono restores per-resource parity by multiplying throughput / capacity /
// way-count by the scale factor while keeping NumSets a power of two for
// the existing indexing mask formula.
func (b *MonoGPUBuilder) buildMMU(
	chiplet *Chiplet,
) {
	scale := int(b.gridSize[0] * b.gridSize[1])
	mmuBuilder := mmu.MakeBuilder().
		WithEngine(b.engine).
		WithFreq(1 * akita.GHz).
		WithLog2PageSize(b.log2PageSize).
		WithPageTable(b.pageTable).
		WithNumChiplets(uint64(b.numChiplets)).
		WithMaxNumReqInFlight(16 * scale)
	chiplet.MMU = mmuBuilder.Build(fmt.Sprintf("%s.MMU", chiplet.name))
	chiplet.MMU.CommandProcessor = b.gpu.CommandProcessor.ToMMUs
	b.gpu.MMUs = append(b.gpu.MMUs, chiplet.MMU)
}

func (b *MonoGPUBuilder) buildL2TLB(
	chiplet *Chiplet,
) {
	scale := int(b.gridSize[0] * b.gridSize[1])
	numSets := 64
	numWays := 8 * scale
	log2NumSets := int(math.Log2(float64(numSets)))

	tlbIndexBitsStart := int(math.Log2(float64(b.remoteTLBInterleavingSize))) +
		int(b.log2PageSize) + 1
	tlbIndexBitsEnd := tlbIndexBitsStart +
		int(math.Log2(float64(b.numChiplets))) - 1

	mask := uint64(0)
	t := uint64(1) << b.log2PageSize
	numBitsSet := 0
	for i := int(b.log2PageSize) + 1; i <= 64; i++ {
		if i < tlbIndexBitsStart || i > tlbIndexBitsEnd {
			mask = mask | t
			numBitsSet++
			if numBitsSet == log2NumSets {
				break
			}
		}
		t = t << 1
	}

	builder := tlb.MakeLatTLBBuilder().
		WithEngine(b.engine).
		WithFreq(b.freq).
		WithNumWays(numWays).
		WithNumSets(numSets).
		WithNumMSHREntry(64 * scale).
		WithNumReqPerCycle(4 * scale).
		WithLog2PageSize(b.log2PageSize).
		WithLowModule(chiplet.MMU.ToTop).
		WithIndexingMask(mask).
		WithLatency(10)

	l2TLB := builder.Build(fmt.Sprintf("%s.L2TLB", chiplet.name))
	l2TLB.SetLowModuleFinder(&lmf.SingleLowModuleFinder{
		LowModule: chiplet.MMU.ToTop,
	})

	b.l2TLBs = append(b.l2TLBs, l2TLB)
	b.gpu.L2TLBs = append(b.gpu.L2TLBs, l2TLB)
	chiplet.L2TLBs = append(chiplet.L2TLBs, l2TLB)
}

// connectRDMAEngines mirrors DirectGPUBuilder.connectRDMAEngines for the
// single-chiplet case: the address tables degenerate to one entry, but the
// L2 caches still expect a non-nil HomeNodeFinder for memory-trace lookups
// (writeback.directoryStage.handleReadMiss indexes chipletIDStrings via
// HomeNodeFinder.GetChipletId — leaving it nil panics on any read miss).
func (b *MonoGPUBuilder) connectRDMAEngines() {
	chipRdmaReqAddrTable := lmf.NewStripedLowModuleFinder(
		b.memAddrOffset,
		uint64(b.numChiplets),
		b.chipletInterleavingSize())
	chipRdmaRspAddrTable := make(map[akita.Port]akita.Port)

	for i := uint64(0); i < b.numChiplets; i++ {
		chipRdmaReqAddrTable.LowModules = append(
			chipRdmaReqAddrTable.LowModules,
			b.chiplets[i].chipRdmaEngine.GetRequestPort())
		chipRdmaRspAddrTable[b.chiplets[i].chipRdmaEngine.GetRequestPort()] =
			b.chiplets[i].chipRdmaEngine.GetResponsePort()
	}

	for i := uint64(0); i < b.numChiplets; i++ {
		b.chiplets[i].chipRdmaEngine.SetRdmaReqAddressTable(chipRdmaReqAddrTable)
		b.chiplets[i].chipRdmaEngine.SetRdmaRspAddressTable(chipRdmaRspAddrTable)
		b.gpu.ChipRDMAEngines = append(
			b.gpu.ChipRDMAEngines,
			b.chiplets[i].chipRdmaEngine)
	}

	homeNodeFinder := lmf.NewHomeNodeFinder(
		b.memAddrOffset,
		uint64(b.numChiplets),
		b.chipletInterleavingSize())

	for _, l2Cache := range b.gpu.L2Caches {
		l2Cache := l2Cache.(*writeback.Cache)
		l2Cache.SetVirtualAddrTable(chipRdmaReqAddrTable)
		l2Cache.SetHomeNodeFinder(homeNodeFinder)
	}

	for _, mmu := range b.gpu.MMUs {
		mmu.VirtualAddrTable = chipRdmaReqAddrTable
	}
}

// Build assembles a single monolithic chiplet whose per-chiplet resource
// counts have been multiplied by gridSize[0]*gridSize[1]. The chiplet-
// internal helpers in CommonBuilder (buildSAs, buildMemBanks, L1↔L2 wiring,
// directory setID + StripedLowModuleFinder) handle the inflated bank/SA
// counts transparently. No inter-chiplet NoC is built.
func (b MonoGPUBuilder) Build(name string, id uint64) *mgpusim.GPU {
	scale := uint64(b.gridSize[0] * b.gridSize[1])
	b.numShaderArrayPerChiplet *= scale
	b.numMemoryBankPerChiplet *= scale
	b.numChiplets = 1
	b.CalculateMemoryParameters()

	b.createGPU(name, id)

	b.buildCP()

	chipletName := fmt.Sprintf("%s.chiplet_00", b.gpuName)
	chiplet := NewChiplet(chipletName, 0)

	b.buildSAs(chiplet)
	b.buildMemBanks(chiplet)
	b.buildMMU(chiplet)
	b.buildL2TLB(chiplet)

	b.buildChipRDMAEngine(chiplet)

	b.connectL1ToL2(chiplet)
	b.connectL2ToDram(chiplet)
	b.connectL2ToRdma(chiplet)
	b.connectL1TLBToL2TLB(chiplet)
	b.connectL2TLBToMMU(chiplet)
	b.connectMMUToBottom(chiplet)

	if b.usePteCaching {
		b.connectMMUToL2(chiplet)
	}

	b.chiplets = append(b.chiplets, chiplet)

	chiplet.chipRdmaEngine.SetChipletId(int(chiplet.ChipletID))
	chiplet.chipRdmaEngine.SetY(0)
	chiplet.chipRdmaEngine.SetX(0)
	chiplet.chipRdmaEngine.SetYDim(1)
	chiplet.chipRdmaEngine.SetXDim(1)

	b.connectRDMAEngines()

	b.buildPageMigrationController()
	b.setupDMA()

	b.connectCP()

	return b.gpu
}
