package builders

import (
	"fmt"

	"gitlab.com/akita/akita"
	"gitlab.com/akita/lmf"
	"gitlab.com/akita/mem/cache/writeback"
	"gitlab.com/akita/mgpusim"
	"gitlab.com/akita/mgpusim/rdma"
	"gitlab.com/akita/noc/networking/chipnetwork"
	"gitlab.com/akita/noc/networking/switching"
)

type PtDirectGPUBuilder struct {
	*CommonBuilder
	gridSize [2]int
}

func MakePtDirectGPUBuilder() PtDirectGPUBuilder {
	cbp := CommonBuilder{}
	b := PtDirectGPUBuilder{CommonBuilder: &cbp}
	b.SetDefaultCommonBuilderParams()
	return b
}

func (b *PtDirectGPUBuilder) WithGridSize(xDim, yDim int) {
	b.gridSize[0] = xDim
	b.gridSize[1] = yDim
}

func (b *PtDirectGPUBuilder) buildChipRDMAEngine(
	chiplet *Chiplet,
) {
	chiplet.chipRdmaEngine = rdma.NewPtDirectRdmaEngine(
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

func (b *PtDirectGPUBuilder) connectRDMAEngines() {
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

	ptDirectConn := switching.NewDirectConnectionWithLatency(
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

		directPwReqPort := b.chiplets[i].chipRdmaEngine.(*rdma.PtDirectRdmaEngine).DirectPwReqPort
		directPwRspPort := b.chiplets[i].chipRdmaEngine.(*rdma.PtDirectRdmaEngine).DirectPwRspPort

		ptDirectConn.PlugIn(directPwReqPort, 4*b.numReqPerCycle)
		ptDirectConn.PlugIn(directPwRspPort, 4*b.numReqPerCycle)

		directChipRdmaReqAddrTable.LowModules = append(
			directChipRdmaReqAddrTable.LowModules,
			directPwReqPort)
		directChipRdmaRspAddrTable[directPwReqPort] = directPwRspPort
	}

	for i := uint64(0); i < b.numChiplets; i++ {
		b.chiplets[i].chipRdmaEngine.SetRdmaReqAddressTable(chipRdmaReqAddrTable)
		b.chiplets[i].chipRdmaEngine.SetRdmaRspAddressTable(chipRdmaRspAddrTable)

		b.chiplets[i].chipRdmaEngine.(*rdma.PtDirectRdmaEngine).DirectPtRdmaReqAddressTable = directChipRdmaReqAddrTable
		b.chiplets[i].chipRdmaEngine.(*rdma.PtDirectRdmaEngine).DirectPtRdmaRspAddressTable = directChipRdmaRspAddrTable

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

func (b PtDirectGPUBuilder) setupSwPortsForRdmaEngine() {
	swReqPorts := b.gpu.InterChipletNetwork[0].(*chipnetwork.InterChipletMeshConnector).
		GetSwReqPorts()
	swRspPorts := b.gpu.InterChipletNetwork[0].(*chipnetwork.InterChipletMeshConnector).
		GetSwRspPorts()

	for _, chiplet := range b.chiplets {
		chiplet.chipRdmaEngine.(*rdma.PtDirectRdmaEngine).SetSwReqPort(swReqPorts[chiplet.ChipletID])
		chiplet.chipRdmaEngine.(*rdma.PtDirectRdmaEngine).SetSwRspPort(swRspPorts[chiplet.ChipletID])
	}
}

func (b PtDirectGPUBuilder) Build(name string, id uint64) *mgpusim.GPU {
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
	b.setupMeshInterchipNetwork()
	b.setupSwPortsForRdmaEngine()

	return b.gpu
}

func (b *PtDirectGPUBuilder) setupMeshInterchipNetwork() {
	fmt.Println("Mesh Builder")

	// interconnect bandwidth: 1GHz * 64B * 12 = 768 GBps (example)
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
	b.gpu.SwitchConnection = chipMeshConnector.SwitchConnection
}

func (b *PtDirectGPUBuilder) GetRdmaReqPorts() map[uint64]akita.Port {
	chipletPorts := make(map[uint64]akita.Port)

	for _, chiplet := range b.chiplets {
		chipletPorts[chiplet.ChipletID] = chiplet.chipRdmaEngine.GetRequestPort()
	}

	return chipletPorts
}

func (b *PtDirectGPUBuilder) GetRdmaRspPorts() map[uint64]akita.Port {
	chipletPorts := make(map[uint64]akita.Port)

	for _, chiplet := range b.chiplets {
		chipletPorts[chiplet.ChipletID] = chiplet.chipRdmaEngine.GetResponsePort()
	}

	return chipletPorts
}
