package builders

import (
	"fmt"

	"gitlab.com/akita/akita"
	"gitlab.com/akita/lmf"
	"gitlab.com/akita/mem"
	"gitlab.com/akita/mem/cache/writeback"
	"gitlab.com/akita/mem/idealmemcontroller"
	"gitlab.com/akita/mgpusim"
	"gitlab.com/akita/mgpusim/rdma"
	"gitlab.com/akita/noc/networking/chipnetwork"
	"gitlab.com/akita/noc/networking/switching"
)

type DataDirectGPUBuilder struct {
	*CommonBuilder
	gridSize [2]int
}

func MakeDataDirectGPUBuilder() DataDirectGPUBuilder {
	cbp := CommonBuilder{}
	b := DataDirectGPUBuilder{CommonBuilder: &cbp}
	b.SetDefaultCommonBuilderParams()
	return b
}

func (b *DataDirectGPUBuilder) buildMemBanks(
	chiplet *Chiplet,
) {
	l2Builder := writeback.MakeBuilder().
		WithEngine(b.engine).
		WithFreq(b.freq).
		WithLog2BlockSize(b.log2CacheLineSize).
		WithWayAssociativity(16).
		WithTotalByteSize(128 * mem.KB).
		WithNumBanks(int(b.numMemoryBankPerChiplet)).
		WithNumMSHREntry(32).   // 32 per bank × 32 banks = 1024 per chiplet (realistic vs Accel-Sim 32–64)
		WithNumMSHRMaxMerge(8). // max 8 merges/entry (Accel-Sim 관례)
		WithNumReqPerCycle(5).
		WithChipletId(int(chiplet.ChipletID))

	for i := uint64(0); i < b.numMemoryBankPerChiplet; i++ {
		dramName := fmt.Sprintf("%s.DRAM_%d", chiplet.name, i)
		// Modulo dual-stripe: external compacted to [0, bankSize) by converter,
		// so Storage uses default unitSize (bankSize/16).
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

func (b *DataDirectGPUBuilder) WithGridSize(xDim, yDim int) {
	b.gridSize[0] = xDim
	b.gridSize[1] = yDim
}

func (b *DataDirectGPUBuilder) buildChipRDMAEngine(
	chiplet *Chiplet,
) {
	chiplet.chipRdmaEngine = rdma.NewDataDirectRdmaEngine(
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

	// rdma engine requires dram ports for remote address translation
	chiplet.chipRdmaEngine.SetTranslationModuleFinder(translationModuleFinderForRDMA)
}

func (b *DataDirectGPUBuilder) connectRDMAEngines() {
	// this table only used for finding request/response port from each chiplet.
	chipRdmaReqAddrTable := lmf.NewStripedLowModuleFinder(
		b.memAddrOffset,
		uint64(b.numChiplets),
		b.chipletInterleavingSize())
	chipRdmaRspAddrTable := make(map[akita.Port]akita.Port)

	directChipRdmaReqAddrTable := lmf.NewStripedLowModuleFinder(
		b.memAddrOffset,
		uint64(b.numChiplets),
		b.chipletInterleavingSize())
	directChipRdmaRspAddrTable := make(map[akita.Port]akita.Port)

	directConnection := switching.NewDirectConnectionWithLatency(
		"DirectConnectionWithLatency",
		b.engine,
		b.freq,
		b.numReqPerCycle,
		b.switchLatency)

	for i := uint64(0); i < b.numChiplets; i++ {
		chipRdmaReqAddrTable.LowModules = append(
			chipRdmaReqAddrTable.LowModules,
			b.chiplets[i].chipRdmaEngine.GetRequestPort())
		chipRdmaRspAddrTable[b.chiplets[i].chipRdmaEngine.GetRequestPort()] =
			b.chiplets[i].chipRdmaEngine.GetResponsePort()

		directRequestPort := b.chiplets[i].chipRdmaEngine.(*rdma.DataDirectRdmaEngine).DirectRequestPort
		directResponsePort := b.chiplets[i].chipRdmaEngine.(*rdma.DataDirectRdmaEngine).DirectResponsePort

		directConnection.PlugIn(directRequestPort, 4*b.numReqPerCycle)
		directConnection.PlugIn(directResponsePort, 4*b.numReqPerCycle)

		directChipRdmaReqAddrTable.LowModules = append(
			directChipRdmaReqAddrTable.LowModules,
			directRequestPort)
		directChipRdmaRspAddrTable[directRequestPort] = directResponsePort
	}

	for i := uint64(0); i < b.numChiplets; i++ {
		b.chiplets[i].chipRdmaEngine.SetRdmaReqAddressTable(chipRdmaReqAddrTable)
		b.chiplets[i].chipRdmaEngine.SetRdmaRspAddressTable(chipRdmaRspAddrTable)

		b.chiplets[i].chipRdmaEngine.(*rdma.DataDirectRdmaEngine).DirectRdmaReqAddressTable =
			directChipRdmaReqAddrTable
		b.chiplets[i].chipRdmaEngine.(*rdma.DataDirectRdmaEngine).DirectRdmaRspAddressTable =
			directChipRdmaRspAddrTable

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
		l2Cache := l2Cache.(*writeback.Cache)
		l2Cache.SetVirtualAddrTable(chipRdmaReqAddrTable)
		l2Cache.SetHomeNodeFinder(homeNodeFinder)
	}

	// used for pte access trace
	for _, mmu := range b.gpu.MMUs {
		mmu.VirtualAddrTable = chipRdmaReqAddrTable
	}
}

func (b DataDirectGPUBuilder) setupSwPortsForRdmaEngine() {
	swReqPorts := b.gpu.InterChipletNetwork[0].(*chipnetwork.InterChipletMeshConnector).
		GetSwReqPorts()
	swRspPorts := b.gpu.InterChipletNetwork[0].(*chipnetwork.InterChipletMeshConnector).
		GetSwRspPorts()

	for _, chiplet := range b.chiplets {
		chiplet.chipRdmaEngine.(*rdma.DataDirectRdmaEngine).SetSwReqPort(swReqPorts[chiplet.ChipletID])
		chiplet.chipRdmaEngine.(*rdma.DataDirectRdmaEngine).SetSwRspPort(swRspPorts[chiplet.ChipletID])
	}
}

func (b DataDirectGPUBuilder) Build(name string, id uint64) *mgpusim.GPU {
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
			b.connectL2ToRdma(chiplet) // Single-port L2-RDMA wiring
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
		}
	}

	b.connectRDMAEngines()

	b.buildPageMigrationController()
	b.setupDMA()

	b.connectCP()
	b.setupMeshInterchipNetwork()
	b.setupSwPortsForRdmaEngine()

	return b.gpu
}

func (b *DataDirectGPUBuilder) setupMeshInterchipNetwork() {
	fmt.Println("Direct Builder")

	// interconnect bandwidth: 1GHz * 64B * 16 = 1TBps
	chipMeshConnector := chipnetwork.NewInterChipletMeshConnector().
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

	chipMeshConnector.CreateNetwork()

	b.gpu.InterChipletNetwork = append(b.gpu.InterChipletNetwork, chipMeshConnector)
}

func (b *DataDirectGPUBuilder) GetRdmaReqPorts() map[uint64]akita.Port {
	chipletPorts := make(map[uint64]akita.Port)

	for _, chiplet := range b.chiplets {
		chipletPorts[chiplet.ChipletID] = chiplet.chipRdmaEngine.GetRequestPort()
	}

	return chipletPorts
}

func (b *DataDirectGPUBuilder) GetRdmaRspPorts() map[uint64]akita.Port {
	chipletPorts := make(map[uint64]akita.Port)

	for _, chiplet := range b.chiplets {
		chipletPorts[chiplet.ChipletID] = chiplet.chipRdmaEngine.GetResponsePort()
	}

	return chipletPorts
}
