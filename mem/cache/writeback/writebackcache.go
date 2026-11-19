package writeback

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

	// lookupBuf holds deadline-stamped transactions in one (toRemote, id)-ordered
	// heap so local-home work is served first regardless of which port admitted
	// it. topParser stamps trans.pipelineReadyCycle and pushes here with reserve
	// awareness (local always keeps room); the directory stage gates each item
	// by that deadline (event-based tag-lookup latency, no shift register).
	lookupBuf util.Buffer

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
	pipelineLatency int

	state                cacheState
	inFlightTransactions []*transaction

	numToRemote uint64

	// only used for memory trace
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

// DeadlockSummary reports every queue, buffer, and counter that can hold a
// pending transaction in the L2 cache: ALL five ports, the directory and
// write-buffer stage buffers, every per-bank buffer, the MSHR stage buffer,
// the local/remote lookup buffers, MSHR active-entry count, and the live
// transaction list. anyPending is true iff any of them is non-empty.
func (c *Cache) DeadlockSummary() (line string, anyPending bool) {
	topQ := c.TopPort.Peek() != nil
	rdmaQ := c.RDMAPort.Peek() != nil
	dramQ := c.BottomToDramPort.Peek() != nil
	brdmaQ := c.BottomToRdmaPort.Peek() != nil
	ctrlQ := c.ControlPort.Peek() != nil

	dirToBankN, dirLocal, dirRemote := 0, 0, 0
	for _, b := range c.dirToBankBuffers {
		if b != nil {
			dirToBankN += b.Size()
			l, r := b.ClassCounts()
			dirLocal += l
			dirRemote += r
		}
	}
	wbToBankN := 0
	for _, b := range c.writeBufferToBankBuffers {
		if b != nil {
			wbToBankN += b.Size()
		}
	}
	wbBufN, wbLocal, wbRemote := 0, 0, 0
	if c.writeBufferBuffer != nil {
		wbBufN = c.writeBufferBuffer.Size()
		wbLocal, wbRemote = c.writeBufferBuffer.ClassCounts()
	}
	mshrStageN := 0
	if c.mshrStageBuffer != nil {
		mshrStageN = c.mshrStageBuffer.Size()
	}
	lookupN, lkLocal, lkRemote := 0, 0, 0
	if c.lookupBuf != nil {
		lookupN = c.lookupBuf.Size()
		if lb, ok := c.lookupBuf.(*LookupBuffer); ok {
			lkLocal, lkRemote = lb.ClassCounts()
		}
	}
	mshrN := len(c.mshr.AllEntries())
	inflN := len(c.inFlightTransactions)

	anyPending = topQ || rdmaQ || dramQ || brdmaQ || ctrlQ ||
		dirToBankN > 0 || wbToBankN > 0 || wbBufN > 0 || mshrStageN > 0 ||
		lookupN > 0 || mshrN > 0 || inflN > 0
	line = fmt.Sprintf(
		"top=%v rdma=%v dram=%v bRdma=%v ctrl=%v "+
			"dirToBank=%d[local=%d/remote=%d] wbToBank=%d "+
			"wbBuf=%d[local=%d/remote=%d] mshrStage=%d "+
			"lookup=%d[local=%d/remote=%d] mshrActive=%d inFlight=%d",
		topQ, rdmaQ, dramQ, brdmaQ, ctrlQ,
		dirToBankN, dirLocal, dirRemote, wbToBankN,
		wbBufN, wbLocal, wbRemote, mshrStageN,
		lookupN, lkLocal, lkRemote, mshrN, inflN)
	if anyPending {
		line += " || " + c.meshDeadlockDiag()
	}
	return
}

// meshDeadlockDiag reports, for the L2's egress ports, how full the
// connection-side (Send-gating) buffer is and whether each BufferedSender can
// still push one message. "+ROOM" on a port means a Send would currently
// succeed; if the cache holds a reply but its sender shows CanSend=false while
// the downstream RDMA shows the matching port +ROOM, the stall is a lost
// wakeup at this boundary rather than true backpressure.
func (c *Cache) meshDeadlockDiag() string {
	out := func(p akita.Port) string {
		lp, ok := p.(*akita.LimitNumMsgPort)
		if !ok {
			return "n/a"
		}
		u, capacity, has := lp.OutBufLen()
		if !has {
			return "noconn"
		}
		room := ""
		if u < capacity {
			room = "+ROOM"
		}
		return fmt.Sprintf("%d/%d%s", u, capacity, room)
	}
	cs := func(s akitaext.BufferedSender) bool { return s != nil && s.CanSend(1) }
	return fmt.Sprintf(
		"egress[RDMAPort=%s BotRdma=%s BotDram=%s Top=%s] "+
			"senders[rdma=%v top=%v botRdma=%v botDram=%v]",
		out(c.RDMAPort), out(c.BottomToRdmaPort),
		out(c.BottomToDramPort), out(c.TopPort),
		cs(c.rdmaSender), cs(c.topSender),
		cs(c.bottomToRdmaSender), cs(c.bottomToDramSender))
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
