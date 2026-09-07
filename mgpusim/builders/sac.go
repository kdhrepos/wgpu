package builders

import (
	"fmt"

	"gitlab.com/akita/akita"
	"gitlab.com/akita/lmf"
	"gitlab.com/akita/mem"
	"gitlab.com/akita/mem/cache/writebacksac"
	"gitlab.com/akita/mem/idealmemcontroller"
	"gitlab.com/akita/mgpusim"
	"gitlab.com/akita/mgpusim/rdma"
	"gitlab.com/akita/mgpusim/sac"
	"gitlab.com/akita/noc/networking/chipnetwork"
	"gitlab.com/akita/util/global"
)

// SACGPUBuilder builds the SAC (Sharing-Aware Caching, ISCA'23) platform: a
// mesh of chiplets identical to the mesh platform, except that the L2
// caches are the reconfigurable writebacksac variant and a SAC controller
// profiles each kernel and switches the LLC between the memory-side and
// SM-side organizations.
type SACGPUBuilder struct {
	*CommonBuilder
	gridSize [2]int

	sacController *sac.Controller
}

func MakeSACGPUBuilder() SACGPUBuilder {
	cbp := CommonBuilder{}
	b := SACGPUBuilder{CommonBuilder: &cbp}
	b.SetDefaultCommonBuilderParams()
	return b
}

func (b *SACGPUBuilder) WithGridSize(xDim, yDim int) {
	b.gridSize[0] = xDim
	b.gridSize[1] = yDim
}

// SACController returns the controller built during Build (for wiring the
// driver-side kernel-completion observer).
func (b *SACGPUBuilder) SACController() *sac.Controller {
	return b.sacController
}

