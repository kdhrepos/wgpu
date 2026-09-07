package builders

import (
	"fmt"

	"gitlab.com/akita/akita"
	"gitlab.com/akita/lmf"
	"gitlab.com/akita/mem/cache/writeback"
	"gitlab.com/akita/mgpusim"
	"gitlab.com/akita/mgpusim/rdma"
	"gitlab.com/akita/noc/networking/chipnetwork"
)

// DataIdealGPUBuilder is the symmetric inverse of PtIdealGPUBuilder. It routes
// regular data inter-chiplet traffic over a parallel ideal 1-hop crossbar
// while keeping PTW (page-table-walk) traffic on the realistic mesh. The
// traffic split happens in DataIdealRdmaEngine based on the PTW flag carried
// by the request; this builder simply wires the data ports to the ideal NoC
// and the PTW ports to the mesh NoC.
type DataIdealGPUBuilder struct {
	*CommonBuilder
	gridSize [2]int
}

func MakeDataIdealGPUBuilder() DataIdealGPUBuilder {
	cbp := CommonBuilder{}
	b := DataIdealGPUBuilder{CommonBuilder: &cbp}
	b.SetDefaultCommonBuilderParams()
	return b
}

func (b *DataIdealGPUBuilder) WithGridSize(xDim, yDim int) {
	b.gridSize[0] = xDim
	b.gridSize[1] = yDim
}

