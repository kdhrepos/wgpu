package l1v

import (
	"fmt"

	"gitlab.com/akita/akita"
	"gitlab.com/akita/lmf"
	"gitlab.com/akita/mem"
	"gitlab.com/akita/mem/cache"
	"gitlab.com/akita/util"
)

// A Cache is a customized L1 cache the for R9nano GPUs.
type Cache struct {
	*akita.TickingComponent

	TopPort     akita.Port
	BottomPort  akita.Port
	ControlPort akita.Port

	numReqPerCycle   int
	log2BlockSize    uint64
	storage          *mem.Storage
	directory        cache.Directory
	mshr             cache.MSHR
	bankLatency      int
	wayAssociativity int
	lowModuleFinder  lmf.LowModuleFinder

	dirBuf   util.Buffer
	bankBufs []util.Buffer

	coalesceStage    *coalescer
	directoryStage   *directory
	bankStages       []*bankStage
	parseBottomStage *bottomParser
	respondStage     *respondStage
	controlStage     *controlStage

	transactions             []*transaction
	postCoalesceTransactions []*transaction

	isPaused bool

	chipletId uint64

	// below-L1V wait: per L1 miss, time from sending the fetch to L2
	// (readToBottom.SendTime) until the L2/remote response returns. This is
	// the L2 + network + DRAM portion of the downstream round-trip, measured
	// at the L1V so we can split "downstream" into L1V-local vs below-L1V.
	BelowWaitSum   akita.VTimeInSec
	BelowWaitCount uint64
}

// DeadlockSummary reports every port + every internal buffer that can hold
// state in the L1 cache: top/bottom/ctrl ports, dirBuf, per-bank buffers,
// MSHR active count, live transaction list, post-coalesce queue, plus the
// paused flag (control msg-driven). anyPending is true iff any bucket is
// non-empty or the cache is paused mid-flush.
func (c *Cache) DeadlockSummary() (line string, anyPending bool) {
	topQ := c.TopPort.Peek() != nil
	bottomQ := c.BottomPort.Peek() != nil
	ctrlQ := c.ControlPort.Peek() != nil
	dirN := 0
	if c.dirBuf != nil {
		dirN = c.dirBuf.Size()
	}
	bankN := 0
	for _, b := range c.bankBufs {
		if b != nil {
			bankN += b.Size()
		}
	}
	mshrN := 0
	if c.mshr != nil {
		mshrN = len(c.mshr.AllEntries())
	}
	transN := len(c.transactions)
	postN := len(c.postCoalesceTransactions)
	anyPending = topQ || bottomQ || ctrlQ ||
		dirN > 0 || bankN > 0 || mshrN > 0 || transN > 0 || postN > 0 ||
		c.isPaused
	line = fmt.Sprintf(
		"top=%v bottom=%v ctrl=%v dir=%d bank=%d mshrActive=%d trans=%d postCoal=%d paused=%v",
		topQ, bottomQ, ctrlQ, dirN, bankN, mshrN, transN, postN, c.isPaused)
	return
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
	madeProgress = c.tickCoalesceState(now) || madeProgress
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
		madeProgress = bs.Tick(now) || madeProgress
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

func (c *Cache) tickCoalesceState(now akita.VTimeInSec) bool {
	madeProgress := false
	for i := 0; i < c.numReqPerCycle; i++ {
		madeProgress = c.coalesceStage.Tick(now) || madeProgress
	}
	return madeProgress
}
