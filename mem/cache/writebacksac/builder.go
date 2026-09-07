package writebacksac

import (
	"gitlab.com/akita/akita"
	"gitlab.com/akita/lmf"
	"gitlab.com/akita/mem"
	"gitlab.com/akita/mem/cache"
	"gitlab.com/akita/util"
	"gitlab.com/akita/util/akitaext"
)

// A Builder can build writeback caches
type Builder struct {
	chipletId           int
	bankId              int
	engine              akita.Engine
	freq                akita.Freq
	lowModuleFinder     lmf.LowModuleFinder
	wayAssociativity    int
	log2BlockSize       uint64
	byteSize            uint64
	numBanks            int
	numMSHREntry        int
	numMSHRMaxMerge     int
	numReqPerCycle      int
	writeBufferCapacity int
	maxInflightFetch    int
	maxInflightEviction int
	bankLatency         int
	pipelineLatency     int
}

func MakeBuilder() Builder {
	return Builder{
		freq:                1 * akita.GHz,
		wayAssociativity:    4,
		log2BlockSize:       6,
		byteSize:            512 * mem.KB,
		numBanks:            1,
		numMSHREntry:        64,
		numMSHRMaxMerge:     8,
		numReqPerCycle:      1,
		writeBufferCapacity: 1024,
		maxInflightFetch:    128,
		maxInflightEviction: 128,
		bankLatency:         2,
		pipelineLatency:     10,
	}
}

// WithNumBanks sets the upstream stripe factor (the number of sibling banks
// that share the cacheline-granular interleaving above this cache). Pass 1
// when there is no upstream stripe. Used to skip bank-index bits before
// computing setID, preventing set/bank aliasing.
func (b Builder) WithNumBanks(n int) Builder {
	b.numBanks = n
	return b
}

func (b Builder) WithBankId(id int) *Builder {
	b.bankId = id
	return &b
}

func (b Builder) WithChipletId(id int) Builder {
	b.chipletId = id
	return b
}

func (b Builder) WithEngine(engine akita.Engine) Builder {
	b.engine = engine
	return b
}

func (b Builder) WithFreq(freq akita.Freq) Builder {
	b.freq = freq
	return b
}

func (b Builder) WithWayAssociativity(n int) Builder {
	b.wayAssociativity = n
	return b
}

func (b Builder) WithLog2BlockSize(n uint64) Builder {
	b.log2BlockSize = n
	return b
}

func (b Builder) WithNumMSHREntry(n int) Builder {
	b.numMSHREntry = n
	return b
}

// WithNumMSHRMaxMerge sets the maximum number of requests that may
// coalesce into a single MSHR entry. n <= 0 means unlimited.
func (b Builder) WithNumMSHRMaxMerge(n int) Builder {
	b.numMSHRMaxMerge = n
	return b
}

func (b Builder) WithLowModuleFinder(f lmf.LowModuleFinder) Builder {
	b.lowModuleFinder = f
	return b
}

func (b Builder) WithNumReqPerCycle(n int) Builder {
	b.numReqPerCycle = n
	return b
}

func (b Builder) WithTotalByteSize(byteSize uint64) Builder {
	b.byteSize = byteSize
	return b
}

// WithWriteBufferSize sets the number of cachlines that can reside in the
// writebuffer.
func (b Builder) WithWriteBufferSize(n int) Builder {
	b.writeBufferCapacity = n
	return b
}

// WithMaxInflightFetch sets the number of concurrent fetch that the write-back
// cache can issue at the same time.
func (b Builder) WithMaxInflightFetch(n int) Builder {
	b.maxInflightFetch = n
	return b
}

// WithMaxInflightEviction sets the number of concurrent eviction that the
// write buffer can write to a low-level module.
func (b Builder) WithMaxInflightEviction(n int) Builder {
	b.maxInflightEviction = n
	return b
}

// WithBankLatency sets the bank read/write latency in cycles.
func (b Builder) WithBankLatency(n int) Builder {
	b.bankLatency = n
	return b
}

// WithPipelineLatency sets the tag-lookup pipeline latency in cycles
// stamped onto each transaction at topParser admission.
func (b Builder) WithPipelineLatency(n int) Builder {
	b.pipelineLatency = n
	return b
}

// Build creates a usable writeback cache.
func (b *Builder) Build(name string) *Cache {
	cache := &Cache{}

	cache.TickingComponent = akita.NewTickingComponent(
		name, b.engine, b.freq, cache)

	b.configureCache(cache)
	b.createPorts(cache)
	b.createPortSenders(cache)
	b.createInternalStages(cache)
	b.createInternalBuffers(cache)

	cache.ChipletId = b.chipletId
	cache.BankId = b.bankId

	return cache
}

func (b *Builder) configureCache(cacheModule *Cache) {
	blockSize := 1 << b.log2BlockSize
	vimctimFinder := cache.NewLRUVictimFinder()
	numSet := int(b.byteSize / uint64(b.wayAssociativity*blockSize))
	directory := cache.NewDirectory(
		numSet, b.wayAssociativity, blockSize, b.numBanks, vimctimFinder)
	mshr := cache.NewMSHR(b.numMSHREntry, b.numMSHRMaxMerge)
	storage := mem.NewStorage(b.byteSize)

	cacheModule.log2BlockSize = b.log2BlockSize
	cacheModule.numReqPerCycle = b.numReqPerCycle
	cacheModule.pipelineLatency = b.pipelineLatency
	cacheModule.directory = directory
	cacheModule.mshr = mshr
	cacheModule.storage = storage
	cacheModule.lowModuleFinder = b.lowModuleFinder
	cacheModule.state = cacheStateRunning
}

