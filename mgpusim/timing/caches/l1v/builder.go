package l1v

import (
	"gitlab.com/akita/akita"
	"gitlab.com/akita/lmf"
	"gitlab.com/akita/mem"
	"gitlab.com/akita/mem/cache"
	"gitlab.com/akita/mgpusim/timing/caches"
	"gitlab.com/akita/util"
	"gitlab.com/akita/util/tracing"
)

// A Builder can build an l1v cache
type Builder struct {
	engine          akita.Engine
	freq            akita.Freq
	log2BlockSize   uint64
	totalByteSize   uint64
	wayAssocitivity int
	numMSHREntry    int
	numBank         int
	bankLatency     int
	numReqPerCycle  int
	lowModuleFinder lmf.LowModuleFinder
	visTracer       tracing.Tracer
	memTracer       tracing.Tracer
	chipletId       uint64
}

// NewBuilder creates a builder with default parameter setting
func NewBuilder() caches.L1CacheBuilder {
	return Builder{
		freq:            1 * akita.GHz,
		log2BlockSize:   6,
		totalByteSize:   4 * mem.KB,
		wayAssocitivity: 2,
		numMSHREntry:    4,
		numBank:         1,
		numReqPerCycle:  4,
		bankLatency:     20,
	}
}

// WithEngine sets the event driven simulation engine that the cache uses
func (b Builder) WithEngine(engine akita.Engine) caches.L1CacheBuilder {
	b.engine = engine
	return b
}

// WithFreq sets the frequency that the cache works at
func (b Builder) WithFreq(freq akita.Freq) caches.L1CacheBuilder {
	b.freq = freq
	return b
}

// WithWayAssocitivity sets the way associtivity the builder builds.
func (b Builder) WithWayAssocitivity(wayAssocitivity int) caches.L1CacheBuilder {
	b.wayAssocitivity = wayAssocitivity
	return b
}

// WithNumMSHREntry sets the number of mshr entry
func (b Builder) WithNumMSHREntry(num int) caches.L1CacheBuilder {
	b.numMSHREntry = num
	return b
}

// WithLog2BlockSize sets the number of bytes in a cache line as a power of 2
func (b Builder) WithLog2BlockSize(n uint64) caches.L1CacheBuilder {
	b.log2BlockSize = n
	return b
}

// WithTotalByteSize sets the capacity of the cache unit
func (b Builder) WithTotalByteSize(byteSize uint64) caches.L1CacheBuilder {
	b.totalByteSize = byteSize
	return b
}

// WithNumBanks sets the number of banks in each cache
func (b Builder) WithNumBanks(n int) caches.L1CacheBuilder {
	b.numBank = n
	return b
}

// WithBankLatency sets the number of cycles needed to read to write a
// cacheline.
func (b Builder) WithBankLatency(n int) caches.L1CacheBuilder {
	b.bankLatency = n
	return b
}

// WithNumReqsPerCycle sets the number of requests that the cache can process
// per cycle
func (b Builder) WithNumReqsPerCycle(n int) caches.L1CacheBuilder {
	b.numReqPerCycle = n
	return b
}

// WithVisTracer sets the visualization tracer
func (b Builder) WithVisTracer(tracer tracing.Tracer) caches.L1CacheBuilder {
	b.visTracer = tracer
	return b
}

// WithMemTracer sets the memory tracer
func (b Builder) WithMemTracer(tracer tracing.Tracer) caches.L1CacheBuilder {
	b.memTracer = tracer
	return b
}

// WithLowModuleFinder specifies how the cache units to create should find low
// level modules.
func (b Builder) WithLowModuleFinder(
	lowModuleFinder lmf.LowModuleFinder,
) caches.L1CacheBuilder {
	b.lowModuleFinder = lowModuleFinder
	return b
}

func (b Builder) WithChipletId(chipletId uint64) caches.L1CacheBuilder {
	b.chipletId = chipletId
	return b
}

// Build returns a new cache unit
func (b Builder) Build(name string) caches.L1Cache {
	b.assertAllRequiredInformationIsAvailable()

	c := &Cache{
		log2BlockSize:  b.log2BlockSize,
		numReqPerCycle: b.numReqPerCycle,
	}
	c.TickingComponent = akita.NewTickingComponent(
		name, b.engine, b.freq, c)

	c.TopPort = akita.NewLimitNumMsgPort(c, b.numReqPerCycle, name+".TopPort")
	c.BottomPort = akita.NewLimitNumMsgPort(c, b.numReqPerCycle,
		name+".BottomPort")
	c.ControlPort = akita.NewLimitNumMsgPort(c, b.numReqPerCycle,
		name+"ControlPort")

	c.dirBuf = util.NewBuffer(b.numReqPerCycle)
	c.bankBufs = make([]util.Buffer, b.numBank)
	for i := 0; i < b.numBank; i++ {
		c.bankBufs[i] = util.NewBuffer(b.numReqPerCycle)
	}

	c.mshr = cache.NewMSHR(b.numMSHREntry, 8) // L1: unlimited merge (out of scope)
	blockSize := 1 << b.log2BlockSize
	numSets := int(b.totalByteSize / uint64(b.wayAssocitivity*blockSize))
	c.directory = cache.NewDirectory(
		numSets, b.wayAssocitivity, 1<<b.log2BlockSize,
		1, // upstream stripe factor; CU→L1V uses SingleLowModuleFinder
		cache.NewLRUVictimFinder())
	c.storage = mem.NewStorage(b.totalByteSize)
	c.bankLatency = b.bankLatency
	c.wayAssociativity = b.wayAssocitivity
	c.lowModuleFinder = b.lowModuleFinder
	c.chipletId = b.chipletId

	b.buildStages(c)

	if b.visTracer != nil {
		tracing.CollectTrace(c, b.visTracer)
	}

	if b.memTracer != nil {
		tracing.CollectTrace(c, b.memTracer)
	}

	return c
}

func (b *Builder) buildStages(c *Cache) {
	c.coalesceStage = &coalescer{cache: c}
	c.directoryStage = &directory{cache: c}
	for i := 0; i < b.numBank; i++ {
		// Deadline-stamping replaces the shift register: extractFromBuf stamps
		// readyCycle = now + bankLatency and pushes straight into the buffer,
		// which therefore holds up to bankLatency cycles' worth of in-flight
		// accesses plus a 1-cycle cushion.
		postPipelineBuf := util.NewBuffer((b.bankLatency + 1) * b.numReqPerCycle)
		bs := &bankStage{
			cache:           c,
			bankID:          i,
			numReqPerCycle:  b.numReqPerCycle,
			latency:         b.bankLatency,
			postPipelineBuf: postPipelineBuf,
		}
		c.bankStages = append(c.bankStages, bs)
	}
	c.parseBottomStage = &bottomParser{cache: c}
	c.respondStage = &respondStage{cache: c}

	c.controlStage = &controlStage{
		ctrlPort:     c.ControlPort,
		transactions: &c.transactions,
		directory:    c.directory,
		cache:        c,
		bankStages:   c.bankStages,
		coalescer:    c.coalesceStage,
	}
}

func (b *Builder) assertAllRequiredInformationIsAvailable() {
	if b.engine == nil {
		panic("engine is not specified")
	}
}
