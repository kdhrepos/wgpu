package builders

import (
	"fmt"

	"gitlab.com/akita/akita"
	"gitlab.com/akita/lmf"
	"gitlab.com/akita/mem"
	"gitlab.com/akita/mem/device"
	"gitlab.com/akita/mem/vm/addresstranslator"
	"gitlab.com/akita/mem/vm/tlb"
	"gitlab.com/akita/mgpusim/timing/caches"
	"gitlab.com/akita/mgpusim/timing/caches/l1v"
	"gitlab.com/akita/mgpusim/timing/caches/rob"
	"gitlab.com/akita/mgpusim/timing/cu"
	"gitlab.com/akita/util/global"
	"gitlab.com/akita/util/tracing"
)

type shaderArray struct {
	cus []*cu.ComputeUnit

	l1vROBs []*rob.ReorderBuffer
	l1sROB  *rob.ReorderBuffer
	l1iROB  *rob.ReorderBuffer

	l1vATs []addresstranslator.AddressTranslator
	l1sAT  addresstranslator.AddressTranslator
	l1iAT  addresstranslator.AddressTranslator

	l1vCaches []caches.L1Cache
	l1sCache  caches.L1Cache
	l1iCache  caches.L1Cache

	l1vTLBs []*tlb.TLB
	l1sTLB  *tlb.TLB
	l1iTLB  *tlb.TLB
}

type shaderArrayBuilder struct {
	gpuID uint64
	name  string
	numCU int

	engine            akita.Engine
	freq              akita.Freq
	log2CacheLineSize uint64
	log2PageSize      uint64
	visTracer         tracing.Tracer
	memTracer         tracing.Tracer
	tlbTracer         tracing.Tracer

	reqWithPC bool

	clusterType string

	chipletId uint64

	pageTable device.PageTable
}

func makeShaderArrayBuilder() shaderArrayBuilder {
	b := shaderArrayBuilder{
		gpuID:             0,
		name:              "SA",
		numCU:             4,
		freq:              1 * akita.GHz,
		log2CacheLineSize: 6,
		log2PageSize:      12,
	}
	return b
}

func (b *shaderArrayBuilder) withEngine(e akita.Engine) {
	b.engine = e
}

func (b *shaderArrayBuilder) withFreq(f akita.Freq) {
	b.freq = f
}

func (b *shaderArrayBuilder) withGPUID(id uint64) {
	b.gpuID = id
}

func (b *shaderArrayBuilder) withNumCU(n int) {
	b.numCU = n
}

func (b *shaderArrayBuilder) withLog2CachelineSize(
	log2Size uint64,
) {
	b.log2CacheLineSize = log2Size
}

func (b *shaderArrayBuilder) withLog2PageSize(
	log2Size uint64,
) {
	b.log2PageSize = log2Size
}

func (b *shaderArrayBuilder) withVisTracer(
	visTracer tracing.Tracer,
) {
	b.visTracer = visTracer
}

func (b *shaderArrayBuilder) withMemTracer(
	memTracer tracing.Tracer,
) {
	b.memTracer = memTracer
}

func (b *shaderArrayBuilder) withTLBTracer(
	tlbTracer tracing.Tracer,
) {
	b.tlbTracer = tlbTracer
}

func (b *shaderArrayBuilder) withPC(
	p bool,
) {
	b.reqWithPC = p
}

func (b *shaderArrayBuilder) withClusterType(
	clusterType string,
) {
	b.clusterType = clusterType
}

func (b *shaderArrayBuilder) withChipletId(
	chipletId uint64,
) {
	b.chipletId = chipletId
}

func (b *shaderArrayBuilder) withPageTable(
	pageTable device.PageTable,
) {
	b.pageTable = pageTable
}

func (b *shaderArrayBuilder) Build(name string) shaderArray {
	b.name = name
	sa := shaderArray{}

	b.buildComponents(&sa)
	b.connectComponents(&sa)

	return sa
}

