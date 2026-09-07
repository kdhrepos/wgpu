package writebackrcr

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

// A Cache in writebackrcr package is a write-back cache with NearFetch(HPCA'25) scheme
type Cache struct {
	*akita.TickingComponent

	TopPort          akita.Port
	RDMAPort         akita.Port // This port processes requests from remote chiplets
	LookupPort       akita.Port
	BottomToRdmaPort akita.Port
	BottomToDramPort akita.Port
	ControlPort      akita.Port

	dirToBankBuffers         []*SmartBuffer
	writeBufferToBankBuffers []*SmartBuffer
	writeBufferBuffer        *SmartBuffer
	mshrStageBuffer          util.Buffer

	topSender          akitaext.BufferedSender
	rdmaSender         akitaext.BufferedSender
	lookupSender       akitaext.BufferedSender
	bottomToDramSender akitaext.BufferedSender
	bottomToRdmaSender akitaext.BufferedSender
	controlPortSender  akitaext.BufferedSender

	// pipelineLatency: simulated tag-lookup latency stamped at admission.
	// Replaces the prior pipelining shift register for all three paths
	// (local / remote / lookup). See topparser.go for the stamp logic.
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
	directory       cache.Directory
	mshr            cache.MSHR
	log2BlockSize   uint64
	numReqPerCycle  int

	state                cacheState
	inFlightTransactions []*transaction

	// numToRemote counts in-flight GENERIC remote misses (local CU misses to a
	// remote home). numCornerServe counts in-flight CORNER-SERVE fetches (a
	// corner-node lookup fetching from its straight-line home). They are
	// separate so the corner-serve reserve is a real per-class quota — a generic
	// flood can fill numToRemote up to its band without touching the
	// corner-serve band, and vice-versa. (A single combined counter made the two
	// bands indistinguishable, exactly like a size-only buffer reserve.)
	numToRemote    uint64
	numCornerServe uint64

	// for checking available corner node in NearFetch
	// find home node by address, outputs request port from chiplet, which is a home node
	RdmaReqAddressTable lmf.LowModuleFinder
	HomeNodeFinder      *lmf.HomeNodeFinder
	chipletIDStrings    []string

	BankId    int
	ChipletId int

	LocalWaitSum    akita.VTimeInSec
	LocalWaitCount  uint64
	RemoteWaitSum   akita.VTimeInSec
	RemoteWaitCount uint64

	// dimension of wafer-scale gpu
	row int
	col int
}

// SetHomeNodeFinder wires the HomeNodeFinder used by hasCornerNode to resolve
// the home chiplet of a remote address without parsing port names.
func (c *Cache) SetHomeNodeFinder(f *lmf.HomeNodeFinder) {
	c.HomeNodeFinder = f
	c.chipletIDStrings = make([]string, f.NumChiplets)
	for i := uint64(0); i < f.NumChiplets; i++ {
		c.chipletIDStrings[i] = strconv.Itoa(int(i))
	}
}

func (c *Cache) SetLowModuleFinder(lmf lmf.LowModuleFinder) {
	c.lowModuleFinder = lmf
}

// DeadlockSummary reports per-port queue flags and every internal stage buffer
// depth so the driver's checkQuiesceDeadlock can localize where transactions
// are stuck. Mirrors writeback's, with the extra rcr LookupPort / lookup buffer.
func (c *Cache) DeadlockSummary() (line string, anyPending bool) {
	topQ := c.TopPort.Peek() != nil
	rdmaQ := c.RDMAPort.Peek() != nil
	lookupQ := c.LookupPort.Peek() != nil
	dramQ := c.BottomToDramPort.Peek() != nil
	brdmaQ := c.BottomToRdmaPort.Peek() != nil
	ctrlQ := c.ControlPort.Peek() != nil

	dirToBankN := 0
	for _, b := range c.dirToBankBuffers {
		if b != nil {
			dirToBankN += b.Size()
		}
	}
	wbToBankN := 0
	for _, b := range c.writeBufferToBankBuffers {
		if b != nil {
			wbToBankN += b.Size()
		}
	}
	wbBufN := 0
	if c.writeBufferBuffer != nil {
		wbBufN = c.writeBufferBuffer.Size()
	}
	mshrStageN := 0
	if c.mshrStageBuffer != nil {
		mshrStageN = c.mshrStageBuffer.Size()
	}
	lookupN := 0
	lkHome, lkReal, lkProbe, lkCoal := 0, 0, 0, 0
	if c.lookupBuf != nil {
		lookupN = c.lookupBuf.Size()
		if lb, ok := c.lookupBuf.(*LookupBuffer); ok {
			lkHome, lkReal, lkProbe, lkCoal = lb.ClassCounts()
		}
	}
	infFetchN, infEvictN, pendEvictN, coalInfN := 0, 0, 0, 0
	wbHeadCoal := false
	if c.writeBuffer != nil {
		infFetchN = c.writeBuffer.inflightFetch.Size()
		infEvictN = c.writeBuffer.inflightEviction.Size()
		pendEvictN = c.writeBuffer.pendingEvictions.Size()
		coalInfN = c.writeBuffer.coalesceInflightCount()
		if h := c.writeBufferBuffer.Peek(); h != nil {
			wbHeadCoal = isCoalesceDelegation(h.(*transaction).action)
		}
	}
	topSN := akitaext.PendingCount(c.topSender)
	rdmaSN := akitaext.PendingCount(c.rdmaSender)
	lookupSN := akitaext.PendingCount(c.lookupSender)
	bDramSN := akitaext.PendingCount(c.bottomToDramSender)
	bRdmaSN := akitaext.PendingCount(c.bottomToRdmaSender)
	mshrN := len(c.mshr.AllEntries())
	inflN := len(c.inFlightTransactions)

	anyPending = topQ || rdmaQ || lookupQ || dramQ || brdmaQ || ctrlQ ||
		dirToBankN > 0 || wbToBankN > 0 || wbBufN > 0 || mshrStageN > 0 ||
		lookupN > 0 || mshrN > 0 || inflN > 0 ||
		infFetchN > 0 || infEvictN > 0 || pendEvictN > 0 ||
		topSN > 0 || rdmaSN > 0 || lookupSN > 0 || bDramSN > 0 || bRdmaSN > 0
	line = fmt.Sprintf(
		"top=%v rdma=%v lookup=%v dram=%v bRdma=%v ctrl=%v "+
			"dirToBank=%d wbToBank=%d wbBuf=%d mshrStage=%d "+
			"lookup=%d(H%d/F%d/P%d/C%d) mshrActive=%d inFlight=%d "+
			"inflightFetch=%d coalInflight=%d wbHeadCoal=%v inflightEvict=%d pendEvict=%d "+
			"topS=%d rdmaS=%d lookupS=%d bDramS=%d bRdmaS=%d "+
			"numToRemote=%d numCornerServe=%d",
		topQ, rdmaQ, lookupQ, dramQ, brdmaQ, ctrlQ,
		dirToBankN, wbToBankN, wbBufN, mshrStageN,
		lookupN, lkHome, lkReal, lkProbe, lkCoal, mshrN, inflN,
		infFetchN, coalInfN, wbHeadCoal, infEvictN, pendEvictN,
		topSN, rdmaSN, lookupSN, bDramSN, bRdmaSN,
		c.numToRemote, c.numCornerServe)
	return
}

