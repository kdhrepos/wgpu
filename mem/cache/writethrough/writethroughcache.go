package writethrough

import (
	"strings"

	"gitlab.com/akita/akita"
	"gitlab.com/akita/lmf"
	"gitlab.com/akita/mem"
	"gitlab.com/akita/mem/cache"
	"gitlab.com/akita/util"
	"gitlab.com/akita/util/akitaext"
	"gitlab.com/akita/util/pipelining"
)

type cachePipelineItem struct {
	taskID uint64
	trans  *transaction
}

func (c cachePipelineItem) TaskID() uint64 {
	return c.taskID
}

// A Cache is a customized L1 cache the for R9nano GPUs.
type Cache struct {
	*akita.TickingComponent

	TopPort          akita.Port
	RDMAPort         akita.Port
	BottomToRdmaPort akita.Port
	BottomToDramPort akita.Port
	ControlPort      akita.Port

	bankBufs []util.Buffer

	topSender          akitaext.BufferedSender
	rdmaSender         akitaext.BufferedSender
	bottomToDramSender akitaext.BufferedSender
	bottomToRdmaSender akitaext.BufferedSender
	controlPortSender  akitaext.BufferedSender

	pipelineForLocal   pipelining.Pipeline
	pipelineForRemote  pipelining.Pipeline
	lookupBufForLocal  util.Buffer
	lookupBufForRemote util.Buffer

	topParserStage   *topParser
	directoryStage   *directory
	bankStages       []*bankStage
	parseBottomStage *bottomParser
	respondStage     *respondStage
	controlStage     *controlStage

	numReqPerCycle   int
	log2BlockSize    uint64
	storage          *mem.Storage
	directory        cache.Directory
	mshr             cache.MSHR
	bankLatency      int
	wayAssociativity int
	lowModuleFinder  lmf.LowModuleFinder

	// state                cacheState
	inFlightTransactions []*transaction

	isPaused bool

	// To prevent the deadlock, we need to separate TopPort
	arbiter     bool // arbitration between TopPort and RdmaPort
	numToRemote uint64

	// only used for l2 miss trace
	virtualAddrTable lmf.LowModuleFinder
}

func (c *Cache) GetPipeline() pipelining.Pipeline {
	return c.pipelineForLocal
}

// SetLowModuleFinder sets the finder that tells which remote port can serve
// the data on a certain address.
func (c *Cache) SetLowModuleFinder(lmf lmf.LowModuleFinder) {
	c.lowModuleFinder = lmf
}

// Tick update the state of the cache
func (c *Cache) Tick(now akita.VTimeInSec) bool {
	madeProgress := false

	if !c.isPaused {
		madeProgress = c.runPipeline(now) || madeProgress
	}

	madeProgress = c.controlStage.Tick(now) || madeProgress

	return madeProgress
}

func (c *Cache) runPipeline(now akita.VTimeInSec) bool {
	madeProgress := false

	madeProgress = c.tickRespondStage(now) || madeProgress
	madeProgress = c.tickParseBottomStage(now) || madeProgress
	madeProgress = c.tickBankStage(now) || madeProgress
	madeProgress = c.tickDirectoryStage(now) || madeProgress
	madeProgress = c.tickPipelineStage(now) || madeProgress
	madeProgress = c.tickTopParserStage(now) || madeProgress

	c.arbiter = !c.arbiter

	return madeProgress
}

func (c *Cache) tickRespondStage(now akita.VTimeInSec) bool {
	madeProgress := false
	for i := 0; i < c.numReqPerCycle; i++ {
		madeProgress = c.respondStage.Tick(now) || madeProgress
	}
	return madeProgress
}

func (c *Cache) tickParseBottomStage(now akita.VTimeInSec) bool {
	madeProgress := false

	for i := 0; i < c.numReqPerCycle; i++ {
		madeProgress = c.parseBottomStage.Tick(now) || madeProgress
	}

	return madeProgress
}

func (c *Cache) tickBankStage(now akita.VTimeInSec) bool {
	madeProgress := false
	for _, bs := range c.bankStages {
		for i := 0; i < c.numReqPerCycle; i++ {
			madeProgress = bs.Tick(now) || madeProgress
		}
	}
	return madeProgress
}

func (c *Cache) tickDirectoryStage(now akita.VTimeInSec) bool {
	madeProgress := false
	for i := 0; i < c.numReqPerCycle; i++ {
		madeProgress = c.directoryStage.Tick(now) || madeProgress
	}
	return madeProgress
}

func (c *Cache) tickPipelineStage(now akita.VTimeInSec) bool {
	madeProgress := false
	for i := 0; i < c.numReqPerCycle; i++ {
		madeProgress = c.pipelineForLocal.Tick(now) || madeProgress
		madeProgress = c.pipelineForRemote.Tick(now) || madeProgress
	}
	return madeProgress
}

func (c *Cache) tickTopParserStage(now akita.VTimeInSec) bool {
	madeProgress := false
	for i := 0; i < c.numReqPerCycle; i++ {
		madeProgress = c.topParserStage.Tick(now) || madeProgress
	}
	return madeProgress
}

func (c *Cache) GetTopPort() akita.Port {
	return c.TopPort
}

func (c *Cache) GetBottomToDramPort() akita.Port {
	return c.BottomToDramPort
}

func (c *Cache) GetBottomToRdmaPort() akita.Port {
	return c.BottomToRdmaPort
}

func (c *Cache) GetRDMAPort() akita.Port {
	return c.RDMAPort
}

func (c *Cache) GetControlPort() akita.Port {
	return c.ControlPort
}

func (c *Cache) GetNumReqPerCycle() int {
	return c.numReqPerCycle
}

// check if the request goes to local memory
func (c *Cache) isReqToRemote(
	addr uint64,
) bool {
	return strings.Contains(c.lowModuleFinder.Find(addr).Name(), "RDMA")
}

func (c *Cache) isRemoteReqPossible() bool {
	// Reserve some MSHR slots for local requests so remote (RDMA) requests
	// cannot monopolize the cache's MSHR. Compare against the fixed MSHR
	// capacity, not current occupancy (AllEntries).
	const localReserve = 4
	limit := c.mshr.Capacity() - localReserve
	if limit < 0 {
		limit = 0
	}
	return c.numToRemote < uint64(limit)
}

func (c *Cache) SetVirtualAddrTable(t lmf.LowModuleFinder) {
	c.virtualAddrTable = t
}
