package writethrough

import (
	"gitlab.com/akita/akita"
	"gitlab.com/akita/lmf"
	"gitlab.com/akita/mem"
	"gitlab.com/akita/mem/cache"
	"gitlab.com/akita/util"
	"gitlab.com/akita/util/pipelining"
	"gitlab.com/akita/util/tracing"
)

// A Builder can build an l1v cache
type Builder struct {
	bankId           int
	engine           akita.Engine
	freq             akita.Freq
	log2BlockSize    uint64
	totalByteSize    uint64
	wayAssociativity int
	numMSHREntry     int
	numMSHRMaxMerge  int
	numBank          int
	numBanks         int
	bankLatency      int
	pipelineLatency  int
	numReqPerCycle   int
	lowModuleFinder  lmf.LowModuleFinder
	visTracer        tracing.Tracer
	memTracer        tracing.Tracer
}

// MakeBuilder creates a builder with default parameter setting
func MakeBuilder() *Builder {
	return &Builder{
		freq:             1 * akita.GHz,
		log2BlockSize:    6,
		totalByteSize:    4 * mem.KB,
		wayAssociativity: 2,
		numMSHREntry:     4,
		numMSHRMaxMerge:  0,
		numBank:          1,
		numBanks:         1,
		numReqPerCycle:   4,
		bankLatency:      1,
	}
}

// WithEngine sets the event driven simulation engine that the cache uses
func (b *Builder) WithEngine(engine akita.Engine) *Builder {
	b.engine = engine
	return b
}

// WithFreq sets the frequency that the cache works at
func (b *Builder) WithFreq(freq akita.Freq) *Builder {
	b.freq = freq
	return b
}

func (b *Builder) WithBankId(id int) *Builder {
	b.bankId = id
	return b
}

// WithWayAssociativity sets the way Associativity the builder builds.
func (b *Builder) WithWayAssociativity(wayAssociativity int) *Builder {
	b.wayAssociativity = wayAssociativity
	return b
}

// WithNumMSHREntry sets the number of mshr entry
func (b *Builder) WithNumMSHREntry(num int) *Builder {
	b.numMSHREntry = num
	return b
}

// WithNumMSHRMaxMerge sets the maximum number of requests that may
// coalesce into a single MSHR entry. num <= 0 means unlimited.
func (b *Builder) WithNumMSHRMaxMerge(num int) *Builder {
	b.numMSHRMaxMerge = num
	return b
}

// WithLog2BlockSize sets the number of bytes in a cache line as a power of 2
func (b *Builder) WithLog2BlockSize(n uint64) *Builder {
	b.log2BlockSize = n
	return b
}

// WithTotalByteSize sets the capacity of the cache unit
func (b *Builder) WithTotalByteSize(byteSize uint64) *Builder {
	b.totalByteSize = byteSize
	return b
}

// WithNumBanks sets the upstream stripe factor (sibling banks sharing the
// cacheline-granular interleaving above this cache). 1 if no upstream
// stripe.
func (b *Builder) WithNumBanks(n int) *Builder {
	b.numBanks = n
	return b
}

// WithBankLatency sets the number of cycles needed to read to write a
// cacheline.
func (b *Builder) WithBankLatency(n int) *Builder {
	b.bankLatency = n
	return b
}

// WithNumReqsPerCycle sets the number of requests that the cache can process
// per cycle
func (b *Builder) WithNumReqPerCycle(n int) *Builder {
	b.numReqPerCycle = n
	return b
}

// WithVisTracer sets the visualization tracer
func (b *Builder) WithVisTracer(tracer tracing.Tracer) *Builder {
	b.visTracer = tracer
	return b
}

// WithMemTracer sets the memory tracer
func (b *Builder) WithMemTracer(tracer tracing.Tracer) *Builder {
	b.memTracer = tracer
	return b
}

