package builders

import (
	"fmt"

	"gitlab.com/akita/akita"
	"gitlab.com/akita/lmf"
	"gitlab.com/akita/mem"
	"gitlab.com/akita/mem/cache"
	"gitlab.com/akita/mem/cache/writeback"
	"gitlab.com/akita/mem/idealmemcontroller"

	// "gitlab.com/akita/mem/cache/writeback"

	"gitlab.com/akita/mem/device"
	"gitlab.com/akita/mem/vm/addresstranslator"
	"gitlab.com/akita/mem/vm/mmu"
	"gitlab.com/akita/mem/vm/tlb"
	"gitlab.com/akita/mgpusim"
	"gitlab.com/akita/mgpusim/pagemigrationcontroller"
	"gitlab.com/akita/mgpusim/rdma"
	"gitlab.com/akita/mgpusim/timing/caches"
	"gitlab.com/akita/mgpusim/timing/caches/rob"
	"gitlab.com/akita/mgpusim/timing/cp"
	"gitlab.com/akita/mgpusim/timing/cu"
	"gitlab.com/akita/util/global"
	"gitlab.com/akita/util/tracing"
)

type Builder interface {
	Build(string, uint64) *mgpusim.GPU
	WithMemAddrOffset(uint64)
	WithISADebugging()
	WithTLBTracer(tracing.Tracer)
	WithMemTracer(tracing.Tracer)
	WithVisTracer(tracing.Tracer)

	WithEngine(akita.Engine)
	WithNumCUPerShaderArray(uint64)
	WithNumShaderArrayPerChiplet(uint64)
	WithNumMemoryBankPerChiplet(uint64)
	WithNumChiplets(uint64)
	WithTotalMem(uint64)
	CalculateMemoryParameters()
	WithLog2PageSize(uint64)
	WithPageTable(*device.PageTableImpl)
	WithAlg(string)
	WithPteCaching(bool)
	WithSchedulingPartition(string)
	WithDataPlacementPartition(string)
	WithDataPlacementStride(int)
}

// Every component, whether used in a specific builder or not, appears here.
// Further, every componenet appears in gpu.go
// Finally, nil the components that aren't going to be used in a specific builder.
type CommonBuilder struct {
	engine                         akita.Engine
	freq                           akita.Freq
	memAddrOffset                  uint64
	totalMem                       uint64
	mmu                            *mmu.MMUImpl
	numChiplets                    uint64
	numShaderArrayPerChiplet       uint64
	numCUPerShaderArray            uint64
	numMemoryBankPerChiplet        uint64
	numReqPerCycle                 int
	memoryPerChiplet               uint64
	memoryPerBank                  uint64
	log2PageSize                   uint64
	log2CacheLineSize              uint64
	log2MemoryBankInterleavingSize uint64 // bank-level stripe (typically cacheline)
	remoteTLBInterleavingSize      uint64
	pageTable                      *device.PageTableImpl

	usePteCaching bool

	enableISADebugging bool
	enableMemTracing   bool
	enableTLBTracing   bool
	enableVisTracing   bool
	disableProgressBar bool
	visTracer          tracing.Tracer
	tlbTracer          tracing.Tracer
	memTracer          tracing.Tracer

	gpuName           string
	gpu               *mgpusim.GPU
	cp                *cp.CommandProcessor
	chiplets          []*Chiplet
	cus               []*cu.ComputeUnit
	l1vReorderBuffers []*rob.ReorderBuffer
	l1iReorderBuffers []*rob.ReorderBuffer
	l1sReorderBuffers []*rob.ReorderBuffer
	l1vCaches         []caches.L1Cache
	l1sCaches         []caches.L1Cache
	l1iCaches         []caches.L1Cache
	l2Caches          []cache.Cache
	// l2Caches          []*writeback.Cache
	l1vAddrTrans []addresstranslator.AddressTranslator
	l1sAddrTrans []addresstranslator.AddressTranslator
	l1iAddrTrans []addresstranslator.AddressTranslator
	l1vTLBs      []*tlb.TLB
	l1sTLBs      []*tlb.TLB
	l1iTLBs      []*tlb.TLB
	l2TLBs       []tlb.L2TLB
	// drams        []*dram.MemController
	drams                   []*idealmemcontroller.Comp
	lowModuleFinderForL1    lmf.LowModuleFinder
	lowModuleFinderForL2    *lmf.InterleavedLowModuleFinder
	lowModuleFinderForPMC   *lmf.InterleavedLowModuleFinder
	dmaEngine               *cp.DMAEngine
	rdmaEngine              rdma.RDMAEngine
	pageMigrationController *pagemigrationcontroller.PageMigrationController

	internalConn *akita.DirectConnection

	alg                    string
	schedPartition         string
	dataPlacementPartition string
	dataPlacementStride    int

	switchLatency int

	reqWithPC bool
}