func (b *Builder) createPorts(cache *Cache) {
	cache.TopPort = akita.NewLimitNumMsgPort(cache,
		cache.numReqPerCycle*16, cache.Name()+".TopPort") // originally, capacity of TopPort was numReqPerCycle*32
	cache.RDMAPort = akita.NewLimitNumMsgPort(cache,
		cache.numReqPerCycle*16, cache.Name()+".RDMAPort")
	cache.BottomToDramPort = akita.NewLimitNumMsgPort(cache,
		cache.numReqPerCycle*16, cache.Name()+".BottomToDramPort")
	cache.BottomToRdmaPort = akita.NewLimitNumMsgPort(cache,
		cache.numReqPerCycle*16, cache.Name()+".BottomToRdmaPort")
	cache.ControlPort = akita.NewLimitNumMsgPort(cache,
		cache.numReqPerCycle*2, cache.Name()+".ControlPort")
}

func (b *Builder) createPortSenders(cache *Cache) {
	cache.topSender = akitaext.NewBufferedSender(
		cache.TopPort, util.NewBuffer(cache.numReqPerCycle*16)) // originally, capacity of TopPort was numReqPerCycle*4
	cache.rdmaSender = akitaext.NewBufferedSender(
		cache.RDMAPort, util.NewBuffer(cache.numReqPerCycle*16))
	cache.bottomToDramSender = akitaext.NewBufferedSender(
		cache.BottomToDramPort, util.NewBuffer(cache.numReqPerCycle*16))
	cache.bottomToRdmaSender = akitaext.NewBufferedSender(
		cache.BottomToRdmaPort, util.NewBuffer(cache.numReqPerCycle*16))
	cache.controlPortSender = akitaext.NewBufferedSender(
		cache.ControlPort, util.NewBuffer(cache.numReqPerCycle*4))
}

func (b *Builder) createInternalStages(cache *Cache) {
	cache.topParser = &topParser{cache: cache}
	cache.dirStage = &directoryStage{cache: cache}
	cache.bankStages = make([]*bankStage, 1)
	cache.bankStages[0] = &bankStage{
		cache:   cache,
		bankID:  0,
		latency: b.bankLatency,
	}
	cache.mshrStage = &mshrStage{cache: cache}
	cache.flusher = &flusher{cache: cache}

	cache.writeBuffer = &writeBufferStage{
		cache:               cache,
		writeBufferCapacity: b.writeBufferCapacity,
		maxInflightFetch:    b.maxInflightFetch,
		maxInflightEviction: b.maxInflightEviction,
		inflightFetch:       NewSeqBuffer(b.maxInflightFetch),
		inflightEviction:    NewSeqBuffer(b.maxInflightEviction),
		pendingEvictions:    NewSeqBuffer(b.writeBufferCapacity), // gated by writeBufferFull()
		inflightPassThrough: NewSeqBuffer(b.maxInflightFetch),
	}

	// Lookup buffers absorb up to pipelineLatency cycles' worth of
	// in-flight (deadline-stamped) transactions plus the original 16-deep
	// post-pipeline cushion. This matches the BDP previously held inside
	// pipelining's shift register, so back-pressure timing is preserved.
	bufCap := (16 + b.pipelineLatency) * b.numReqPerCycle
	cache.lookupBufForLocal = NewLookupBuffer(bufCap)
	cache.lookupBufForRemote = NewLookupBuffer(bufCap)
}

func (b *Builder) createInternalBuffers(cache *Cache) {
	// cache.dirStageBuffer = NewSmartBuffer(2 * cache.numReqPerCycle)
	// cache.dirToBankBuffers = make([]SmartBuffer, 1)
	// cache.dirToBankBuffers[0] = NewSmartBuffer(2 * cache.numReqPerCycle)
	// cache.writeBufferToBankBuffers = make([]SmartBuffer, 1)
	// cache.writeBufferToBankBuffers[0] = NewSmartBuffer(2 * cache.numReqPerCycle)
	// cache.mshrStageBuffer = NewSmartBuffer(2 * cache.numReqPerCycle)
	// cache.writeBufferBuffer = NewSmartBuffer(2 * cache.numReqPerCycle)

	cache.dirToBankBuffers = make([]*SmartBuffer, 1)
	cache.dirToBankBuffers[0] = NewSmartBuffer(16 * cache.numReqPerCycle)
	cache.writeBufferToBankBuffers = make([]*SmartBuffer, 1)
	cache.writeBufferToBankBuffers[0] = NewSmartBuffer(16 * cache.numReqPerCycle)
	cache.mshrStageBuffer = util.NewBuffer(16 * cache.numReqPerCycle)
	cache.writeBufferBuffer = NewSmartBuffer(16 * cache.numReqPerCycle)
}
