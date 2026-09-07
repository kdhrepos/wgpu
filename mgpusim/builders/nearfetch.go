package builders

import (
	"fmt"

	"gitlab.com/akita/akita"
	"gitlab.com/akita/lmf"
	"gitlab.com/akita/mem"
	"gitlab.com/akita/mem/cache/writebackrcr"
	"gitlab.com/akita/mem/idealmemcontroller"
	"gitlab.com/akita/mgpusim"
	"gitlab.com/akita/mgpusim/rdma"
	"gitlab.com/akita/noc/networking/chipnetwork"
)

type NearFetchGPUBuilder struct {
	*CommonBuilder
	gridSize [2]int
}

func MakeNearFetchGPUBuilder() NearFetchGPUBuilder {
	cbp := CommonBuilder{}
	b := NearFetchGPUBuilder{CommonBuilder: &cbp}
	b.SetDefaultCommonBuilderParams()
	return b
}

func (b *NearFetchGPUBuilder) WithGridSize(xDim, yDim int) {
	b.gridSize[0] = xDim
	b.gridSize[1] = yDim
}

func (b *NearFetchGPUBuilder) buildChipRDMAEngine(
	chiplet *Chiplet,
) {
	chiplet.chipRdmaEngine = rdma.NewRCRDMAEngine(
		fmt.Sprintf("%s.ChipRCRDMA", chiplet.name),
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

	// rdma engine requires dram ports for remote address translation
	chiplet.chipRdmaEngine.SetTranslationModuleFinder(translationModuleFinderForRDMA)
}

func (b *NearFetchGPUBuilder) buildMemBanks(
	chiplet *Chiplet,
) {
	l2Builder := writebackrcr.MakeBuilder().
		WithEngine(b.engine).
		WithFreq(b.freq).
		WithLog2BlockSize(b.log2CacheLineSize).
		WithWayAssociativity(16).
		WithTotalByteSize(128 * mem.KB).
		WithNumBanks(int(b.numMemoryBankPerChiplet)).
		WithNumMSHREntry(32).   // 32 per bank × 32 banks = 1024 per chiplet (realistic vs Accel-Sim 32–64)
		WithNumMSHRMaxMerge(8). // max 8 merges/entry (Accel-Sim 관례)
		WithNumReqPerCycle(5).
		WithNumRCREntry(32)

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
			WithChipletId(int(chiplet.ChipletID)).
			Build(cacheName)

		// for NearFetch
		l2.SetGridSize(b.gridSize[1], b.gridSize[0])

		b.l2Caches = append(b.l2Caches, l2)
		b.gpu.L2Caches = append(b.gpu.L2Caches, l2)
		chiplet.L2Caches = append(chiplet.L2Caches, l2)
	}
}

