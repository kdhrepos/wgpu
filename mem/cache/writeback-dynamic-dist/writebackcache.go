package writeback_dynamic_dist

import (
	"fmt"
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

	ClusterId int
	ChipletId int
	BankId    int

	// To prevent the deadlock, we need to separate TopPort
	TopPort          akita.Port
	RDMAPort         akita.Port // This port processes requests from remote chiplets
	BottomToRdmaPort akita.Port
	BottomToDramPort akita.Port
	ControlPort      akita.Port

	dirToBankBuffers         []*SmartBuffer
	writeBufferToBankBuffers []*SmartBuffer
	writeBufferBuffer        *SmartBuffer
	mshrStageBuffer          util.Buffer

	topSender          akitaext.BufferedSender
	rdmaSender         akitaext.BufferedSender
	bottomToDramSender akitaext.BufferedSender
	bottomToRdmaSender akitaext.BufferedSender
	controlPortSender  akitaext.BufferedSender

	pipelineLatency int
	lookupBuf       util.Buffer

	topParser   *topParser
	writeBuffer *writeBufferStage
	dirStage    *directoryStage
	bankStages  []*bankStage
	mshrStage   *mshrStage
	flusher     *flusher

	storage         *mem.Storage
	lowModuleFinder lmf.LowModuleFinder
	directory       Directory
	mshr            MSHR
	log2BlockSize   uint64
	numReqPerCycle  int

	state                cacheState
	inFlightTransactions []*transaction

	numToRemote        uint64 // to prevent the deadlock from mshr entry allocation
	numRemoteEvictions uint64 // to prevent the deadlock from writebuffer eviction

	LocalWaitSum    akita.VTimeInSec
	LocalWaitCount  uint64
	RemoteWaitSum   akita.VTimeInSec
	RemoteWaitCount uint64

	// used for l2 miss trace and src node query
	virtualAddrTable    lmf.LowModuleFinder
	HomeNodeFinder      *lmf.HomeNodeFinder
	chipletIDStrings    []string
	selfChipletIDString string
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
}

func (c *Cache) SetLowModuleFinder(lmf lmf.LowModuleFinder) {
	c.lowModuleFinder = lmf
}

// DeadlockSummary reports this dist-dist L2's pending state for the
// quiesce-deadlock dumper. Distinct from the plain writeback variant: it
// reports the lookup buffers and the deadlock-prevention counters
// (numToRemote / numRemoteEvictions) that are unique to this cache.
func (c *Cache) DeadlockSummary() (line string, anyPending bool) {
	topQ := c.TopPort.Peek() != nil
	rdmaQ := c.RDMAPort.Peek() != nil
	dramQ := c.BottomToDramPort.Peek() != nil
	brdmaQ := c.BottomToRdmaPort.Peek() != nil
	ctrlQ := c.ControlPort.Peek() != nil

	lookupN := 0
	if b, ok := c.lookupBuf.(*LookupBuffer); ok {
		lookupN = b.Size()
	}
	wbBuf := c.writeBufferBuffer.Size()
	inflight := len(c.inFlightTransactions)
	mshrFull := c.mshr.IsFull()
	mshrBufBlocked := !c.mshrStageBuffer.CanPush()

	anyPending = topQ || rdmaQ || dramQ || brdmaQ || ctrlQ ||
		lookupN > 0 || wbBuf > 0 || inflight > 0

	line = fmt.Sprintf(
		"top=%v rdma=%v dram=%v brdma=%v ctrl=%v | "+
			"lookup=%d wbBuf=%d inflight=%d "+
			"mshrFull=%v mshrBufBlocked=%v numToRemote=%d numRemoteEvict=%d",
		topQ, rdmaQ, dramQ, brdmaQ, ctrlQ,
		lookupN, wbBuf, inflight,
		mshrFull, mshrBufBlocked, c.numToRemote, c.numRemoteEvictions)
	return
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

	madeProgress = c.runStage(now, c.bottomToDramSender) || madeProgress
	madeProgress = c.runStage(now, c.bottomToRdmaSender) || madeProgress
	madeProgress = c.runStage(now, c.rdmaSender) || madeProgress
	madeProgress = c.runStage(now, c.topSender) || madeProgress
	madeProgress = c.runStage(now, c.mshrStage) || madeProgress
	madeProgress = c.runStage(now, c.writeBuffer) || madeProgress

	for _, bs := range c.bankStages {
		madeProgress = c.runStage(now, bs) || madeProgress
	}

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

func (c *Cache) GetNumReqPerCycle() int {
	return c.numReqPerCycle
}