func (b *shaderArrayBuilder) buildComponents(sa *shaderArray) {
	b.buildCUs(sa)

	b.buildL1VTLBs(sa)
	b.buildL1VAddressTranslators(sa)
	b.buildL1VReorderBuffers(sa)
	b.buildL1VCaches(sa)

	b.buildL1STLB(sa)
	b.buildL1SAddressTranslator(sa)
	b.buildL1SReorderBuffer(sa)
	b.buildL1SCache(sa)

	b.buildL1ITLB(sa)
	b.buildL1IAddressTranslator(sa)
	b.buildL1IReorderBuffer(sa)
	b.buildL1ICache(sa)
}

func (b *shaderArrayBuilder) connectComponents(sa *shaderArray) {
	b.connectVectorMem(sa)
	b.connectScalarMem(sa)
	b.connectInstMem(sa)
	b.connectVM(sa)
}

func (b *shaderArrayBuilder) connectATToL1TLB(
	at addresstranslator.AddressTranslator,
	tlb *tlb.TLB,
) {
	// IDEAL-AT: translator holds the page table directly and resolves inline.
	// No translation port / TLB connection (the L1 TLB stays idle).
	at.SetTranslationProvider(b.pageTable)
}

func (b *shaderArrayBuilder) connectVM(sa *shaderArray) {
	for i := 0; i < b.numCU; i++ {
		at := sa.l1vATs[i]
		tlb := sa.l1vTLBs[i]
		b.connectATToL1TLB(at, tlb)
	}
	b.connectATToL1TLB(sa.l1sAT, sa.l1sTLB)
	b.connectATToL1TLB(sa.l1iAT, sa.l1iTLB)
}

func (b *shaderArrayBuilder) connectVectorMem(sa *shaderArray) {
	g := global.GetInstance()
	robN := g.GetROBNumReqPerCycle()
	atN := g.GetATNumReqPerCycle()
	l1vN := g.GetL1VCacheNumReqs()

	for i := 0; i < b.numCU; i++ {
		cu := sa.cus[i]
		rob := sa.l1vROBs[i]
		at := sa.l1vATs[i]
		l1v := sa.l1vCaches[i]

		cu.VectorMemModules = &lmf.SingleLowModuleFinder{
			LowModule: rob.TopPort,
		}
		b.connectWithDirectConnection(cu.ToVectorMem, rob.TopPort, robN)

		rob.BottomUnit = at.GetTopPort()
		b.connectWithDirectConnection(rob.BottomPort, at.GetTopPort(), atN)

		at.SetLowModuleFinder(&lmf.SingleLowModuleFinder{
			LowModule: l1v.GetTopPort(),
		})
		b.connectWithDirectConnection(l1v.GetTopPort(), at.GetBottomPort(), l1vN)
	}
}

func (b *shaderArrayBuilder) connectScalarMem(sa *shaderArray) {
	rob := sa.l1sROB
	at := sa.l1sAT
	// tlb := sa.l1sTLB
	l1s := sa.l1sCache

	rob.BottomUnit = at.GetTopPort()
	b.connectWithDirectConnection(rob.BottomPort, at.GetTopPort(), 8)

	at.SetLowModuleFinder(&lmf.SingleLowModuleFinder{
		LowModule: l1s.GetTopPort(),
	})
	b.connectWithDirectConnection(l1s.GetTopPort(), at.GetBottomPort(), 8)

	conn := akita.NewDirectConnection(b.name, b.engine, b.freq)
	conn.PlugIn(rob.TopPort, 8)
	for i := 0; i < b.numCU; i++ {
		cu := sa.cus[i]
		cu.ScalarMem = rob.TopPort
		conn.PlugIn(cu.ToScalarMem, 8)
	}
}