// MakeCommonBuilder provides a GPU builder that can builds the MCM GPU.
// TODO: need to modify interleaving granularity
func (b *CommonBuilder) SetDefaultCommonBuilderParams() {
	b.freq = 1 * akita.GHz
	b.numShaderArrayPerChiplet = 8
	b.numCUPerShaderArray = 4
	b.numMemoryBankPerChiplet = 32
	b.log2CacheLineSize = 6
	b.log2PageSize = 12
	// Dual-stripe defaults:
	//   bank stripe    = cacheline (64B) — BLP within a chiplet
	//   chiplet stripe = numBanksPerChiplet × pageSize (= 64KB by default,
	//                    derived on demand via chipletInterleavingSize()).
	// The chiplet stripe is NOT a separate tunable — it must equal the LASP
	// allocator's per-chiplet chunk size in setInitialAddress
	// (mem/device/devicelaspmemstate.go), which is always
	// numBankPerChiplet × pageSize.
	b.log2MemoryBankInterleavingSize = b.log2CacheLineSize

	// this ought not to be in common
	b.remoteTLBInterleavingSize = 512
}

// WithEngine sets the engine that the GPU use.
func (b *CommonBuilder) WithEngine(engine akita.Engine) {
	b.engine = engine
}

// WithFreq sets the frequency that the GPU works at.
func (b *CommonBuilder) WithFreq(freq akita.Freq) {
	b.freq = freq
}

// WithMemAddrOffset sets the address of the first byte of the GPU to build.
func (b *CommonBuilder) WithMemAddrOffset(offset uint64) {
	b.memAddrOffset = offset
}

// WithTotalMem sets the total amount of memory in the GPU.
func (b *CommonBuilder) WithTotalMem(memory uint64) {
	b.totalMem = memory
}

// WithMMU sets the MMU component that provides the address translation service
// for the GPU.
func (b *CommonBuilder) WithMMU(mmu *mmu.MMUImpl) {
	b.mmu = mmu
}

// WithNumMemoryBankPerChiplet sets the number of L2 cache modules and number
// of memory controllers in each chiplet.
func (b *CommonBuilder) WithNumMemoryBankPerChiplet(n uint64) {
	b.numMemoryBankPerChiplet = n
}

// WithNumChiplet sets the number of chiplets
func (b *CommonBuilder) WithNumChiplets(n uint64) {
	b.numChiplets = n
}

// WithNumShaderArrayPerChiplet sets the number of shader arrays in each chiplet
func (b *CommonBuilder) WithNumShaderArrayPerChiplet(n uint64) {
	b.numShaderArrayPerChiplet = n
}

// WithNumCUPerShaderArray sets the number of CU and number of L1V caches in
// each Shader Array.
func (b *CommonBuilder) WithNumCUPerShaderArray(n uint64) {
	b.numCUPerShaderArray = n
}

// WithLog2MemoryBankInterleavingSize sets the number of consecutive bytes that
// are guaranteed to be on a memory bank (bank stripe — typically the cacheline
// size under dual-stripe).
func (b *CommonBuilder) WithLog2MemoryBankInterleavingSize(n uint64) {
	b.log2MemoryBankInterleavingSize = n
}

// chipletInterleavingSize returns the chiplet-level stripe in bytes
// (= numBanksPerChiplet × pageSize). Always derived — must match the LASP
// allocator's per-chiplet chunk size in
// mem/device/devicelaspmemstate.go:setInitialAddress, where each chiplet
// receives `numBankPerChiplet` consecutive pages before the allocator moves
// to the next chiplet. Any other value here would make the routing layer
// disagree with the allocator about which chiplet owns a page.
func (b *CommonBuilder) chipletInterleavingSize() uint64 {
	return b.numMemoryBankPerChiplet << b.log2PageSize
}

func (b *CommonBuilder) WithRemoteTLB(interleavingSize uint64) {
	b.remoteTLBInterleavingSize = interleavingSize
}

// WithVisTracer applies a tracer to trace all the tasks of all the GPU
// components
func (b *CommonBuilder) WithVisTracer(t tracing.Tracer) {
	b.enableVisTracing = true
	b.visTracer = t
}

// WithTLBTracer applies a tracer to trace the TLBory transactions.
func (b *CommonBuilder) WithTLBTracer(t tracing.Tracer) {
	b.enableTLBTracing = true
	b.tlbTracer = t
}

// WithMemTracer applies a tracer to trace the memory transactions.
func (b *CommonBuilder) WithMemTracer(t tracing.Tracer) {
	b.enableMemTracing = true
	b.memTracer = t
}

// WithISADebugging enables the GPU to dump instruction execution information.
func (b *CommonBuilder) WithISADebugging() {
	b.enableISADebugging = true
}

// WithoutProgressBar disables the progress bar for kernel execution
func (b *CommonBuilder) WithoutProgressBar() {
	b.disableProgressBar = true
}

// WithLog2CacheLineSize sets the cache line size with the power of 2.
func (b *CommonBuilder) WithLog2CacheLineSize(
	log2CacheLine uint64,
) {
	b.log2CacheLineSize = log2CacheLine
}

// WithLog2PageSize sets the page size with the power of 2.
func (b *CommonBuilder) WithLog2PageSize(log2PageSize uint64) {
	b.log2PageSize = log2PageSize
}

// WithPageTable sets the page size with the power of 2.
func (b *CommonBuilder) WithPageTable(pt *device.PageTableImpl) {
	b.pageTable = pt
}

// WithAlg sets the scheduling algorithm
func (b *CommonBuilder) WithAlg(alg string) {
	b.alg = alg
}

