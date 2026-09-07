package builders

import (
	"fmt"

	"gitlab.com/akita/akita"
	"gitlab.com/akita/lmf"
	"gitlab.com/akita/mem"
	"gitlab.com/akita/mem/cache/writeback"
	"gitlab.com/akita/mem/idealmemcontroller"
	"gitlab.com/akita/mgpusim"
	"gitlab.com/akita/noc/networking/chipnetwork"
)

type InfLLCGPUBuilder struct {
	*CommonBuilder
	gridSize [2]int
}

func MakeInfLLCGPUBuilder() InfLLCGPUBuilder {
	cbp := CommonBuilder{}
	b := InfLLCGPUBuilder{CommonBuilder: &cbp}
	b.SetDefaultCommonBuilderParams()
	return b
}

func (b *InfLLCGPUBuilder) WithGridSize(xDim, yDim int) {
	b.gridSize[0] = xDim
	b.gridSize[1] = yDim
}

func (b *InfLLCGPUBuilder) connectRDMAEngines() {
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
		l2Cache := l2Cache.(*writeback.Cache)
		l2Cache.SetVirtualAddrTable(chipRdmaReqAddrTable)
		l2Cache.SetHomeNodeFinder(homeNodeFinder)
	}
}

func (b *InfLLCGPUBuilder) buildMemBanks(
	chiplet *Chiplet,
) {
	// memCtrlBuilder := b.createDRAMControllerBuilder()
	l2Builder := writeback.MakeBuilder().
		WithEngine(b.engine).
		WithFreq(b.freq).
		WithLog2BlockSize(b.log2CacheLineSize).
		WithWayAssociativity(1024).
		WithTotalByteSize(16 * mem.MB). // 256MB llc per chiplet
		WithNumBanks(int(b.numMemoryBankPerChiplet)).
		WithNumMSHREntry(32).   // 32 per bank × 32 banks = 1024 per chiplet (realistic vs Accel-Sim 32–64)
		WithNumMSHRMaxMerge(8). // max 8 merges/entry (Accel-Sim 관례)
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

func (b InfLLCGPUBuilder) Build(name string, id uint64) *mgpusim.GPU {
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
		}
	}

	b.connectRDMAEngines()

	b.buildPageMigrationController()
	b.setupDMA()

	b.connectCP()
	b.setupMeshInterchipNetwork()

	return b.gpu
}

func (b *InfLLCGPUBuilder) setupMeshInterchipNetwork() {
	fmt.Println("Mesh Builder")

	// interconnect bandwidth: 1GHz * 64B * 12 = 768 GBps (example)
	chipMeshConnector := chipnetwork.NewInterChipletMeshConnector().
		WithEngine(b.engine).
		WithSwitchLatency(b.switchLatency). // 32ns
		WithFreq(1*akita.GHz).
		WithFlitByteSize(64).
		WithNumReqPerCycle(b.numReqPerCycle).
		WithGridSize(b.gridSize[0], b.gridSize[1]).
		WithNumChiplets(b.gridSize[0] * b.gridSize[1]).
		WithRDMAReqPorts(b.GetRdmaReqPorts()).
		WithRDMARspPorts(b.GetRdmaRspPorts()).
		WithNetworkName("ICN")

	chipMeshConnector.CreateNetwork()

	b.gpu.InterChipletNetwork = append(b.gpu.InterChipletNetwork, chipMeshConnector)
}

func (b *InfLLCGPUBuilder) GetRdmaReqPorts() map[uint64]akita.Port {
	chipletPorts := make(map[uint64]akita.Port)

	for _, chiplet := range b.chiplets {
		chipletPorts[chiplet.ChipletID] = chiplet.chipRdmaEngine.GetRequestPort()
	}

	return chipletPorts
}

func (b *InfLLCGPUBuilder) GetRdmaRspPorts() map[uint64]akita.Port {
	chipletPorts := make(map[uint64]akita.Port)

	for _, chiplet := range b.chiplets {
		chipletPorts[chiplet.ChipletID] = chiplet.chipRdmaEngine.GetResponsePort()
	}

	return chipletPorts
}
