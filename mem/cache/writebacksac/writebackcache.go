package writebacksac

import (
	"strconv"
	"strings"

	"gitlab.com/akita/akita"
	"gitlab.com/akita/lmf"
	"gitlab.com/akita/mem"
	"gitlab.com/akita/mem/cache"
	"gitlab.com/akita/util"
	"gitlab.com/akita/util/akitaext"
)

type cacheState int

// don't forget to cast accessReq into the correct type at the end of pipeline
type cachePipelineItem struct {
	taskID uint64
	trans  *transaction
}

func (c cachePipelineItem) TaskID() uint64 {
	return c.taskID
}

const (
	cacheStateInvalid cacheState = iota
	cacheStateRunning
	cacheStatePreFlushing
	cacheStateFlushing
	cacheStatePaused
)

// A Cache in writeback package  is a cache that performs the write-back policy
type Cache struct {
	*akita.TickingComponent

	ChipletId int
	BankId    int

	// To prevent the deadlock, we need to separate TopPort
	TopPort          akita.Port
	RDMAPort         akita.Port // This port processes requests from remote chiplets
	BottomToRdmaPort akita.Port
	BottomToDramPort akita.Port
	ControlPort      akita.Port
	// BottomPort  akita.Port

	dirToBankBuffers         []*SmartBuffer
	writeBufferToBankBuffers []*SmartBuffer
	writeBufferBuffer        *SmartBuffer
	mshrStageBuffer          util.Buffer

	topSender          akitaext.BufferedSender
	rdmaSender         akitaext.BufferedSender
	bottomToDramSender akitaext.BufferedSender
	bottomToRdmaSender akitaext.BufferedSender
	controlPortSender  akitaext.BufferedSender
	// bottomSender      akitaext.BufferedSender

	// pipelineLatency is the simulated tag-lookup latency stamped onto
	// every transaction at topParser admission. The pipelining package's
	// shift register has been replaced with a single TickLater wake-up
	// at the stamped readyCycle (see topparser.go / directorystage.go).
	pipelineLatency    int
	lookupBufForLocal  util.Buffer
	lookupBufForRemote util.Buffer

	topParser   *topParser
	dirStage    *directoryStage
	bankStages  []*bankStage
	writeBuffer *writeBufferStage
	mshrStage   *mshrStage
	flusher     *flusher

	storage         *mem.Storage
	lowModuleFinder lmf.LowModuleFinder
	directory       cache.Directory
	mshr            cache.MSHR
	log2BlockSize   uint64
	numReqPerCycle  int

	state                cacheState
	inFlightTransactions []*transaction

	numToRemote uint64

	// only used for memory trace
	virtualAddrTable    lmf.LowModuleFinder
	HomeNodeFinder      *lmf.HomeNodeFinder
	chipletIDStrings    []string
	selfChipletIDString string

	// SAC: per-bank Chip Request Directory used to predict the SM-side
	// hit rate during the profiling window. Created by SetHomeNodeFinder.
	crd *chipRequestDirectory
}

// ResetCRD clears the CRD sampling state (called at each kernel start by
// the SAC controller).
func (c *Cache) ResetCRD() {
	if c.crd != nil {
		c.crd.Reset()
	}
}

// removeInFlightTransaction removes trans from the in-flight list and
// reports whether it was found. A missing transaction is legal after a
// DiscardInflight flush wiped the list while a bottom response was still
// on the wire.
func (c *Cache) removeInFlightTransaction(trans *transaction) bool {
	for i, t := range c.inFlightTransactions {
		if trans == t {
			c.inFlightTransactions = append(
				c.inFlightTransactions[:i],
				c.inFlightTransactions[i+1:]...)
			return true
		}
	}
	return false
}

