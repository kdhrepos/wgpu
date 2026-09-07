package writeback

import (
	"fmt"

	"gitlab.com/akita/akita"
	"gitlab.com/akita/mem"
	"gitlab.com/akita/mem/cache"
	"gitlab.com/akita/util"
	"gitlab.com/akita/util/akitaext"
)

type pwcPipelineItem struct {
	taskID uint64
	trans  *transaction
}

func (p pwcPipelineItem) TaskID() uint64 {
	return p.taskID
}

// A PageWalkCache in writeback package  is a PageWalkCache that performs the write-back policy
type PageWalkCache struct {
	*akita.TickingComponent

	TopPort akita.Port

	dirStageBuffer   util.Buffer
	dirToBankBuffers []util.Buffer

	topSender akitaext.BufferedSender

	lookupBuffer util.Buffer

	topParser  *pageWalkCacheTopParser
	dirStage   *pageWalkCacheDirectoryStage
	bankStages []*pageWalkCacheBankStage

	storage        *mem.Storage
	directory      *cache.PageWalkCacheDirectoryImpl
	log2BlockSize  uint64
	numReqPerCycle int

	state                cacheState
	inFlightTransactions []*transaction
}

// DeadlockSummary reports the PageWalkCache's port pending flag + internal
// queue depths + in-flight transaction count.
func (c *PageWalkCache) DeadlockSummary() (line string, anyPending bool) {
	topQ := c.TopPort != nil && c.TopPort.Peek() != nil
	dirN := 0
	if c.dirStageBuffer != nil {
		dirN = c.dirStageBuffer.Size()
	}
	bankN := 0
	for _, b := range c.dirToBankBuffers {
		if b != nil {
			bankN += b.Size()
		}
	}
	lookupN := 0
	if c.lookupBuffer != nil {
		lookupN = c.lookupBuffer.Size()
	}
	inflN := len(c.inFlightTransactions)
	anyPending = topQ || dirN > 0 || bankN > 0 || lookupN > 0 || inflN > 0
	line = fmt.Sprintf(
		"top=%v dir=%d bank=%d lookup=%d inFlight=%d",
		topQ, dirN, bankN, lookupN, inflN)
	return
}

func (c *PageWalkCache) Tick(now akita.VTimeInSec) bool {
	madeProgress := false
	if c.state != cacheStatePaused {
		madeProgress = c.runPipeline(now) || madeProgress
	}
	return madeProgress
}

func (c *PageWalkCache) runPipeline(now akita.VTimeInSec) bool {
	madeProgress := false

	madeProgress = c.runStage(now, c.topSender) || madeProgress
	for _, bs := range c.bankStages {
		madeProgress = c.runStage(now, bs) || madeProgress
	}
	madeProgress = c.runStage(now, c.dirStage) || madeProgress
	madeProgress = c.runStage(now, c.topParser) || madeProgress
	return madeProgress
}

func (c *PageWalkCache) runStage(now akita.VTimeInSec, stage akita.Ticker) bool {
	madeProgress := false
	for i := 0; i < c.numReqPerCycle; i++ {
		madeProgress = stage.Tick(now) || madeProgress
	}
	return madeProgress
}