// WithLowModuleFinder specifies how the cache units to create should find low
// level modules.
func (b *Builder) WithLowModuleFinder(
	lowModuleFinder lmf.LowModuleFinder,
) *Builder {
	b.lowModuleFinder = lowModuleFinder
	return b
}

// Build returns a new cache unit
func (b *Builder) Build(name string) *Cache {
	b.assertAllRequiredInformationIsAvailable()

	c := &Cache{
		log2BlockSize:  b.log2BlockSize,
		numReqPerCycle: b.numReqPerCycle,
	}
	c.TickingComponent = akita.NewTickingComponent(
		name, b.engine, b.freq, c)

	c.TopPort = akita.NewLimitNumMsgPort(c,
		b.numReqPerCycle*16, name+".TopPort")
	c.RDMAPort = akita.NewLimitNumMsgPort(c,
		b.numReqPerCycle*16, name+".RDMAPort")
	c.BottomToDramPort = akita.NewLimitNumMsgPort(c,
		c.numReqPerCycle*2, c.Name()+".BottomToDramPort")
	c.BottomToRdmaPort = akita.NewLimitNumMsgPort(c,
		c.numReqPerCycle*2, c.Name()+".BottomToRdmaPort")
	c.ControlPort = akita.NewLimitNumMsgPort(c,
		b.numReqPerCycle*2, name+".ControlPort")

	c.bankBufs = make([]util.Buffer, b.numBank)
	for i := 0; i < b.numBank; i++ {
		c.bankBufs[i] = util.NewBuffer(2 * b.numReqPerCycle)
	}

	c.mshr = cache.NewMSHR(b.numMSHREntry, b.numMSHRMaxMerge)
	blockSize := 1 << b.log2BlockSize
	numSets := int(b.totalByteSize / uint64(b.wayAssociativity*blockSize))
	c.directory = cache.NewDirectory(
		numSets, b.wayAssociativity, 1<<b.log2BlockSize, b.numBanks,
		cache.NewLRUVictimFinder())
	c.storage = mem.NewStorage(b.totalByteSize)
	c.bankLatency = b.bankLatency
	c.wayAssociativity = b.wayAssociativity
	c.lowModuleFinder = b.lowModuleFinder

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
	c.topParserStage = &topParser{cache: c}
	c.directoryStage = &directory{cache: c}

	for i := 0; i < b.numBank; i++ {
		c.lookupBufForLocal = util.NewBuffer(b.numReqPerCycle)
		c.pipelineForLocal = pipelining.MakeBuilder().
			WithPipelineWidth(b.numReqPerCycle).
			WithNumStage(b.pipelineLatency).
			WithCyclePerStage(1).
			WithPostPipelineBuffer(c.lookupBufForLocal).
			Build(c.Name() + "_pipelineForLocal")
		c.lookupBufForRemote = util.NewBuffer(b.numReqPerCycle)
		c.pipelineForRemote = pipelining.MakeBuilder().
			WithPipelineWidth(b.numReqPerCycle).
			WithNumStage(b.pipelineLatency).
			WithCyclePerStage(1).
			WithPostPipelineBuffer(c.lookupBufForRemote).
			Build(c.Name() + "_pipelineForRemote")
		bs := &bankStage{
			cache:   c,
			bankID:  i,
			latency: b.bankLatency}
		c.bankStages = append(c.bankStages, bs)

		// if b.visTracer != nil {
		// 	tracing.CollectTrace(ppiipeline, b.visTracer)
		// }
	}
	c.parseBottomStage = &bottomParser{cache: c}
	c.respondStage = &respondStage{cache: c}

	c.controlStage = &controlStage{
		ctrlPort:     c.ControlPort,
		transactions: &c.inFlightTransactions,
		directory:    c.directory,
		cache:        c,
		bankStages:   c.bankStages,
		topParser:    c.topParserStage,
	}
}

func (b *Builder) assertAllRequiredInformationIsAvailable() {
	if b.engine == nil {
		panic("engine is not specified")
	}
}