func (b *shaderArrayBuilder) connectInstMem(sa *shaderArray) {
	rob := sa.l1iROB
	at := sa.l1iAT
	l1i := sa.l1iCache

	rob.BottomUnit = at.GetTopPort()
	b.connectWithDirectConnection(rob.BottomPort, at.GetTopPort(), 8)

	at.SetLowModuleFinder(&lmf.SingleLowModuleFinder{
		LowModule: l1i.GetTopPort(),
	})
	b.connectWithDirectConnection(l1i.GetTopPort(), at.GetBottomPort(), 8)

	conn := akita.NewDirectConnection(b.name, b.engine, b.freq)
	conn.PlugIn(rob.TopPort, 8)
	for i := 0; i < b.numCU; i++ {
		cu := sa.cus[i]
		cu.InstMem = rob.TopPort
		conn.PlugIn(cu.ToInstMem, 8)
	}
}

func (b *shaderArrayBuilder) connectWithDirectConnection(
	port1, port2 akita.Port,
	bufferSize int,
) {
	name := fmt.Sprintf("%s-%s", port1.Name(), port2.Name())
	conn := akita.NewDirectConnection(
		name,
		b.engine, b.freq,
	)
	conn.PlugIn(port1, bufferSize)
	conn.PlugIn(port2, bufferSize)
}

func (b *shaderArrayBuilder) buildCUs(sa *shaderArray) {
	cuBuilder := cu.MakeBuilder().
		WithEngine(b.engine).
		WithFreq(b.freq).
		WithPC(b.reqWithPC).
		WithLog2CachelineSize(b.log2CacheLineSize)

	for i := 0; i < b.numCU; i++ {
		cuName := fmt.Sprintf("%s.CU_%02d", b.name, i)
		cu := cuBuilder.Build(cuName)
		sa.cus = append(sa.cus, cu)

		if b.visTracer != nil {
			tracing.CollectTrace(cu, b.visTracer)
		}
	}
}

func (b *shaderArrayBuilder) buildL1VReorderBuffers(sa *shaderArray) {
	builder := rob.MakeBuilder().
		WithEngine(b.engine).
		WithFreq(b.freq).
		WithBufferSize(global.GetInstance().GetROBBufferSize()).
		WithNumReqPerCycle(global.GetInstance().GetROBNumReqPerCycle()).
		WithOutOfOrder(true).
		WithPerWfSubFifo(global.GetInstance().GetROBPerWfSubFifo())

	for i := 0; i < b.numCU; i++ {
		name := fmt.Sprintf("%s.L1VROB_%02d", b.name, i)
		rob := builder.Build(name)
		sa.l1vROBs = append(sa.l1vROBs, rob)

		if b.visTracer != nil {
			tracing.CollectTrace(rob, b.visTracer)
		}
	}
}

func (b *shaderArrayBuilder) makeAddressTranslatorBuilder() (builder addresstranslator.AddressTranslatorBuilder) {

	// IDEAL-AT: VA->PA resolved inline against the page table, no TLB/MMU/PWC
	// round-trip. Mapping (and thus chiplet/bank home routing) is preserved.
	builder = addresstranslator.MakeIdealAddressTranslatorBuilder()

	builder.WithEngine(b.engine)
	builder.WithFreq(b.freq)
	builder.WithDeviceID(b.gpuID)
	builder.WithLog2PageSize(b.log2PageSize)
	builder.WithNumReqPerCycle(global.GetInstance().GetATNumReqPerCycle())
	return
}

func (b *shaderArrayBuilder) buildL1VAddressTranslators(sa *shaderArray) {
	builder := b.makeAddressTranslatorBuilder()

	for i := 0; i < b.numCU; i++ {
		name := fmt.Sprintf("%s.L1VAddrTrans_%02d", b.name, i)
		at := builder.Build(name)
		sa.l1vATs = append(sa.l1vATs, at)

		// if b.visTracer != nil {
		// 	tracing.CollectTrace(at, b.visTracer)
		// }
	}
}