// buildChipRDMAEngine overrides the common implementation so each chiplet
// receives a DataIdealRdmaEngine (which carries the parallel PTW ports) in
// place of the plain RDMAEngineImpl.
func (b *DataIdealGPUBuilder) buildChipRDMAEngine(chiplet *Chiplet) {
	chiplet.chipRdmaEngine = rdma.NewDataIdealRdmaEngine(
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

// connectRDMAEngines installs two parallel RDMA address-table sets on each
// chiplet's RDMA engine: the regular table (pointing at remote chiplets'
// RequestPort, for data) plus a PTW-only table (pointing at remote chiplets'
// PtwReqPort). The shared response-address table accepts both source
// ports so both networks can deliver responses back to the right rsp port.
func (b *DataIdealGPUBuilder) connectRDMAEngines() {
	// Regular (data) request address table -> remote chiplet's RequestPort
	chipRdmaReqAddrTable := lmf.NewStripedLowModuleFinder(
		b.memAddrOffset,
		uint64(b.numChiplets),
		b.chipletInterleavingSize())

	// PTW request address table -> remote chiplet's PtwReqPort
	chipRdmaReqAddrTablePtw := lmf.NewStripedLowModuleFinder(
		b.memAddrOffset,
		uint64(b.numChiplets),
		b.chipletInterleavingSize())

	// Shared response routing map: any incoming request port (ideal or mesh)
	// maps to that same chiplet's matching response port.
	chipRdmaRspAddrTable := make(map[akita.Port]akita.Port)

	for i := uint64(0); i < b.numChiplets; i++ {
		eng := b.chiplets[i].chipRdmaEngine.(*rdma.DataIdealRdmaEngine)

		chipRdmaReqAddrTable.LowModules = append(
			chipRdmaReqAddrTable.LowModules,
			eng.GetRequestPort())
		chipRdmaReqAddrTablePtw.LowModules = append(
			chipRdmaReqAddrTablePtw.LowModules,
			eng.GetPtwReqPort())

		chipRdmaRspAddrTable[eng.GetRequestPort()] = eng.GetResponsePort()
		chipRdmaRspAddrTable[eng.GetPtwReqPort()] = eng.GetPtwRspPort()
	}

	for i := uint64(0); i < b.numChiplets; i++ {
		eng := b.chiplets[i].chipRdmaEngine.(*rdma.DataIdealRdmaEngine)
		eng.SetRdmaReqAddressTable(chipRdmaReqAddrTable)
		eng.SetPtwRdmaReqAddressTable(chipRdmaReqAddrTablePtw)
		eng.SetRdmaRspAddressTable(chipRdmaRspAddrTable)

		b.gpu.ChipRDMAEngines = append(b.gpu.ChipRDMAEngines, eng)
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

func (b DataIdealGPUBuilder) setupSwPortsForRdmaEngine() {
	swReqPorts := b.gpu.InterChipletNetwork[0].(*chipnetwork.InterChipletIdealConnector).
		GetSwReqPorts()
	swRspPorts := b.gpu.InterChipletNetwork[0].(*chipnetwork.InterChipletIdealConnector).
		GetSwRspPorts()

	for _, chiplet := range b.chiplets {
		eng := chiplet.chipRdmaEngine.(*rdma.DataIdealRdmaEngine)
		eng.SetSwReqPort(swReqPorts[chiplet.ChipletID])
		eng.SetSwRspPort(swRspPorts[chiplet.ChipletID])
	}
}

func (b DataIdealGPUBuilder) Build(name string, id uint64) *mgpusim.GPU {
	b.createGPU(name, id)

	b.buildCP()

	for r := 0; r < b.gridSize[1]; r++ { // row
		for c := 0; c < b.gridSize[0]; c++ { // column
			chipletName := fmt.Sprintf("%s.chiplet_%02d", b.gpuName, r*b.gridSize[0]+c)
			chipletId := uint64(r*b.gridSize[0] + c)
			chiplet := NewChiplet(chipletName, chipletId)

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
	b.setupIdealInterchipNetworkForData()
	b.setupMeshInterchipNetworkForPtw()
	b.setupSwPortsForRdmaEngine()

	return b.gpu
}

// setupIdealInterchipNetworkForData wires the regular (data) RequestPort /
// ResponsePort of each chiplet's RDMA engine into an ideal 1-hop crossbar.
// This exposes data traffic to a near-zero-distance network independent of
// the PTW mesh.
func (b *DataIdealGPUBuilder) setupIdealInterchipNetworkForData() {
	fmt.Println("DataIdeal Builder: data ideal crossbar")

	chipIdealConnector := chipnetwork.NewInterChipletIdealConnector().
		WithEngine(b.engine).
		WithSwitchLatency(b.switchLatency).
		WithFreq(1*akita.GHz).
		WithFlitByteSize(16).
		WithNumReqPerCycle(b.numReqPerCycle).
		WithGridSize(b.gridSize[0], b.gridSize[1]).
		WithNumChiplets(b.gridSize[0] * b.gridSize[1]).
		WithRDMAReqPorts(b.GetRdmaReqPorts()).
		WithRDMARspPorts(b.GetRdmaRspPorts()).
		WithNetworkName("ICN")

	chipIdealConnector.CreateNetwork()

	b.gpu.InterChipletNetwork = append(b.gpu.InterChipletNetwork, chipIdealConnector)
}

// setupMeshInterchipNetworkForPtw wires the PTW-only PtwReqPort / PtwRspPort
// of each chiplet's RDMA engine into a realistic mesh NoC. PTW traffic thus
// pays the mesh hop-count penalty while data stays on the ideal crossbar.
func (b *DataIdealGPUBuilder) setupMeshInterchipNetworkForPtw() {
	fmt.Println("DataIdeal Builder: PTW mesh")

	chipMeshConnector := chipnetwork.NewInterChipletMeshConnector().
		WithEngine(b.engine).
		WithSwitchLatency(b.switchLatency).
		WithFreq(1*akita.GHz).
		WithFlitByteSize(16).
		WithNumReqPerCycle(b.numReqPerCycle).
		WithGridSize(b.gridSize[0], b.gridSize[1]).
		WithNumChiplets(b.gridSize[0] * b.gridSize[1]).
		WithRDMAReqPorts(b.GetRdmaReqPortsPtw()).
		WithRDMARspPorts(b.GetRdmaRspPortsPtw()).
		WithNetworkName("ICN_PTW")

	chipMeshConnector.CreateNetwork()

	b.gpu.SwitchConnection = chipMeshConnector.SwitchConnection

	// Append the PTW network as the second entry of InterChipletNetwork so
	// tracers attached to every network pick up PTW traffic on the mesh in
	// addition to data traffic on the ideal crossbar (entry 0).
	b.gpu.InterChipletNetwork = append(b.gpu.InterChipletNetwork, chipMeshConnector)
}

func (b *DataIdealGPUBuilder) GetRdmaReqPorts() map[uint64]akita.Port {
	chipletPorts := make(map[uint64]akita.Port)
	for _, chiplet := range b.chiplets {
		chipletPorts[chiplet.ChipletID] = chiplet.chipRdmaEngine.GetRequestPort()
	}
	return chipletPorts
}

func (b *DataIdealGPUBuilder) GetRdmaRspPorts() map[uint64]akita.Port {
	chipletPorts := make(map[uint64]akita.Port)
	for _, chiplet := range b.chiplets {
		chipletPorts[chiplet.ChipletID] = chiplet.chipRdmaEngine.GetResponsePort()
	}
	return chipletPorts
}

func (b *DataIdealGPUBuilder) GetRdmaReqPortsPtw() map[uint64]akita.Port {
	chipletPorts := make(map[uint64]akita.Port)
	for _, chiplet := range b.chiplets {
		eng := chiplet.chipRdmaEngine.(*rdma.DataIdealRdmaEngine)
		chipletPorts[chiplet.ChipletID] = eng.GetPtwReqPort()
	}
	return chipletPorts
}

func (b *DataIdealGPUBuilder) GetRdmaRspPortsPtw() map[uint64]akita.Port {
	chipletPorts := make(map[uint64]akita.Port)
	for _, chiplet := range b.chiplets {
		eng := chiplet.chipRdmaEngine.(*rdma.DataIdealRdmaEngine)
		chipletPorts[chiplet.ChipletID] = eng.GetPtwRspPort()
	}
	return chipletPorts
}