func (b *CommonBuilder) WithDataPlacementPartition(partition string) {
	b.dataPlacementPartition = partition
}

func (b *CommonBuilder) WithDataPlacementStride(stride int) {
	b.dataPlacementStride = stride
}

func (b *CommonBuilder) WithSchedulingPartition(partition string) {
	b.schedPartition = partition
}

func (b *CommonBuilder) WithNumReqPerCycle(n int) {
	b.numReqPerCycle = n
}

func (b *CommonBuilder) WithSwitchLatency(s int) {
	b.switchLatency = s
}

func (b *CommonBuilder) WithPC(p bool) {
	b.reqWithPC = p
}

func (b *CommonBuilder) WithPteCaching(pteCaching bool) {
	b.usePteCaching = pteCaching
}

// CalculateMemoryParameters calculates
// -> memoryPerChiplet
// -> memoryPerBank
// based on the totalMem, numChiplets and numMemoryBankPerChiplet
func (b *CommonBuilder) CalculateMemoryParameters() {
	b.memoryPerChiplet = b.totalMem / uint64(b.numChiplets)
	b.memoryPerBank =
		b.totalMem / (b.numChiplets * uint64(b.numMemoryBankPerChiplet))
}

func (b *CommonBuilder) createGPU(name string, id uint64) {
	b.gpuName = name

	b.gpu = mgpusim.NewGPU(b.gpuName)

	b.gpu.GPUID = id
}

func (b *CommonBuilder) buildCP() {
	builder := cp.MakeBuilder().
		WithEngine(b.engine).
		WithFreq(b.freq).
		WithAlg(b.alg).
		WithPartition(b.schedPartition). // scheduling partition
		WithNumChiplets(b.numChiplets)

	if !b.disableProgressBar {
		builder = builder.ShowProgressBar()
	}

	b.cp = builder.Build(b.gpuName + ".CommandProcessor")
	b.gpu.CommandProcessor = b.cp

	b.buildDMAEngine()
	b.buildRDMAEngine()
	b.buildPageMigrationController()
}

func (b *CommonBuilder) buildDMAEngine() {
	b.dmaEngine = cp.NewDMAEngine(
		fmt.Sprintf("%s.DMA", b.gpuName),
		b.engine,
		nil)
}

func (b *CommonBuilder) buildRDMAEngine() {
	b.rdmaEngine = rdma.NewRDMAEngine(
		fmt.Sprintf("%s.RDMA", b.gpuName),
		b.engine,
		b.numReqPerCycle,
	)
	b.gpu.RDMAEngine = b.rdmaEngine
}

func (b *CommonBuilder) buildPageMigrationController() {
	b.pageMigrationController =
		pagemigrationcontroller.NewPageMigrationController(
			fmt.Sprintf("%s.PMC", b.gpuName),
			b.engine,
			b.lowModuleFinderForPMC,
			nil)

	b.gpu.PMC = b.pageMigrationController
}