// SetHomeNodeFinder wires the HomeNodeFinder and precomputes the per-chiplet
// numeric strings used as task.What/task.Detail payloads for memory-trace and
// l2-miss-trace events. Must be called after ChipletId is set.
func (c *Cache) SetHomeNodeFinder(f *lmf.HomeNodeFinder) {
	c.HomeNodeFinder = f
	c.chipletIDStrings = make([]string, f.NumChiplets)
	for i := uint64(0); i < f.NumChiplets; i++ {
		c.chipletIDStrings[i] = strconv.Itoa(int(i))
	}
	c.selfChipletIDString = strconv.Itoa(c.ChipletId)

	// 8 sampled sets x 16 ways per bank (SAC ISCA'23 §3.6).
	c.crd = newChipRequestDirectory(
		8, 16, uint64(len(c.directory.GetSets())))
}

func (c *Cache) SetLowModuleFinder(lmf lmf.LowModuleFinder) {
	c.lowModuleFinder = lmf
}

func (c *Cache) Tick(now akita.VTimeInSec) bool {
	madeProgress := false

	madeProgress = c.controlPortSender.Tick(now) || madeProgress

	if c.state != cacheStatePaused {
		madeProgress = c.runPipeline(now) || madeProgress
	}

	madeProgress = c.flusher.Tick(now) || madeProgress

	return madeProgress
}

func (c *Cache) runPipeline(now akita.VTimeInSec) bool {
	madeProgress := false

	madeProgress = c.runStage(now, c.topSender) || madeProgress
	madeProgress = c.runStage(now, c.rdmaSender) || madeProgress
	madeProgress = c.runStage(now, c.bottomToDramSender) || madeProgress
	madeProgress = c.runStage(now, c.bottomToRdmaSender) || madeProgress
	madeProgress = c.runStage(now, c.mshrStage) || madeProgress

	for _, bs := range c.bankStages {
		madeProgress = c.runStage(now, bs) || madeProgress
	}

	madeProgress = c.runStage(now, c.writeBuffer) || madeProgress
	madeProgress = c.runStage(now, c.dirStage) || madeProgress
	madeProgress = c.runStage(now, c.topParser) || madeProgress

	return madeProgress
}

func (c *Cache) runStage(now akita.VTimeInSec, stage akita.Ticker) bool {
	madeProgress := false
	for i := 0; i < c.numReqPerCycle; i++ {
		madeProgress = stage.Tick(now) || madeProgress
	}
	return madeProgress
}

func (c *Cache) discardInflightTransactions(now akita.VTimeInSec) {
	sets := c.directory.GetSets()
	for _, set := range sets {
		for _, block := range set.Blocks {
			block.ReadCount = 0
			block.IsLocked = false
		}
	}

	c.dirStage.Reset(now)
	for _, bs := range c.bankStages {
		bs.Reset(now)
	}
	c.mshrStage.Reset(now)
	c.writeBuffer.Reset(now)

	cache.ClearPort(c.TopPort, now)

	c.topSender.Clear()

	c.inFlightTransactions = nil
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

// GetDirectory returns the underlying Directory. Exposed so external
// reporters (e.g. setID access histogram) can read directory state.
func (c *Cache) GetDirectory() cache.Directory {
	return c.directory
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
	const localReserve = 10
	limit := c.mshr.Capacity() - localReserve
	if limit < 0 {
		limit = 0
	}
	return c.numToRemote < uint64(limit)
}

func (c *Cache) SetVirtualAddrTable(t lmf.LowModuleFinder) {
	c.virtualAddrTable = t
}

func (c *Cache) HasPendingWork() bool {
	if c.TopPort.Peek() != nil {
		return true
	}
	if c.RDMAPort.Peek() != nil {
		return true
	}
	if c.BottomToDramPort.Peek() != nil {
		return true
	}
	if c.BottomToRdmaPort.Peek() != nil {
		return true
	}
	if c.ControlPort.Peek() != nil {
		return true
	}
	for _, b := range c.dirToBankBuffers {
		if b.Peek() != nil {
			return true
		}
	}
	for _, b := range c.writeBufferToBankBuffers {
		if b.Peek() != nil {
			return true
		}
	}
	if c.writeBufferBuffer.Peek() != nil {
		return true
	}
	if c.mshrStageBuffer.Peek() != nil {
		return true
	}
	if len(c.inFlightTransactions) != 0 {
		return true
	}

	return false
}
