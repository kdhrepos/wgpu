package writeback

import (
	"fmt"

	"gitlab.com/akita/akita"
	"gitlab.com/akita/lmf"
	"gitlab.com/akita/mem"
	"gitlab.com/akita/mem/cache"
	"gitlab.com/akita/util"
	"gitlab.com/akita/util/akitaext"
)

// A PageWalkCacheBuilder can build writeback caches
type PageWalkCacheBuilder struct {
	engine           akita.Engine
	freq             akita.Freq
	lowModuleFinder  lmf.LowModuleFinder
	wayAssociativity int
	log2BlockSize    uint64
	byteSize         uint64
	numBanks         int
	numReqPerCycle   int
	pipelineLatency  int
	log2PageSize     uint64
	bitsPerLevel     uint64
}

func MakePageWalkCacheBuilder() PageWalkCacheBuilder {
	return PageWalkCacheBuilder{
		freq:             1 * akita.GHz,
		wayAssociativity: 1024, // 32
		log2BlockSize:    3,
		byteSize:         8192, // 512
		numBanks:         1,
		numReqPerCycle:   4,
		pipelineLatency:  10,
	}
}

// WithNumBanks sets the upstream stripe factor (1 if no upstream stripe).
func (b PageWalkCacheBuilder) WithNumBanks(n int) PageWalkCacheBuilder {
	b.numBanks = n
	return b
}

func (b PageWalkCacheBuilder) WithEngine(engine akita.Engine) PageWalkCacheBuilder {
	b.engine = engine
	return b
}

func (b PageWalkCacheBuilder) WithFreq(freq akita.Freq) PageWalkCacheBuilder {
	b.freq = freq
	return b
}

func (b PageWalkCacheBuilder) WithWayAssociativity(n int) PageWalkCacheBuilder {
	b.wayAssociativity = n
	return b
}

func (b PageWalkCacheBuilder) WithLog2BlockSize(n uint64) PageWalkCacheBuilder {
	b.log2BlockSize = n
	return b
}

func (b PageWalkCacheBuilder) WithLog2PageSize(n uint64) PageWalkCacheBuilder {
	b.log2PageSize = n
	return b
}

func (b PageWalkCacheBuilder) WithBitsPerLevel(n uint64) PageWalkCacheBuilder {
	b.bitsPerLevel = n
	return b
}

func (b PageWalkCacheBuilder) WithNumReqPerCycle(n int) PageWalkCacheBuilder {
	b.numReqPerCycle = n
	return b
}

func (b PageWalkCacheBuilder) WithByteSize(byteSize uint64) PageWalkCacheBuilder {
	b.byteSize = byteSize
	return b
}

// WithPipelineLatency sets the tag-lookup pipeline depth in cycles.
func (b PageWalkCacheBuilder) WithPipelineLatency(n int) PageWalkCacheBuilder {
	b.pipelineLatency = n
	return b
}

// bufStages decouples buffer depth from pipeline latency. Buffers (esp. the
// topSender, which Sends without a CanSend guard) need the original depth or
// they overflow; latency=0 only zeros the pipeline delay, not the buffering.
func (b *PageWalkCacheBuilder) bufStages() int {
	if b.pipelineLatency < 10 {
		return 10
	}
	return b.pipelineLatency
}

// Build creates a usable writeback cache.
func (b *PageWalkCacheBuilder) Build(name string) *PageWalkCache {
	cache := new(PageWalkCache)
	cache.TickingComponent = akita.NewTickingComponent(
		name, b.engine, b.freq, cache)

	b.configureCache(cache)
	b.createPorts(cache)
	b.createPortSenders(cache)
	b.createInternalStages(cache)
	b.createInternalBuffers(cache)

	return cache
}

func (b *PageWalkCacheBuilder) configureCache(cacheModule *PageWalkCache) {
	blockSize := 1 << b.log2BlockSize
	vimctimFinder := cache.NewLRUVictimFinder()
	numSet := int(b.byteSize / uint64(b.wayAssociativity*blockSize))
	// The directory is LEVEL-TAGGED: coarse-level entries (root / mid PDE) share
	// one tag over a huge VA region but getSet() picks a set from low VA bits, so
	// a shared entry can only ever live in one set. The lookup is therefore only
	// correct fully-associatively. numSet>1 silently collapses the hit rate
	// (root ~0.1%) instead of crashing — fail fast at build time instead.
	if numSet != 1 {
		panic(fmt.Sprintf(
			"PageWalkCache requires numSet==1 (level-tagged dir); got %d "+
				"(byteSize=%d, ways=%d, block=%d). Grow wayAssociativity, "+
				"not byteSize.", numSet, b.byteSize, b.wayAssociativity, blockSize))
	}
	directory := cache.NewPageWalkCacheDirectory(
		numSet,
		b.wayAssociativity,
		blockSize,
		b.numBanks,
		vimctimFinder,
		b.log2PageSize,
		b.bitsPerLevel)
	storage := mem.NewStorage(b.byteSize)

	cacheModule.log2BlockSize = b.log2BlockSize
	cacheModule.numReqPerCycle = b.numReqPerCycle
	cacheModule.directory = directory
	cacheModule.storage = storage
	cacheModule.state = cacheStateRunning
}

func (b *PageWalkCacheBuilder) createPorts(cache *PageWalkCache) {
	cache.TopPort = akita.NewLimitNumMsgPort(cache,
		cache.numReqPerCycle*4, cache.Name()+".ToTop")
}

func (b *PageWalkCacheBuilder) createPortSenders(cache *PageWalkCache) {
	cache.topSender = akitaext.NewBufferedSender(
		cache.TopPort,
		util.NewBuffer(cache.numReqPerCycle*b.bufStages()))
}

func (b *PageWalkCacheBuilder) createInternalStages(cache *PageWalkCache) {
	cache.topParser = &pageWalkCacheTopParser{cache: cache}
	cache.dirStage = &pageWalkCacheDirectoryStage{cache: cache}
	cache.bankStages = make([]*pageWalkCacheBankStage, 1)
	cache.bankStages[0] = &pageWalkCacheBankStage{
		cache:   cache,
		bankID:  0,
		latency: 1,
	}
	cache.lookupBuffer = util.NewBuffer(b.bufStages() * b.numReqPerCycle)
}

func (b *PageWalkCacheBuilder) createInternalBuffers(cache *PageWalkCache) {
	cache.dirStageBuffer = util.NewBuffer(b.bufStages() * b.numReqPerCycle)
	cache.dirToBankBuffers = make([]util.Buffer, 1)
	cache.dirToBankBuffers[0] = util.NewBuffer(b.bufStages() * b.numReqPerCycle)
}