// buildSAs builds shader arrays.
func (b *CommonBuilder) buildSAs(
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

func (b *CommonBuilder) collectSAComponents(
	sa shaderArray,
	chiplet *Chiplet,
) {
	for _, cu := range sa.cus {
		b.gpu.CUs = append(b.gpu.CUs, cu)
		b.cus = append(b.cus, cu)
		chiplet.CUs = append(chiplet.CUs, cu)
	}

	for _, rob := range sa.l1vROBs {
		b.l1vReorderBuffers = append(b.l1vReorderBuffers, rob)
		b.gpu.L1VROBs = append(b.gpu.L1VROBs, rob)
		chiplet.L1VROBs = append(chiplet.L1VROBs, rob)
	}

	for _, tlb := range sa.l1vTLBs {
		b.l1vTLBs = append(b.l1vTLBs, tlb)
		b.gpu.L1VTLBs = append(b.gpu.L1VTLBs, tlb)
		chiplet.L1VTLBs = append(chiplet.L1VTLBs, tlb)
	}

	for _, l1v := range sa.l1vCaches {
		b.l1vCaches = append(b.l1vCaches, l1v)
		b.gpu.L1VCaches = append(b.gpu.L1VCaches, l1v)
		chiplet.L1VCaches = append(chiplet.L1VCaches, l1v)
	}

	for _, at := range sa.l1vATs {
		b.l1vAddrTrans = append(b.l1vAddrTrans, at)
		b.gpu.L1VAddrTranslator = append(b.gpu.L1VAddrTranslator, at)
		chiplet.L1VAddrTranslator = append(chiplet.L1VAddrTranslator, at)
	}

	b.l1sAddrTrans = append(b.l1sAddrTrans, sa.l1sAT)
	b.gpu.L1SAddrTranslator = append(b.gpu.L1SAddrTranslator, sa.l1sAT)
	chiplet.L1SAddrTranslator = append(chiplet.L1SAddrTranslator, sa.l1sAT)
	b.l1sReorderBuffers = append(b.l1sReorderBuffers, sa.l1sROB)
	b.gpu.L1SROBs = append(b.gpu.L1SROBs, sa.l1sROB)
	chiplet.L1SROBs = append(chiplet.L1SROBs, sa.l1sROB)
	b.l1sCaches = append(b.l1sCaches, sa.l1sCache)
	b.gpu.L1SCaches = append(b.gpu.L1SCaches, sa.l1sCache)
	chiplet.L1SCaches = append(chiplet.L1SCaches, sa.l1sCache)
	b.l1sTLBs = append(b.l1sTLBs, sa.l1sTLB)
	b.gpu.L1STLBs = append(b.gpu.L1STLBs, sa.l1sTLB)
	chiplet.L1STLBs = append(chiplet.L1STLBs, sa.l1sTLB)

	b.l1iAddrTrans = append(b.l1iAddrTrans, sa.l1iAT)
	b.gpu.L1IAddrTranslator = append(b.gpu.L1IAddrTranslator, sa.l1iAT)
	chiplet.L1IAddrTranslator = append(chiplet.L1IAddrTranslator, sa.l1iAT)
	b.l1iReorderBuffers = append(b.l1iReorderBuffers, sa.l1iROB)
	b.gpu.L1IROBs = append(b.gpu.L1IROBs, sa.l1iROB)
	chiplet.L1IROBs = append(chiplet.L1IROBs, sa.l1iROB)
	b.l1iCaches = append(b.l1iCaches, sa.l1iCache)
	b.gpu.L1ICaches = append(b.gpu.L1ICaches, sa.l1iCache)
	chiplet.L1ICaches = append(chiplet.L1ICaches, sa.l1iCache)
	b.l1iTLBs = append(b.l1iTLBs, sa.l1iTLB)
	b.gpu.L1ITLBs = append(b.gpu.L1ITLBs, sa.l1iTLB)
	chiplet.L1ITLBs = append(chiplet.L1ITLBs, sa.l1iTLB)
}

func (b *CommonBuilder) buildMemBanks(
	chiplet *Chiplet,
) {
	l2MSHR := 32 // default: 32 per bank × banks (realistic vs Accel-Sim 32–64)
	if ov := global.GetInstance().GetL2MSHREntry(); ov > 0 {
		l2MSHR = ov // experimental override (-l2-mshr)
	}

	l2Builder := writeback.MakeBuilder().
		WithEngine(b.engine).
		WithFreq(b.freq).
		WithLog2BlockSize(b.log2CacheLineSize).
		WithWayAssociativity(16).
		WithTotalByteSize(128 * mem.KB). // 16 banks -> 4MB, totally
		WithNumBanks(int(b.numMemoryBankPerChiplet)).
		WithNumMSHREntry(l2MSHR).
		WithNumMSHRMaxMerge(8). // max 8 merges/entry (Accel-Sim 관례)
		WithNumReqPerCycle(5).
		WithChipletId(int(chiplet.ChipletID))

	for i := uint64(0); i < b.numMemoryBankPerChiplet; i++ {
		dramName := fmt.Sprintf("%s.DRAM_%d", chiplet.name, i)
		// Capacity = per-bank size; DualStripeConverter compacts external
		// addresses to [0, bankSize). Storage uses its default unitSize
		// (bankSize/16), keeping host RAM bounded by allocated units
		// rather than touched-cacheline count.
		dram := idealmemcontroller.New(
			dramName, b.engine, b.memoryPerBank)
		// Modulo dual-stripe converter: bank = (addr/bankStripe) % N.
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

func (b *CommonBuilder) buildMMU(
	chiplet *Chiplet,
) {
	mmuBuilder := mmu.MakeBuilder().
		WithEngine(b.engine).
		WithFreq(1 * akita.GHz).
		WithLog2PageSize(b.log2PageSize).
		WithPageTable(b.pageTable).
		WithNumChiplets(uint64(b.numChiplets)).
		WithMaxNumReqInFlight(4096) // IDEAL-TRANSLATION: was 64 (no page-walk serialization)
	chiplet.MMU = mmuBuilder.Build(fmt.Sprintf("%s.MMU", chiplet.name))
	chiplet.MMU.CommandProcessor = b.gpu.CommandProcessor.ToMMUs
	b.gpu.MMUs = append(b.gpu.MMUs, chiplet.MMU)
}

func (b *CommonBuilder) buildL2TLB(
	chiplet *Chiplet,
) {
	// IDEAL-TRANSLATION: the L2 TLB resolves every lookup straight from the page
	// table (always hit — see idealtlbwithlatency.go), so an L1 TLB miss is
	// answered HERE and NEVER reaches the MMU / page-walk / DRAM PTE fetch. The
	// `latency` cycles (pipeline) are the only address-translation cost left on
	// an L1 miss — set 1~10 to taste. flush/restart is implemented by
	// IdealLatTLB, so the command processor's TLB-flush handshake still
	// completes (no deadlock). The MMU/PWC are still built but see no traffic.
	builder := tlb.MakeIdealLatTLBBuilder().
		WithEngine(b.engine).
		WithFreq(b.freq).
		WithNumReqPerCycle(256).
		WithLatency(1). // IDEAL-TRANSLATION: fixed L2 TLB latency in cycles (1~10)
		WithPageTable(b.pageTable)

	l2TLB := builder.Build(fmt.Sprintf("%s.L2TLB", chiplet.name))

	b.l2TLBs = append(b.l2TLBs, l2TLB)
	b.gpu.L2TLBs = append(b.gpu.L2TLBs, l2TLB)
	chiplet.L2TLBs = append(chiplet.L2TLBs, l2TLB)
}

func (b *CommonBuilder) buildChipRDMAEngine(
	chiplet *Chiplet,
) {
	chiplet.chipRdmaEngine = rdma.NewRDMAEngine(
		fmt.Sprintf("%s.ChipRDMA", chiplet.name),
		b.engine,
		b.numReqPerCycle,
	)

	translationModuleFinderForRDMA := lmf.NewStripedLowModuleFinder(
		b.memAddrOffset,
		b.numMemoryBankPerChiplet,
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

func (b *CommonBuilder) connectL1ToL2(
	chiplet *Chiplet,
) {
	lowModuleFinder := lmf.NewStripedLowModuleFinder(
		b.memAddrOffset,
		uint64(b.numMemoryBankPerChiplet),
		1<<b.log2MemoryBankInterleavingSize,
	)

	l1ToL2Conn := akita.NewDirectConnection(chiplet.name+".L1-L2",
		b.engine, b.freq)

	for _, l2 := range chiplet.L2Caches {
		lowModuleFinder.LowModules = append(
			lowModuleFinder.LowModules,
			l2.GetTopPort())
		l1ToL2Conn.PlugIn(l2.GetTopPort(), 16*l2.GetNumReqPerCycle())
	}

	for _, l1v := range chiplet.L1VCaches {
		l1v.SetLowModuleFinder(lowModuleFinder) // L2 Top Ports
		l1ToL2Conn.PlugIn(l1v.GetBottomPort(), 16)
	}

	for _, l1s := range chiplet.L1SCaches {
		l1s.SetLowModuleFinder(lowModuleFinder) // L2 Top Ports
		l1ToL2Conn.PlugIn(l1s.GetBottomPort(), 16)
	}

	for _, l1i := range chiplet.L1ICaches {
		l1i.SetLowModuleFinder(lowModuleFinder) // L2 Top Ports
		l1ToL2Conn.PlugIn(l1i.GetBottomPort(), 16)
	}

	chiplet.lowModuleFinderForL1 = lowModuleFinder // ports from L2 caches
	chiplet.L1ToL2Connection = l1ToL2Conn

	b.gpu.L1ToL2Connection = append(b.gpu.L1ToL2Connection, l1ToL2Conn)
}

// This connect L2 cache with DRAM
func (b *CommonBuilder) connectL2ToDram(
	chiplet *Chiplet,
) {
	// lowmodulefinder for pmc below
	lowModuleFinderForPMC := lmf.NewInterleavedLowModuleFinder(
		1 << b.log2MemoryBankInterleavingSize)

	chiplet.L2ToDramConnection = akita.NewDirectConnection(
		chiplet.name+".L2-DRAM", b.engine, b.freq)

	lowModuleFinderForL2 := make([]*lmf.SingleVRemoteLowModuleFinder, len(chiplet.L2Caches))

	// DRAMs should be connected to l2 cache and mmu
	// mmu is connected at connectMMUToBottom function
	for i, dram := range chiplet.DRAMs {
		chiplet.L2ToDramConnection.PlugIn(dram.ToTop, 2*dram.MaxNumTransaction)
		lowModuleFinderForPMC.LowModules = append(lowModuleFinderForPMC.LowModules, dram.ToTop)

		lowModuleFinderForL2[i] = lmf.NewDualStripeSingleVRemoteLowModuleFinder(
			dram.ToTop,
			// L2 sends data requests through its bottom port.
			// Responses go to ToL2Top but it exists at transaction buffer.
			chiplet.chipRdmaEngine.GetToL2Bottom(),
			b.memAddrOffset,
			b.numChiplets,
			b.numMemoryBankPerChiplet,
			b.chipletInterleavingSize(),
			1<<b.log2MemoryBankInterleavingSize,
			uint64(b.numMemoryBankPerChiplet)*chiplet.ChipletID+uint64(i))
	}

	for i, l2Cache := range chiplet.L2Caches {
		chiplet.L2ToDramConnection.PlugIn(l2Cache.GetBottomToDramPort(),
			2*l2Cache.GetNumReqPerCycle())
		l2Cache.SetLowModuleFinder(lowModuleFinderForL2[i])
	}

	// chip rdma engines' local module should be L2 cache
	// Top and Bottom ports are both needed.
	b.pageMigrationController.MemCtrlFinder = lowModuleFinderForPMC
	chiplet.L2ToDramConnection.PlugIn(b.pageMigrationController.LocalMemPort, 16)
}

// This connect L2 cache with RDMA
func (b *CommonBuilder) connectL2ToRdma(
	chiplet *Chiplet,
) {
	localModuleFinderForRDMA := lmf.NewStripedLowModuleFinder(
		b.memAddrOffset,
		b.numMemoryBankPerChiplet,
		1<<b.log2MemoryBankInterleavingSize)

	chiplet.L2ToRdmaConnection = akita.NewDirectConnection(
		chiplet.name+".L2-RDMA", b.engine, b.freq)

	chiplet.L2ToRdmaConnection.PlugIn(chiplet.chipRdmaEngine.GetToL2Bottom(),
		4*chiplet.chipRdmaEngine.GetNumTransPerCycle()) // from rdma to l2 bottom port for rsp

	for _, l2Cache := range chiplet.L2Caches {
		chiplet.L2ToRdmaConnection.PlugIn(l2Cache.GetBottomToRdmaPort(),
			2*chiplet.chipRdmaEngine.GetNumTransPerCycle()) // from l2 to rdma for req
		chiplet.L2ToRdmaConnection.PlugIn(l2Cache.GetRDMAPort(),
			2*chiplet.chipRdmaEngine.GetNumTransPerCycle()) // from l2 to rdma for rsp
		localModuleFinderForRDMA.LowModules =
			append(localModuleFinderForRDMA.LowModules, l2Cache.GetRDMAPort())
	}

	// chip rdma engines' local module should be L2 cache
	// Top and Bottom ports are both needed.
	chiplet.chipRdmaEngine.SetLocalModuleFinder(localModuleFinderForRDMA)
	chiplet.L2ToRdmaConnection.PlugIn(chiplet.chipRdmaEngine.GetToL2RDMA(),
		4*chiplet.chipRdmaEngine.GetNumTransPerCycle()) // from rdma to l2 for req

	// only the ToDramTop port should be connected to dram connection for l2 cache eviction
	chiplet.L2ToDramConnection.PlugIn(chiplet.chipRdmaEngine.GetToDramTop(),
		4*chiplet.chipRdmaEngine.GetNumTransPerCycle()) // from rdma to dram for trans and evict req
}

// only used when pte caching is enabled.
func (b *CommonBuilder) connectMMUToL2(
	chiplet *Chiplet,
) {
	if !b.usePteCaching {
		return
	}

	// now, mmu goes to l2 since PTEs are cached in l2.
	lowModuleFinderForMMU := lmf.NewStripedLowModuleFinder(
		b.memAddrOffset,
		uint64(b.numMemoryBankPerChiplet),
		1<<b.log2MemoryBankInterleavingSize,
	)

	for _, l2 := range chiplet.L2Caches {
		lowModuleFinderForMMU.LowModules = append(
			lowModuleFinderForMMU.LowModules,
			l2.GetTopPort())
	}

	// if pte caching is enabled, mmu lookups its l2 cache.
	chiplet.MMU.SetLowModuleFinder(lowModuleFinderForMMU)

	// unplug mmu from dram for page table walking
	chiplet.L2ToDramConnection.Unplug(chiplet.MMU.LocalTransPort)

	// mmu to l2 for pte caching
	chiplet.L1ToL2Connection.PlugIn(chiplet.MMU.LocalTransPort, chiplet.MMU.GetNumReqPerCycle()*4)
}

func (b *CommonBuilder) connectL1TLBToL2TLB(
	chiplet *Chiplet,
) {
	tlbConn := akita.NewDirectConnection(chiplet.name+"L1TLB-L2TLB", b.engine, b.freq)
	tlbConn.PlugIn(chiplet.L2TLBs[0].GetTopPort(), chiplet.L2TLBs[0].GetNumReqPerCycle()*4)

	var lowModuleFinder lmf.LowModuleFinder

	singeLowModuleFinder := new(lmf.SingleLowModuleFinder)          // L1 TLBs have only one L2 TLB as a low module.
	singeLowModuleFinder.LowModule = chiplet.L2TLBs[0].GetTopPort() // We have only one L2 TLB in the system.

	// Only the real (caching) LatTLB walks to the MMU on a miss, so it needs the
	// finder. IdealLatTLB always hits and has no low module — skip it.
	if lat, ok := chiplet.L2TLBs[0].(*tlb.LatTLB); ok {
		lat.TLBFinder = singeLowModuleFinder
	}

	lowModuleFinder = singeLowModuleFinder

	for _, l1vTLB := range chiplet.L1VTLBs {
		l1vTLB.SetLowModuleFinder(lowModuleFinder)
		tlbConn.PlugIn(l1vTLB.BottomPort, l1vTLB.GetNumReqPerCycle()*4)
	}

	for _, l1iTLB := range chiplet.L1ITLBs {
		l1iTLB.SetLowModuleFinder(lowModuleFinder)
		tlbConn.PlugIn(l1iTLB.BottomPort, l1iTLB.GetNumReqPerCycle()*4)
	}

	for _, l1sTLB := range chiplet.L1STLBs {
		l1sTLB.SetLowModuleFinder(lowModuleFinder)
		tlbConn.PlugIn(l1sTLB.BottomPort, l1sTLB.GetNumReqPerCycle()*4)
	}

	chiplet.L1TLBToL2TLBConnection = tlbConn
}

func (b *CommonBuilder) connectL2TLBToMMU(
	chiplet *Chiplet,
) {
	tlbToMMUConn := akita.NewDirectConnection(
		chiplet.name+".L2TLB-MMU",
		b.engine,
		b.freq)

	tlbToMMUConn.PlugIn(
		chiplet.MMU.ToTop,
		chiplet.MMU.GetNumReqPerCycle()*4)

	for _, l2tlb := range chiplet.L2TLBs { /* there's only one L2TLB */
		tlbToMMUConn.PlugIn(
			l2tlb.GetBottomPort(),
			l2tlb.GetNumReqPerCycle()*4)
	}

	chiplet.TLBToMMUConnection = tlbToMMUConn
}

func (b *CommonBuilder) connectMMUToBottom(
	chiplet *Chiplet,
) {
	lowModuleFinderForMMU := lmf.NewDualStripeStripedLocalVRemoteLowModuleFinder(
		b.memAddrOffset,
		b.numChiplets,
		b.numMemoryBankPerChiplet,
		b.chipletInterleavingSize(),
		1<<b.log2MemoryBankInterleavingSize,
		uint64(b.numMemoryBankPerChiplet)*chiplet.ChipletID,
		uint64(b.numMemoryBankPerChiplet)*chiplet.ChipletID+uint64(b.numMemoryBankPerChiplet-1))

	for _, dram := range chiplet.DRAMs {
		lowModuleFinderForMMU.LowModules = append(lowModuleFinderForMMU.LowModules, dram.ToTop)
	}

	// mmu local translation port is connected to dram for page table walking
	chiplet.L2ToDramConnection.PlugIn(chiplet.MMU.LocalTransPort, chiplet.MMU.GetNumReqPerCycle()*4) // page table walkers * 2

	// remote translation port is connected to rdma but in dram connection
	chiplet.L2ToDramConnection.PlugIn(chiplet.MMU.RemoteTransPort, chiplet.MMU.GetNumReqPerCycle()*4) // page table walkers * 2

	// chip rdma engine's page walk port should be a remote module for lmf of mmu
	lowModuleFinderForMMU.RemoteModule = chiplet.chipRdmaEngine.GetPwPort()

	// also, pwport should be plugged to L2ToDramConnection, not L2ToRdmaConnection
	chiplet.L2ToDramConnection.PlugIn(chiplet.chipRdmaEngine.GetPwPort(),
		4*chiplet.chipRdmaEngine.GetNumTransPerCycle())

	// mmu's low module finder, same as L2 cache's low module finder
	chiplet.MMU.SetLowModuleFinder(lowModuleFinderForMMU)
}

func (b *CommonBuilder) connectCP() {
	b.internalConn = akita.NewDirectConnection(
		b.gpuName+"InternalConn", b.engine, b.freq)
	b.gpu.InternalConnection = b.internalConn

	b.internalConn.PlugIn(b.cp.ToDriver, 1)
	b.internalConn.PlugIn(b.cp.ToDMA, 128)
	b.internalConn.PlugIn(b.cp.ToCaches, 128)
	b.internalConn.PlugIn(b.cp.ToCUs, 128)
	b.internalConn.PlugIn(b.cp.ToTLBs, 128)
	b.internalConn.PlugIn(b.cp.ToAddressTranslators, 128)
	b.internalConn.PlugIn(b.cp.ToRDMA, 4)
	b.internalConn.PlugIn(b.cp.ToPMC, 4)

	b.internalConn.PlugIn(b.cp.ToMMUs, 4)

	b.cp.RDMA = b.rdmaEngine.GetCtrlPort()
	b.internalConn.PlugIn(b.cp.RDMA, 1)

	b.cp.DMAEngine = b.dmaEngine.ToCP
	b.internalConn.PlugIn(b.dmaEngine.ToCP, 1)

	b.cp.PMC = b.pageMigrationController.CtrlPort
	b.internalConn.PlugIn(b.pageMigrationController.CtrlPort, 1)

	b.connectCPWithCUs()
	b.connectCPWithAddressTranslators()
	b.connectCPWithCaches()
	b.connectCPWithChipRdmaEngines()
	b.connectCPWithMMUs()
	b.connectCPWithTLBs()
}

func (b *CommonBuilder) connectCPWithChipRdmaEngines() {
	for _, chiplet := range b.chiplets {
		b.cp.ChipRdmaEngines = append(b.cp.ChipRdmaEngines, chiplet.chipRdmaEngine)
	}
}

func (b *CommonBuilder) connectCPWithCUs() {
	for _, chiplet := range b.chiplets {
		for _, cu := range chiplet.CUs {
			b.cp.RegisterCU(cu)
			b.internalConn.PlugIn(cu.ToACE, 1)
			b.internalConn.PlugIn(cu.ToCP, 1)
		}
	}
}

func (b *CommonBuilder) connectCPWithAddressTranslators() {
	for _, chiplet := range b.chiplets {
		for _, at := range chiplet.L1VAddrTranslator {
			b.cp.AddressTranslators = append(b.cp.AddressTranslators, at.GetCtrlPort())
			b.internalConn.PlugIn(at.GetCtrlPort(), 1)
		}

		for _, at := range chiplet.L1SAddrTranslator {
			b.cp.AddressTranslators = append(b.cp.AddressTranslators, at.GetCtrlPort())
			b.internalConn.PlugIn(at.GetCtrlPort(), 1)
		}

		for _, at := range chiplet.L1IAddrTranslator {
			b.cp.AddressTranslators = append(b.cp.AddressTranslators, at.GetCtrlPort())
			b.internalConn.PlugIn(at.GetCtrlPort(), 1)
		}

		for _, rob := range chiplet.L1VROBs {
			b.cp.AddressTranslators = append(
				b.cp.AddressTranslators, rob.ControlPort)
			b.internalConn.PlugIn(rob.ControlPort, 1)
		}

		for _, rob := range chiplet.L1IROBs {
			b.cp.AddressTranslators = append(
				b.cp.AddressTranslators, rob.ControlPort)
			b.internalConn.PlugIn(rob.ControlPort, 1)
		}

		for _, rob := range chiplet.L1SROBs {
			b.cp.AddressTranslators = append(
				b.cp.AddressTranslators, rob.ControlPort)
			b.internalConn.PlugIn(rob.ControlPort, 1)
		}
	}
}

func (b *CommonBuilder) connectCPWithTLBs() {
	for _, chiplet := range b.chiplets {
		for _, tlb := range chiplet.L2TLBs {
			b.cp.TLBs = append(b.cp.TLBs, tlb.GetControlPort())
			b.internalConn.PlugIn(tlb.GetControlPort(), 10)
			// added to setup reverse comm from L2 TLB to CP
			tlb.SetCommandProcessor(b.cp.ToTLBs)
		}

		// 	for _, tlb := range chiplet.L1VTLBs {
		// 		b.cp.TLBs = append(b.cp.TLBs, tlb.ControlPort)
		// 		b.internalConn.PlugIn(tlb.ControlPort, 1)
		// 	}

		// 	for _, tlb := range chiplet.L1STLBs {
		// 		b.cp.TLBs = append(b.cp.TLBs, tlb.ControlPort)
		// 		b.internalConn.PlugIn(tlb.ControlPort, 1)
		// 	}

		// 	for _, tlb := range chiplet.L1ITLBs {
		// 		b.cp.TLBs = append(b.cp.TLBs, tlb.ControlPort)
		// 		b.internalConn.PlugIn(tlb.ControlPort, 1)
		// 	}
	}
}

func (b *CommonBuilder) connectCPWithCaches() {
	for _, chiplet := range b.chiplets {
		for _, c := range chiplet.L1ICaches {
			b.cp.L1ICaches = append(b.cp.L1ICaches, c.GetControlPort())
			b.internalConn.PlugIn(c.GetControlPort(), 1)
		}

		for _, c := range chiplet.L1VCaches {
			b.cp.L1VCaches = append(b.cp.L1VCaches, c.GetControlPort())
			b.internalConn.PlugIn(c.GetControlPort(), 1)
		}

		for _, c := range chiplet.L1SCaches {
			b.cp.L1SCaches = append(b.cp.L1SCaches, c.GetControlPort())
			b.internalConn.PlugIn(c.GetControlPort(), 1)
		}

		for _, c := range chiplet.L2Caches {
			b.cp.L2Caches = append(b.cp.L2Caches, c.GetControlPort())
			b.internalConn.PlugIn(c.GetControlPort(), 1)
		}
	}
}

func (b *CommonBuilder) connectCPWithMMUs() {
	for _, chiplet := range b.chiplets {
		mmu := chiplet.MMU
		b.cp.MMUs = append(b.cp.MMUs, mmu.ControlPort)
		b.internalConn.PlugIn(mmu.ControlPort, 10)
	}
}

func (b *CommonBuilder) setupDMA() {
	// DMA needs global fan-out across all DRAMs (numChiplets * numBanks).
	// Use the modulo dual-stripe variant so the bank-within-chiplet
	// index matches the L2/RDMA/MMU finders (avoids routing-vs-DRAM
	// converter inconsistency in DirectWriteH2D).
	lowModuleFinder := lmf.NewDualStripeStripedLowModuleFinder(
		b.memAddrOffset,
		b.numChiplets,
		b.numMemoryBankPerChiplet,
		b.chipletInterleavingSize(),
		1<<b.log2MemoryBankInterleavingSize)

	for _, chiplet := range b.chiplets {
		toMem := akita.NewLimitNumMsgPort(b.dmaEngine, 64, chiplet.name+".ToMem")
		b.dmaEngine.ToMem = append(b.dmaEngine.ToMem, toMem)

		for _, dram := range chiplet.DRAMs {
			lowModuleFinder.LowModules = append(
				lowModuleFinder.LowModules,
				dram.ToTop)
			b.dmaEngine.DramToChipletMap[dram.ToTop] = toMem
		}

		chiplet.L2ToDramConnection.PlugIn(toMem, 64)
	}

	b.dmaEngine.SetLocalDataSource(lowModuleFinder)
	b.gpu.LocalDataSource = lowModuleFinder
	// b.mmu.SetLowModuleFinder(lowModuleFinder)

	b.pageMigrationController.MemCtrlFinder = lowModuleFinder
	for _, chiplet := range b.chiplets {
		chiplet.L2ToDramConnection.PlugIn(b.pageMigrationController.LocalMemPort, 16)
	}
}

func (b *CommonBuilder) setConnectionToDriver(
	chiplet *Chiplet,
) {
	b.gpu.L1TLBToL2TLBConnection = append(b.gpu.L1TLBToL2TLBConnection, chiplet.L1TLBToL2TLBConnection)
	b.gpu.TLBToMMUConnection = append(b.gpu.TLBToMMUConnection, chiplet.TLBToMMUConnection)
	b.gpu.L2ToDramConnection = append(b.gpu.L2ToDramConnection, chiplet.L2ToDramConnection)
	b.gpu.L2ToRdmaConnection = append(b.gpu.L2ToRdmaConnection, chiplet.L2ToRdmaConnection)
}