func (b *shaderArrayBuilder) buildL1VTLBs(sa *shaderArray) {
	// IDEAL-TRANSLATION: L1 V TLB made near-ideal. MSHR + req/cycle hardcoded
	// high so this stage is never a queueing bottleneck. Capacity (sets/ways)
	// left alone: PWC hit ~99% so few misses to remove, and shrinking remote
	// PTW would cut the topology-sensitive part of the mesh-vs-ideal gap.
	builder := tlb.MakeBuilder().
		WithEngine(b.engine).
		WithFreq(b.freq).
		WithNumMSHREntry(1024).
		WithNumSets(256). // IDEAL-TRANSLATION: was 1 (capacity↑, VPN%numSets indexing)
		WithNumWays(64).
		WithNumReqPerCycle(64).
		WithPageSize(1 << b.log2PageSize)

	for i := 0; i < b.numCU; i++ {
		name := fmt.Sprintf("%s.L1VTLB_%02d", b.name, i)
		tlb := builder.Build(name)
		sa.l1vTLBs = append(sa.l1vTLBs, tlb)

		if b.visTracer != nil {
			tracing.CollectTrace(tlb, b.visTracer)
		}

		if b.tlbTracer != nil {
			tracing.CollectTrace(tlb, b.tlbTracer)
		}
	}
}

func (b *shaderArrayBuilder) buildL1VCaches(sa *shaderArray) {
	builder := l1v.NewBuilder().
		WithEngine(b.engine).
		WithFreq(b.freq).
		WithBankLatency(20).
		WithNumBanks(1).
		WithLog2BlockSize(b.log2CacheLineSize).
		WithWayAssocitivity(8).
		WithNumMSHREntry(32).
		WithTotalByteSize(16 * mem.KB).
		WithNumReqsPerCycle(global.GetInstance().GetL1VCacheNumReqs()).
		WithChipletId(b.chipletId)

	for i := 0; i < b.numCU; i++ {
		name := fmt.Sprintf("%s.L1VCache_%02d", b.name, i)
		cache := builder.Build(name)
		sa.l1vCaches = append(sa.l1vCaches, cache)

		if b.memTracer != nil {
			tracing.CollectTrace(cache, b.memTracer)
		}
	}
}

func (b *shaderArrayBuilder) buildL1SReorderBuffer(sa *shaderArray) {
	// L1S/L1I: scalar/inst CU handlers pop InFlight[0] in global arrival
	// order across all WFs (computeunit.go:553,598). Per-WF sub-FIFO breaks
	// that invariant → InFlight mismatches → corrupt scalar regs / inst
	// bytes → garbage VA → vmem panic. Keep strict single-FIFO in-order.
	builder := rob.MakeBuilder().
		WithEngine(b.engine).
		WithFreq(b.freq).
		WithBufferSize(128).
		WithNumReqPerCycle(4)

	name := fmt.Sprintf("%s.L1SROB", b.name)
	rob := builder.Build(name)
	sa.l1sROB = rob

	if b.visTracer != nil {
		tracing.CollectTrace(rob, b.visTracer)
	}
}

func (b *shaderArrayBuilder) buildL1SAddressTranslator(sa *shaderArray) {
	builder := b.makeAddressTranslatorBuilder()

	name := fmt.Sprintf("%s.L1SAddrTrans", b.name)
	at := builder.Build(name)
	sa.l1sAT = at
}

func (b *shaderArrayBuilder) buildL1STLB(sa *shaderArray) {
	builder := tlb.MakeBuilder().
		WithEngine(b.engine).
		WithFreq(b.freq).
		WithNumMSHREntry(1024). // IDEAL-TRANSLATION: was 8
		WithNumSets(256).       // IDEAL-TRANSLATION: was 1 (capacity↑, VPN%numSets indexing)
		WithNumWays(32).
		WithNumReqPerCycle(64). // IDEAL-TRANSLATION: was 4
		WithPageSize(1 << b.log2PageSize)

	name := fmt.Sprintf("%s.L1STLB", b.name)
	tlb := builder.Build(name)
	sa.l1sTLB = tlb

	if b.visTracer != nil {
		tracing.CollectTrace(tlb, b.visTracer)
	}

	if b.tlbTracer != nil {
		tracing.CollectTrace(tlb, b.tlbTracer)
	}
}