func (c *Cache) SetRdmaReqAddressTable(t lmf.LowModuleFinder) {
	c.RdmaReqAddressTable = t
}

func (c *Cache) SetGridSize(row, col int) {
	c.row = row
	c.col = col
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
	madeProgress = c.runStage(now, c.lookupSender) || madeProgress
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

// check if the request goes to local memory
func (c *Cache) isReqToRemote(
	addr uint64,
) bool {
	return strings.Contains(c.lowModuleFinder.Find(addr).Name(), "RDMA")
}

// MSHR budget is split into three bands so each class always has reachable
// slots:
//   - mshrLocalReserve       : local-home requests only (the drain endpoint).
//   - mshrCornerServeReserve : corner-node lookup serves only. A corner lookup
//     (isCornerNode) reaching this chiplet must fetch from its home to advance
//     the NearFetch chain. It uses a REAL remote fetch (home is on a straight
//     line from the corner, hasCornerNode(addr)=false), so it competes for the
//     same numToRemote budget as generic remote misses. Without its own band a
//     generic-remote flood (local CU misses) fills numToRemote and the corner
//     serve goes not-ready — which deadlocks: the chain stalls at the corner,
//     never reaches home, the in-flight fetches never get a response, and
//     numToRemote never drains. (This band was removed and that is exactly the
//     deadlock observed: chiplets 0/32 stuck as corners with numToRemote=28.)
//   - the rest               : generic remote (RDMA) misses.
const (
	mshrLocalReserve       = 4
	mshrCornerServeReserve = 4
)

// isRemoteReqPossible gates a generic (non-corner-serve) remote miss against its
// OWN count only: generic in-flight must stay below capacity minus both
// reserves, so the local and corner-serve bands are always reachable.
func (c *Cache) isRemoteReqPossible() bool {
	limit := c.mshr.Capacity() - mshrLocalReserve - mshrCornerServeReserve
	if limit < 0 {
		limit = 0
	}
	return c.numToRemote < uint64(limit)
}

// isCornerServePossible gates a corner-node lookup serve against the dedicated
// corner-serve count only: it has its own quota (mshrCornerServeReserve) that a
// generic-remote flood cannot consume, so the lookup chain can always advance.
func (c *Cache) isCornerServePossible() bool {
	return c.numCornerServe < uint64(mshrCornerServeReserve)
}

// acquireRemoteBudget / releaseRemoteBudget keep the two per-class counters in
// lock-step with fetch issue/completion. A corner-serve fetch (isLookup &&
// isCornerNode) charges numCornerServe; every other remote fetch charges
// numToRemote. The release mirrors the acquire on the SAME transaction, so the
// class can never be mismatched between ++ and --.
func (c *Cache) acquireRemoteBudget(t *transaction) {
	if t.isLookup && t.isCornerNode {
		c.numCornerServe++
	} else {
		c.numToRemote++
	}
}

func (c *Cache) releaseRemoteBudget(t *transaction) {
	if t.isLookup && t.isCornerNode {
		if c.numCornerServe > 0 {
			c.numCornerServe--
		}
	} else {
		if c.numToRemote > 0 {
			c.numToRemote--
		}
	}
}

// hasCornerNode checks corner node is available or not,
// which is implemented as MSHR Coalescing in NearFetch
func (c *Cache) hasCornerNode(
	address uint64,
) bool {
	srcChipId := c.ChipletId
	dstChipId := int(c.HomeNodeFinder.GetChipletId(address))

	srcX := srcChipId % c.row
	srcY := srcChipId / c.row
	dstX := dstChipId % c.row
	dstY := dstChipId / c.row

	if srcX == dstX {
		return false
	} else {
		if srcY == dstY {
			return false
		} else {
			return true
		}
	}
}
