package builders

import (
	"fmt"

	"gitlab.com/akita/akita"
	"gitlab.com/akita/lmf"
	"gitlab.com/akita/mem"
	writeback_dynamic_dist "gitlab.com/akita/mem/cache/writeback-dynamic-dist"
	"gitlab.com/akita/mem/idealmemcontroller"
	"gitlab.com/akita/mgpusim/rdma"
	"gitlab.com/akita/noc/networking/chipnetwork"
)

func (b *DynamicClusteringGPUBuilder) buildSAs(
	chiplet *Chiplet,
) {
	saBuilder := makeShaderArrayBuilder()
	saBuilder.withEngine(b.engine)
	saBuilder.withFreq(b.freq)
	saBuilder.withGPUID(b.gpu.GPUID)
	saBuilder.withLog2CachelineSize(b.log2CacheLineSize)
	saBuilder.withLog2PageSize(b.log2PageSize)
	saBuilder.withNumCU(int(b.numCUPerShaderArray))
	saBuilder.withPC(b.reqWithPC)
	saBuilder.withChipletId(chiplet.ChipletID)
	saBuilder.withPageTable(b.pageTable)

	for i := uint64(0); i < b.numShaderArrayPerChiplet; i++ {
		saName := fmt.Sprintf("%s.SA_%02d", chiplet.name, i)
		sa := saBuilder.Build(saName)
		b.collectSAComponents(sa, chiplet)
	}
}

func (b *DynamicClusteringGPUBuilder) buildChipRDMAEngine(
	chiplet *Chiplet,
) {
	ClusterId := rdma.GetClusterId(
		int(chiplet.ChipletID),
		b.gridSize[0])

	chiplet.chipRdmaEngine = rdma.NewDynamicClusteringRdmaEngine(
		fmt.Sprintf("%s.ChipRDMA", chiplet.name),
		b.engine,
		b.numReqPerCycle,
		ClusterId,
		b.clusterSize,
		b.gridSize[0],
		b.gridSize[1])

	translationModuleFinderForRDMA := lmf.NewStripedLowModuleFinder(
		b.memAddrOffset,
		uint64(b.numMemoryBankPerChiplet),
		1<<b.log2MemoryBankInterleavingSize)

	for _, dram := range chiplet.DRAMs {
		translationModuleFinderForRDMA.LowModules = append(
			translationModuleFinderForRDMA.LowModules,
			dram.ToTop)
	}

	// rdma engine requires dram ports for remote address translation
	chiplet.chipRdmaEngine.SetTranslationModuleFinder(translationModuleFinderForRDMA)

	L2BottomFinder := lmf.NewStripedLowModuleFinder(
		b.memAddrOffset,
		uint64(b.numMemoryBankPerChiplet),
		1<<b.log2MemoryBankInterleavingSize)

	// used to find bottom port from l2 cache
	for _, l2 := range chiplet.L2Caches {
		L2BottomFinder.LowModules = append(L2BottomFinder.LowModules, l2.GetBottomToRdmaPort())
	}

	chiplet.chipRdmaEngine.(*rdma.DynamicClusteringRdmaEngine).L2BottomFinder = L2BottomFinder

	chiplet.chipRdmaEngine.SetChipletId(int(chiplet.ChipletID))
	chiplet.chipRdmaEngine.SetYDim(b.gridSize[1])
	chiplet.chipRdmaEngine.SetXDim(b.gridSize[0])
}

func (b *DynamicClusteringGPUBuilder) buildMemBanks(
	chiplet *Chiplet,
) {
	l2Builder := writeback_dynamic_dist.MakeBuilder().
		WithEngine(b.engine).
		WithFreq(b.freq).
		WithLog2BlockSize(b.log2CacheLineSize).
		WithWayAssociativity(16).
		WithTotalByteSize(128 * mem.KB).
		WithNumBanks(int(b.numMemoryBankPerChiplet)).
		WithNumMSHREntry(32).   // 32 per bank × 32 banks = 1024 per chiplet (realistic vs Accel-Sim 32–64)
		WithNumMSHRMaxMerge(8). // max 8 merges/entry (Accel-Sim 관례)
		WithNumReqPerCycle(5)

	for i := uint64(0); i < b.numMemoryBankPerChiplet; i++ {
		dramName := fmt.Sprintf("%s.DRAM_%d", chiplet.name, i)
		// Modulo dual-stripe: external compacted to [0, bankSize) by
		// converter, so Storage uses default unitSize (bankSize/16).
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

		ClusterId := rdma.GetClusterId(
			int(chiplet.ChipletID),
			b.gridSize[0],
		)

		cacheName := fmt.Sprintf("%s.L2_%d", chiplet.name, i)
		l2 := l2Builder.
			WithClusterId(ClusterId).
			WithChipletId(int(chiplet.ChipletID)).
			WithBankId(int(i)).
			Build(cacheName)

		b.l2Caches = append(b.l2Caches, l2)
		b.gpu.L2Caches = append(b.gpu.L2Caches, l2)
		chiplet.L2Caches = append(chiplet.L2Caches, l2)
	}
}

func (b *DynamicClusteringGPUBuilder) connectRDMAEngines() {
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

	// only used for l2 miss trace
	for _, l2Cache := range b.gpu.L2Caches {
		l2Cache := l2Cache.(*writeback_dynamic_dist.Cache)
		l2Cache.SetVirtualAddrTable(chipRdmaReqAddrTable)
		l2Cache.SetHomeNodeFinder(homeNodeFinder)
	}

	// used for pte access trace
	for _, mmu := range b.gpu.MMUs {
		mmu.VirtualAddrTable = chipRdmaReqAddrTable
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
		chiplet.chipRdmaEngine.(*rdma.DynamicClusteringRdmaEngine).
			RdmaReqPortByChipletId = RdmaReqPortByChipletId
		chiplet.chipRdmaEngine.(*rdma.DynamicClusteringRdmaEngine).
			RdmaRspPortByChipletId = RdmaRspPortByChipletId
	}
}

func (b DynamicClusteringGPUBuilder) setupSwPortsForRdmaEngine() {
	swReqPorts := b.gpu.InterChipletNetwork[0].(*chipnetwork.InterChipletDynamicClusteringConnector).
		GetSwReqPorts()
	swRspPorts := b.gpu.InterChipletNetwork[0].(*chipnetwork.InterChipletDynamicClusteringConnector).
		GetSwRspPorts()

	for _, chiplet := range b.chiplets {
		chiplet.chipRdmaEngine.(*rdma.DynamicClusteringRdmaEngine).SetSwReqPort(swReqPorts[chiplet.ChipletID])
		chiplet.chipRdmaEngine.(*rdma.DynamicClusteringRdmaEngine).SetSwRspPort(swRspPorts[chiplet.ChipletID])
	}
}