func (b *shaderArrayBuilder) buildL1SCache(sa *shaderArray) {
	builder := l1v.NewBuilder().
		WithEngine(b.engine).
		WithFreq(b.freq).
		WithBankLatency(12).
		WithNumBanks(1).
		WithLog2BlockSize(b.log2CacheLineSize).
		WithWayAssocitivity(4).
		WithNumMSHREntry(32).
		WithTotalByteSize(16 * mem.KB).
		WithNumReqsPerCycle(4).
		WithChipletId(b.chipletId)

	name := fmt.Sprintf("%s.L1SCache", b.name)
	cache := builder.Build(name)
	sa.l1sCache = cache

	if b.visTracer != nil {
		tracing.CollectTrace(cache, b.visTracer)
	}
	if b.memTracer != nil {
		tracing.CollectTrace(cache, b.memTracer)
	}
}

func (b *shaderArrayBuilder) buildL1IReorderBuffer(sa *shaderArray) {
	// L1I: same arrival-order InFlight[0] assumption as L1S — no sub-FIFO.
	builder := rob.MakeBuilder().
		WithEngine(b.engine).
		WithFreq(b.freq).
		WithBufferSize(128).
		WithNumReqPerCycle(4)

	name := fmt.Sprintf("%s.L1IROB", b.name)
	rob := builder.Build(name)
	sa.l1iROB = rob

	if b.visTracer != nil {
		tracing.CollectTrace(rob, b.visTracer)
	}
}

func (b *shaderArrayBuilder) buildL1IAddressTranslator(sa *shaderArray) {
	builder := b.makeAddressTranslatorBuilder()

	name := fmt.Sprintf("%s.L1IAddrTrans", b.name)
	at := builder.Build(name)
	sa.l1iAT = at
}

func (b *shaderArrayBuilder) buildL1ITLB(sa *shaderArray) {
	builder := tlb.MakeBuilder().
		WithEngine(b.engine).
		WithFreq(b.freq).
		WithNumMSHREntry(1024). // IDEAL-TRANSLATION: was 8
		WithNumSets(256).       // IDEAL-TRANSLATION: was 1 (capacity↑, VPN%numSets indexing)
		WithNumWays(32).
		WithNumReqPerCycle(64). // IDEAL-TRANSLATION: was 4
		WithPageSize(1 << b.log2PageSize)

	name := fmt.Sprintf("%s.L1ITLB", b.name)
	tlb := builder.Build(name)
	sa.l1iTLB = tlb

	if b.visTracer != nil {
		tracing.CollectTrace(tlb, b.visTracer)
	}

	if b.tlbTracer != nil {
		tracing.CollectTrace(tlb, b.tlbTracer)
	}
}

func (b *shaderArrayBuilder) buildL1ICache(sa *shaderArray) {
	builder := l1v.NewBuilder().
		WithEngine(b.engine).
		WithFreq(b.freq).
		WithBankLatency(4).
		WithNumBanks(1).
		WithLog2BlockSize(b.log2CacheLineSize).
		WithWayAssocitivity(4).
		WithNumMSHREntry(32).
		WithTotalByteSize(32 * mem.KB).
		WithNumReqsPerCycle(4).
		WithChipletId(b.chipletId)

	name := fmt.Sprintf("%s.L1ICache", b.name)
	cache := builder.Build(name)
	sa.l1iCache = cache

	if b.visTracer != nil {
		tracing.CollectTrace(cache, b.visTracer)
	}
}