// connectL2ToRdma overrides the CommonBuilder singular helper to wire
// nearfetch's RCRDMAEngine to its L2 caches. Single outbound port per role
// (ToL2RDMA / ToL2Bottom / ToDramTop) — no AddL2BankPorts/MapL2Dest. The
// RC-specific LookupPort is still plugged per bank because it belongs to the
// writebackrcr cache, not the RDMA engine.
func (b *NearFetchGPUBuilder) connectL2ToRdma(
	chiplet *Chiplet,
) {
	localModuleFinderforRDMA := lmf.NewStripedLowModuleFinder(
		b.memAddrOffset,
		b.numMemoryBankPerChiplet,
		1<<b.log2MemoryBankInterleavingSize)
	localModuleFinderforLookup := lmf.NewStripedLowModuleFinder(
		b.memAddrOffset,
		b.numMemoryBankPerChiplet,
		1<<b.log2MemoryBankInterleavingSize)

	chiplet.L2ToRdmaConnection = akita.NewDirectConnection(
		chiplet.name+".L2-RDMA", b.engine, b.freq)

	chiplet.L2ToRdmaConnection.PlugIn(chiplet.chipRdmaEngine.GetToL2Bottom(),
		4*chiplet.chipRdmaEngine.GetNumTransPerCycle()) // from rdma to l2 bottom port for rsp

	rcRdma := chiplet.chipRdmaEngine.(*rdma.RCRDMAEngine)

	for _, l2Cache := range chiplet.L2Caches {
		chiplet.L2ToRdmaConnection.PlugIn(l2Cache.GetBottomToRdmaPort(),
			2*chiplet.chipRdmaEngine.GetNumTransPerCycle())
		chiplet.L2ToRdmaConnection.PlugIn(l2Cache.GetRDMAPort(),
			2*chiplet.chipRdmaEngine.GetNumTransPerCycle())
		chiplet.L2ToRdmaConnection.PlugIn(l2Cache.(*writebackrcr.Cache).LookupPort,
			2*chiplet.chipRdmaEngine.GetNumTransPerCycle())

		localModuleFinderforRDMA.LowModules =
			append(localModuleFinderforRDMA.LowModules, l2Cache.GetRDMAPort())
		localModuleFinderforLookup.LowModules =
			append(localModuleFinderforLookup.LowModules, l2Cache.(*writebackrcr.Cache).LookupPort)
	}

	// chip rdma engines' local module should be L2 cache
	// Top and Bottom ports are both needed.
	chiplet.chipRdmaEngine.SetLocalModuleFinder(localModuleFinderforRDMA)
	rcRdma.SetLocalModuleFinderForLookup(localModuleFinderforLookup)

	// Single ToL2RDMA outbound port (no per-bank ToL2RDMAs[i]).
	chiplet.L2ToRdmaConnection.PlugIn(chiplet.chipRdmaEngine.GetToL2RDMA(),
		4*chiplet.chipRdmaEngine.GetNumTransPerCycle()) // from rdma to l2 for req

	// only the ToDramTop port should be connected to dram connection for l2 cache eviction
	chiplet.L2ToDramConnection.PlugIn(chiplet.chipRdmaEngine.GetToDramTop(),
		4*chiplet.chipRdmaEngine.GetNumTransPerCycle())
}

func (b *NearFetchGPUBuilder) connectRDMAEngines() {
	// this table only used for finding request/response port from each chiplet.
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

	// put the chipRdmaReqAddrTable to l2 cache
	for _, l2 := range b.gpu.L2Caches {
		l2, ok := l2.(*writebackrcr.Cache)
		if !ok {
			panic("wrong conversion on from cache interface to writebackrcr")
		}

		l2.RdmaReqAddressTable = chipRdmaReqAddrTable
		l2.SetHomeNodeFinder(homeNodeFinder)
	}

	RdmaReqPortByChipletId := make(map[int]akita.Port)
	RdmaRspPortByChipletId := make(map[int]akita.Port)

	for _, chiplet := range b.chiplets {
		RdmaReqPortByChipletId[int(chiplet.ChipletID)] =
			chiplet.chipRdmaEngine.GetRequestPort()
		RdmaRspPortByChipletId[int(chiplet.ChipletID)] =
			chiplet.chipRdmaEngine.GetResponsePort()
	}
	for _, chiplet := range b.chiplets {
		chiplet.chipRdmaEngine.(*rdma.RCRDMAEngine).
			RdmaReqPortByChipletId = RdmaReqPortByChipletId
		chiplet.chipRdmaEngine.(*rdma.RCRDMAEngine).
			RdmaRspPortByChipletId = RdmaRspPortByChipletId
	}

	// used for pte access trace
	for _, mmu := range b.gpu.MMUs {
		mmu.VirtualAddrTable = chipRdmaReqAddrTable
	}
}

func (b NearFetchGPUBuilder) setupSwPortsForRdmaEngine() {
	swReqPorts := b.gpu.InterChipletNetwork[0].(*chipnetwork.InterChipletMeshConnector).
		GetSwReqPorts()
	swRspPorts := b.gpu.InterChipletNetwork[0].(*chipnetwork.InterChipletMeshConnector).
		GetSwRspPorts()

	for _, chiplet := range b.chiplets {
		chiplet.chipRdmaEngine.(*rdma.RCRDMAEngine).SetSwReqPort(swReqPorts[chiplet.ChipletID])
		chiplet.chipRdmaEngine.(*rdma.RCRDMAEngine).SetSwRspPort(swRspPorts[chiplet.ChipletID])
	}
}