// buildMemBanksSAC mirrors CommonBuilder.buildMemBanks but instantiates the
// writebacksac L2 variant.
func (b *SACGPUBuilder) buildMemBanksSAC(
	chiplet *Chiplet,
) {
	l2Builder := writebacksac.MakeBuilder().
		WithEngine(b.engine).
		WithFreq(b.freq).
		WithLog2BlockSize(b.log2CacheLineSize).
		WithWayAssociativity(16).
		WithTotalByteSize(128 * mem.KB). // 16 banks -> 4MB, totally
		WithNumBanks(int(b.numMemoryBankPerChiplet)).
		WithNumMSHREntry(32).
		WithNumMSHRMaxMerge(8).
		WithNumReqPerCycle(5).
		WithChipletId(int(chiplet.ChipletID))

	for i := uint64(0); i < b.numMemoryBankPerChiplet; i++ {
		dramName := fmt.Sprintf("%s.DRAM_%d", chiplet.name, i)
		// DualStripeConverter compacts external addresses to a per-bank
		// internal offset, so each bank only needs memoryPerBank storage.
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

func (b *SACGPUBuilder) connectRDMAEngines() {
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
		l2Cache := l2Cache.(*writebacksac.Cache)
		l2Cache.SetVirtualAddrTable(chipRdmaReqAddrTable)
		l2Cache.SetHomeNodeFinder(homeNodeFinder)
	}

	// used for pte access trace
	for _, mmu := range b.gpu.MMUs {
		mmu.VirtualAddrTable = chipRdmaReqAddrTable
	}
}

// buildSACController configures the global SAC state, builds the
// controller, plugs it into the CP's internal connection (which already
// hosts the L2 control ports), and registers it as the CP's kernel
// observer.
func (b *SACGPUBuilder) buildSACController() {
	s := global.GetInstance().SAC()
	s.Enabled = true
	s.Mode = global.SACMemorySide
	s.NumChiplets = int(b.numChiplets)
	s.NumBanksPerChiplet = int(b.numMemoryBankPerChiplet)
	s.ResetCounters()

	window := s.ProfileWindow
	if window <= 0 {
		window = 2000
	}
	theta := s.Theta
	if theta == 0 {
		theta = 0.05
	}

	// Default EAB constants derived from the platform parameters (per
	// chiplet, GB/s at 1 GHz): the L2 banks can deliver one cache line per
	// cycle each; the inter-chiplet mesh injects numReqPerCycle 16B flits
	// per cycle per chiplet.
	lineBytes := float64(uint64(1) << b.log2CacheLineSize)
	bLLC := float64(b.numMemoryBankPerChiplet) * lineBytes
	bIntra := bLLC
	bMem := bLLC
	bInter := float64(b.numReqPerCycle) * 16

	pick := func(override, computed float64) float64 {
		if override > 0 {
			return override
		}
		return computed
	}

	b.sacController = sac.MakeBuilder().
		WithEngine(b.engine).
		WithFreq(b.freq).
		WithProfileWindow(window).
		WithTheta(theta).
		WithForcedMode(s.ForcedMode).
		WithBandwidths(
			pick(s.BIntraGBs, bIntra),
			pick(s.BInterGBs, bInter),
			pick(s.BLLCGBs, bLLC),
			pick(s.BMemGBs, bMem)).
		Build(b.gpuName + ".SACController")

	for _, l2 := range b.gpu.L2Caches {
		l2sac := l2.(*writebacksac.Cache)
		b.sacController.RegisterL2(l2sac.GetControlPort(), l2sac)
	}
	b.sacController.FinalizePorts()

	b.internalConn.PlugIn(b.sacController.ControlPort, 16)
	b.cp.SetKernelObserver(b.sacController)
}

func (b *SACGPUBuilder) setupSwPortsForRdmaEngine() {
	swReqPorts := b.gpu.InterChipletNetwork[0].(*chipnetwork.InterChipletMeshConnector).
		GetSwReqPorts()
	swRspPorts := b.gpu.InterChipletNetwork[0].(*chipnetwork.InterChipletMeshConnector).
		GetSwRspPorts()

	for _, chiplet := range b.chiplets {
		chiplet.chipRdmaEngine.(*rdma.RDMAEngineImpl).SetSwReqPort(swReqPorts[chiplet.ChipletID])
		chiplet.chipRdmaEngine.(*rdma.RDMAEngineImpl).SetSwRspPort(swRspPorts[chiplet.ChipletID])
	}
}

func (b *SACGPUBuilder) Build(name string, id uint64) *mgpusim.GPU {
	b.createGPU(name, id)

	b.buildCP()

	for r := 0; r < b.gridSize[1]; r++ { // row
		for c := 0; c < b.gridSize[0]; c++ { // column
			chipletName := fmt.Sprintf("%s.chiplet_%02d", b.gpuName, r*b.gridSize[0]+c)
			chipletId := uint64(r*b.gridSize[0] + c)
			chiplet := NewChiplet(chipletName, chipletId)

			b.buildSAs(chiplet)         // L1 cache and TLB are built in here.
			b.buildMemBanksSAC(chiplet) // writebacksac L2 variant.
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

			b.setConnectionToDriver(chiplet)
		}
	}

	b.connectRDMAEngines()

	b.buildPageMigrationController()
	b.setupDMA()

	b.connectCP()
	b.buildSACController()
	b.setupMeshInterchipNetwork()
	b.setupSwPortsForRdmaEngine()

	return b.gpu
}

func (b *SACGPUBuilder) setupMeshInterchipNetwork() {
	fmt.Println("SAC Builder (mesh interconnect)")

	chipMeshConnector := chipnetwork.NewInterChipletMeshConnector().
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

	chipMeshConnector.CreateNetwork()

	b.gpu.InterChipletNetwork = append(b.gpu.InterChipletNetwork, chipMeshConnector)
	b.gpu.SwitchConnection = chipMeshConnector.SwitchConnection
}

func (b *SACGPUBuilder) GetRdmaReqPorts() map[uint64]akita.Port {
	chipletPorts := make(map[uint64]akita.Port)

	for _, chiplet := range b.chiplets {
		chipletPorts[chiplet.ChipletID] = chiplet.chipRdmaEngine.GetRequestPort()
	}

	return chipletPorts
}

func (b *SACGPUBuilder) GetRdmaRspPorts() map[uint64]akita.Port {
	chipletPorts := make(map[uint64]akita.Port)

	for _, chiplet := range b.chiplets {
		chipletPorts[chiplet.ChipletID] = chiplet.chipRdmaEngine.GetResponsePort()
	}

	return chipletPorts
}
