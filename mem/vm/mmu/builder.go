package mmu

import (
	"gitlab.com/akita/akita"
	"gitlab.com/akita/mem/cache/writeback"
	"gitlab.com/akita/mem/device"
	"gitlab.com/akita/util"
	"gitlab.com/akita/util/akitaext"
)

// A Builder can build MMU component
type Builder struct {
	engine                   akita.Engine
	freq                     akita.Freq
	log2PageSize             uint64
	pageTable                *device.PageTableImpl
	migrationServiceProvider akita.Port
	maxNumReqInFlight        int
	pageWalkingLatency       int
	numChiplets              uint64
}

// MakeBuilder creates a new builder
func MakeBuilder() Builder {
	return Builder{
		freq:              1 * akita.GHz,
		log2PageSize:      12,
		maxNumReqInFlight: 8, //16,
	}
}

// WithEngine sets the engine to be used with the MMU
func (b Builder) WithEngine(engine akita.Engine) Builder {
	b.engine = engine
	return b
}

// WithFreq sets the frequency that the MMU to work at
func (b Builder) WithFreq(freq akita.Freq) Builder {
	b.freq = freq
	return b
}

// WithLog2PageSize sets the page size that the mmu support.
func (b Builder) WithLog2PageSize(log2PageSize uint64) Builder {
	b.log2PageSize = log2PageSize
	return b
}

// WithPageTable sets the page table that the MMU uses.
func (b Builder) WithPageTable(pageTable *device.PageTableImpl) Builder {
	b.pageTable = pageTable
	return b
}

// WithMaxNumReqInFlight sets the number of requests can be concurrently
// processed by the MMU.
func (b Builder) WithMaxNumReqInFlight(n int) Builder {
	b.maxNumReqInFlight = n
	return b
}

// WithNumChiplets sets the number of cycles required
// for walking a page table.
func (b Builder) WithNumChiplets(n uint64) Builder {
	b.numChiplets = n
	return b
}

// Build returns a newly created MMU component
func (b Builder) Build(name string) *MMUImpl {
	mmu := new(MMUImpl)
	mmu.TickingComponent = *akita.NewTickingComponent(
		name, b.engine, b.freq, mmu)
	//mmu.migrationQueueSize = 4096

	mmu.ToTop = akita.NewLimitNumMsgPort(mmu, 4096, name+".ToTop")
	mmu.ControlPort = akita.NewLimitNumMsgPort(mmu, 1, name+".ControlPort")

	//mmu.MigrationPort = akita.NewLimitNumMsgPort(mmu, 1, name+".MigrationPort")
	//might want to change capacity later
	mmu.LocalTransPort = akita.NewLimitNumMsgPort(mmu, 64, name+".LocalTransPort")
	mmu.RemoteTransPort = akita.NewLimitNumMsgPort(mmu, 64, name+".RemoteTransPort")
	//mmu.MigrationServiceProvider = b.migrationServiceProvider

	mmu.topSender = akitaext.NewBufferedSender(mmu.ToTop, util.NewBuffer(4096))
	if b.pageTable != nil {
		mmu.pageTable = b.pageTable
	} else {
		panic("no page table!")
	}
	mmu.maxRequestsInFlight = b.maxNumReqInFlight
	//mmu.latency = b.pageWalkingLatency
	//mmu.PageAccesedByDeviceID = make(map[uint64][]uint64)
	// IDEAL-TRANSLATION: the PWC directory is LEVEL-TAGGED and getSet() indexes
	// by low VA bits, so a shared coarse-level entry (root / mid PDE) maps to a
	// tag that spans a huge region but a set chosen from low bits. The lookup
	// therefore ONLY works fully-associatively, i.e. numSet==1. numSet =
	// byteSize/(wayAssoc*blockSize); grow CAPACITY via wayAssociativity, NEVER
	// byteSize. A 16MB byteSize gave numSet=2048, scattering the root across
	// sets -> ~0.1% root hit -> every walk fell through to ~3.4 DRAM PTE reads.
	// 8192 ways * 8B block = 64KB holds the page-table interior nodes for an
	// ~O(16GB) footprint with numSet==1.
	const pwcWays = 8192
	pageWalkCacheBuilder := writeback.MakePageWalkCacheBuilder().
		WithEngine(b.engine).
		WithLog2PageSize(b.log2PageSize).
		WithBitsPerLevel(9).
		WithWayAssociativity(pwcWays). // numSet stays 1 (fully associative)
		WithByteSize(pwcWays * 8).     // 8B block => numSet = byteSize/(ways*8) = 1
		WithNumReqPerCycle(256).       // IDEAL-TRANSLATION: high walk throughput (was 4)
		WithPipelineLatency(0).        // tag-lookup latency forced to 0
		WithNumBanks(1)                // MMU is per-chiplet single-instance; no upstream stripe
	pageWalkCache := pageWalkCacheBuilder.Build("PageWalkCache")
	mmu.PageWalkCache = pageWalkCache.TopPort
	mmu.pageWalkCachePort = akita.NewLimitNumMsgPort(mmu, 4096, name+".ToTop")
	mmuToPageWalkCache := akita.NewDirectConnection("MMUToPageWalkCache", b.engine, b.freq)
	mmuToPageWalkCache.PlugIn(pageWalkCache.TopPort, 32)
	mmuToPageWalkCache.PlugIn(mmu.pageWalkCachePort, 32)
	mmu.sendStateInfo = false
	return mmu
}