func (b NearFetchGPUBuilder) Build(name string, id uint64) *mgpusim.GPU {
	b.createGPU(name, id)

	b.buildCP()

	for r := 0; r < b.gridSize[1]; r++ { // row
		for c := 0; c < b.gridSize[0]; c++ { // column
			chipletName := fmt.Sprintf("%s.chiplet_%02d", b.gpuName, r*b.gridSize[0]+c)
			chipletId := uint64(r*b.gridSize[0] + c)
			chiplet := NewChiplet(chipletName, chipletId)

			b.buildSAs(chiplet)      // L1 cache and TLB are built in here.
			b.buildMemBanks(chiplet) // L2 is built in here.
			b.buildMMU(chiplet)
			b.buildL2TLB(chiplet)

			b.buildChipRDMAEngine(chiplet)

			b.connectL1ToL2(chiplet)
			b.connectL2ToDram(chiplet) // Single-port L2-DRAM wiring
			b.connectL2ToRdma(chiplet) // Single-port L2-RDMA wiring (RC LookupPort kept)
			b.connectL1TLBToL2TLB(chiplet)
			b.connectL2TLBToMMU(chiplet)
			b.connectMMUToBottom(chiplet)

			if b.usePteCaching {
				b.connectMMUToL2(chiplet)
			}

			b.chiplets = append(b.chiplets, chiplet)

			chiplet.chipRdmaEngine.SetChipletId(int(chiplet.ChipletID))
			chiplet.chipRdmaEngine.SetY(r)
			chiplet.chipRdmaEngine.SetX(c)
			chiplet.chipRdmaEngine.SetYDim(b.gridSize[1])
			chiplet.chipRdmaEngine.SetXDim(b.gridSize[0])

			b.setConnectionToDriver(chiplet)
		}
	}

	b.connectRDMAEngines()

	b.buildPageMigrationController()
	b.setupDMA()

	b.connectCP()
	b.setupNearFetchInterchipNetwork()
	b.setupSwPortsForRdmaEngine()

	return b.gpu
}

func (b *NearFetchGPUBuilder) setupNearFetchInterchipNetwork() {
	fmt.Println("Mesh Builder")

	// interconnect bandwidth: 1GHz * 64B * 16 = 1TBps
	nearfetchChipletConnector := chipnetwork.NewInterChipletMeshConnector().
		WithEngine(b.engine).
		WithSwitchLatency(b.switchLatency). // 32ns
		WithFreq(1*akita.GHz).
		// WithFlitByteSize(64).
		WithFlitByteSize(16).
		WithNumReqPerCycle(b.numReqPerCycle).
		WithGridSize(b.gridSize[0], b.gridSize[1]).
		WithNumChiplets(b.gridSize[0] * b.gridSize[1]).
		WithRDMAReqPorts(b.GetRdmaReqPorts()).
		WithRDMARspPorts(b.GetRdmaRspPorts()).
		WithNetworkName("ICN")

	nearfetchChipletConnector.CreateNetwork()

	b.gpu.InterChipletNetwork = append(b.gpu.InterChipletNetwork, nearfetchChipletConnector)
	b.gpu.SwitchConnection = nearfetchChipletConnector.SwitchConnection
}

func (b *NearFetchGPUBuilder) GetRdmaReqPorts() map[uint64]akita.Port {
	chipletPorts := make(map[uint64]akita.Port)
	for _, chiplet := range b.chiplets {
		chipletPorts[chiplet.ChipletID] = chiplet.chipRdmaEngine.GetRequestPort()
	}

	return chipletPorts
}

func (b *NearFetchGPUBuilder) GetRdmaRspPorts() map[uint64]akita.Port {
	chipletPorts := make(map[uint64]akita.Port)
	for _, chiplet := range b.chiplets {
		chipletPorts[chiplet.ChipletID] = chiplet.chipRdmaEngine.GetResponsePort()
	}

	return chipletPorts
}
