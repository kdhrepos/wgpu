/*
1. parseFromTop -> startWalking
2. parseFromMem → handleMemResponse
    1. finalizeTransaction → doPageWalkHit
    2. fillPageWalkCache
3. parseFromPageWalkCache → handlePageWalkCacheResponse
4. walkPageTable
    1. sendToPageWalkCache
    2. sendToMem
5. topSender.Tick
6. Control Operation
*/

package mmu

import (
	"encoding/binary"
	"fmt"
	"log"
	"reflect"
	"strconv"

	"strings"

	"gitlab.com/akita/akita"
	"gitlab.com/akita/lmf"
	"gitlab.com/akita/mem/device"
	"gitlab.com/akita/mem/idealmemcontroller"
	"gitlab.com/akita/protocol"
	"gitlab.com/akita/util/akitaext"
	"gitlab.com/akita/util/tracing"
)

type transactionState int

const (
	newTransaction transactionState = iota
	sentToPageWalkCache
	pageWalkCacheDone
	sentToMem
	memDone
	transactionFinished
)

type transaction struct {
	req  *device.TranslationReq // original request
	page device.Page
	//cycleLeft int
	//migration *device.PageMigrationReqToDriver
	level             int
	msgID             uint64
	state             transactionState
	PPN               uint64
	vAddr             uint64
	remoteMemAccesses int
	startTime         akita.VTimeInSec // parseFromTop accepted the walk
	lastReqTime       akita.VTimeInSec // when current PWC/mem read was sent
}

// MMUImpl is the default mmu implementation. It is also an akita Component.
type MMUImpl struct {
	akita.TickingComponent

	ToTop            akita.Port
	ControlPort      akita.Port
	CommandProcessor akita.Port

	pageWalkCachePort akita.Port
	PageWalkCache     akita.Port
	topSender         akitaext.BufferedSender

	LocalTransPort  akita.Port
	RemoteTransPort akita.Port
	lowModuleFinder lmf.LowModuleFinder
	numChiplets     uint64

	pageTable *device.PageTableImpl
	//	latency             int
	maxRequestsInFlight int

	// WalkSum/WalkCount: per-walk service time inside the MMU, from
	// parseFromTop accepting the walk to doPageWalkHit sending the response
	// (PWC lookups + PTE fetch + network). Isolates the walk itself from the
	// queueing to enter the MMU.
	WalkSum   akita.VTimeInSec
	WalkCount uint64

	// Per-round-trip split of a walk. PwcWaitSum/PwcReqCount = PWC lookups;
	// MemWaitSum/MemReqCount = page-table memory (L2$/DRAM) reads. Counts
	// tell how many of each happen per walk; sums give avg round-trip time.
	PwcWaitSum  akita.VTimeInSec
	PwcReqCount uint64
	MemWaitSum  akita.VTimeInSec
	MemReqCount uint64

	walkingTranslations []transaction

	remoteMemAccessesInCurEpoch  uint64
	avgWalksEnqueuedInCurEpoch   float64
	remoteMemAccessesInPrevEpoch uint64
	avgWalksEnqueuedInPrevEpoch  float64
	memAccessesInCurEpoch        uint64
	memAccessesInPrevEpoch       uint64

	numWalksDone        uint64
	numWalksInCurEpoch  uint64
	numWalksInPrevEpoch uint64
	lastChecked         uint64
	sendStateInfo       bool
	// numWalksArrived          uint64
	interleaving uint64

	VirtualAddrTable lmf.LowModuleFinder
}

// DeadlockSummary reports the MMU's in-flight page-walk count and the
// per-port "any msg pending" flags. anyPending is true iff at least one
// bucket is non-empty.
func (mmu *MMUImpl) DeadlockSummary() (line string, anyPending bool) {
	walking := len(mmu.walkingTranslations)
	topQ := mmu.ToTop.Peek() != nil
	pwcQ := false
	if mmu.PageWalkCache != nil {
		pwcQ = mmu.PageWalkCache.Peek() != nil
	}
	localQ := false
	if mmu.LocalTransPort != nil {
		localQ = mmu.LocalTransPort.Peek() != nil
	}
	remoteQ := false
	if mmu.RemoteTransPort != nil {
		remoteQ = mmu.RemoteTransPort.Peek() != nil
	}
	ctrlQ := false
	if mmu.ControlPort != nil {
		ctrlQ = mmu.ControlPort.Peek() != nil
	}
	anyPending = walking > 0 || topQ || pwcQ || localQ || remoteQ || ctrlQ
	line = fmt.Sprintf(
		"walking=%d top=%v pwc=%v localTrans=%v remoteTrans=%v ctrl=%v",
		walking, topQ, pwcQ, localQ, remoteQ, ctrlQ)
	return
}

func (mmu *MMUImpl) GetNumReqPerCycle() int {
	return mmu.maxRequestsInFlight
}

// Tick defines how the MMU update state each cycle
func (mmu *MMUImpl) Tick(now akita.VTimeInSec) bool {
	madeProgress := false

	madeProgress = mmu.performCtrlReq(now) || madeProgress
	madeProgress = mmu.topSender.Tick(now) || madeProgress

	for i := 0; i < mmu.maxRequestsInFlight; i++ {
		madeProgress = mmu.parseFromMem(now) || madeProgress
	}
	for i := 0; i < mmu.maxRequestsInFlight; i++ {
		madeProgress = mmu.parseFromRemote(now) || madeProgress
	}
	for i := 0; i < mmu.maxRequestsInFlight; i++ {
		madeProgress = mmu.parseFromPageWalkCache(now) || madeProgress
	}

	madeProgress = mmu.walkPageTable(now) || madeProgress

	for i := 0; i < mmu.maxRequestsInFlight; i++ {
		madeProgress = mmu.parseFromTop(now) || madeProgress
	}

	return madeProgress
}

func (mmu *MMUImpl) parseFromTop(now akita.VTimeInSec) bool {
	if len(mmu.walkingTranslations) >= mmu.maxRequestsInFlight {
		return false
	}

	req := mmu.ToTop.Retrieve(now) // from l2 tlb or remote chiplet

	if req == nil {
		return false
	}

	switch req := req.(type) {
	case *device.TranslationReq:
		mmu.startWalking(req, now)
	default:
		log.Panicf("MMU canot handle request of type %s", reflect.TypeOf(req))
	}
	return true
}

/*
- This function sets top-level page table and set the translation.
- New transaction is saved at mmu.walkingTrasnlations.
*/
func (mmu *MMUImpl) startWalking(req *device.TranslationReq, now akita.VTimeInSec) {
	rearrangedVAddr := mmu.pageTable.Rearrange(req.VAddr)
	root := mmu.pageTable.GetRoot(req.PID)

	translationInPipeline := transaction{
		req: req,
		//cycleLeft: mmu.latency,
		level:     0,
		msgID:     0,
		state:     newTransaction,
		vAddr:     rearrangedVAddr,
		PPN:       root,
		startTime: now,
	}

	mmu.walkingTranslations = append(mmu.walkingTranslations, translationInPipeline)

	tracing.TraceReqReceive(req, now, mmu)
}

func (mmu *MMUImpl) parseFromMem(now akita.VTimeInSec) bool {
	madeProgress := false
	item := mmu.LocalTransPort.Peek()

	if item != nil {
		// destination should be mmu's translation port
		switch msg := item.(type) {
		case *protocol.DataReadyRsp:
			mmu.handleMemResponse(msg, now)
		default:
			panic("unknown message type")
		}
		madeProgress = true
	}
	mmu.LocalTransPort.Retrieve(now)
	return madeProgress
}

func (mmu *MMUImpl) parseFromRemote(now akita.VTimeInSec) bool {
	madeProgress := false
	item := mmu.RemoteTransPort.Peek()

	if item != nil {
		// destination should be mmu's translation port
		switch msg := item.(type) {
		case *protocol.DataReadyRsp:
			mmu.handleMemResponse(msg, now)
		default:
			panic("unknown message type")
		}
		madeProgress = true
	}
	mmu.RemoteTransPort.Retrieve(now)
	return madeProgress
}

func (mmu *MMUImpl) handleMemResponse(rsp *protocol.DataReadyRsp, now akita.VTimeInSec) {
	for i := 0; i < len(mmu.walkingTranslations); i++ {
		trans := &mmu.walkingTranslations[i]

		if trans.msgID == rsp.RespondTo {

			// rspInfo := rsp.Info.(*mem.DataReadyRspInfo)
			// accessResult := rspInfo.AccessResult
			// src := rspInfo.Src

			// taskStep := fmt.Sprintf("chiplet-%s-level-%d-%s",
			// 	getChipletNum(src),
			// 	trans.level,
			// 	getAccessResultString(accessResult))

			// tracing.AddTaskStep(
			// 	tracing.MMUMemLatencyTaskID(trans.msgID),
			// 	now, mmu,
			// 	taskStep,
			// )
			tracing.EndTask(
				tracing.MMUMemLatencyTaskID(trans.msgID),
				now,
				mmu,
			)

			if trans.lastReqTime != 0 {
				mmu.MemWaitSum += now - trans.lastReqTime
				mmu.MemReqCount++
			}

			rawPPN := binary.LittleEndian.Uint64(rsp.Data)
			trans.PPN = rawPPN
			trans.state = memDone

			if trans.level+1 == 4 {
				mmu.finalizeTransaction(now, i)
			} else {
				mmu.fillPageWalkCache(now, i)
			}
			trans.level++
		}
	}
}

func (mmu *MMUImpl) finalizeTransaction(
	now akita.VTimeInSec,
	walkingIndex int,
) bool {
	req := mmu.walkingTranslations[walkingIndex].req
	page, found := mmu.pageTable.Find(req.PID, req.VAddr)
	if !found {
		panic("page not found")
	}
	pAddr := mmu.walkingTranslations[walkingIndex].PPN
	if pAddr != page.PAddr {
		panic("addresses don't match!")
	}
	newPage := device.Page{PID: req.PID, VAddr: req.VAddr, PAddr: pAddr, Valid: true}
	mmu.walkingTranslations[walkingIndex].page = newPage
	mmu.walkingTranslations[walkingIndex].state = transactionFinished
	return mmu.doPageWalkHit(now, walkingIndex)
}

func (mmu *MMUImpl) fillPageWalkCache(now akita.VTimeInSec, i int) bool {
	trans := &mmu.walkingTranslations[i]
	level := uint64(trans.level)
	data := uint64ToBytes(trans.PPN | level)

	writeReq := protocol.WriteReqBuilder{}.
		WithSendTime(now).
		WithSrc(mmu.pageWalkCachePort).
		WithDst(mmu.PageWalkCache).
		WithPID(trans.req.PID).
		WithAddress(mmu.pageTable.AlignToPage(trans.req.VAddr) | level). //HUGEPAGES
		WithData(data).
		Build()
	trans.msgID = writeReq.ID

	mmu.pageWalkCachePort.Send(writeReq)
	return true
}

func (mmu *MMUImpl) doPageWalkHit(
	now akita.VTimeInSec,
	walkingIndex int,
) bool {
	if !mmu.topSender.CanSend(1) {
		return false
	}
	walking := mmu.walkingTranslations[walkingIndex]

	rsp := device.TranslationRspBuilder{}.
		WithSendTime(now).
		WithSrc(mmu.ToTop).
		WithDst(walking.req.Src).
		WithRspTo(walking.req.ID).
		WithPage(walking.page).
		Build()

	mmu.topSender.Send(rsp)
	if walking.startTime != 0 {
		mmu.WalkSum += now - walking.startTime
		mmu.WalkCount++
	}
	// mmu.toRemoveFromPTW = append(mmu.toRemoveFromPTW, walkingIndex)
	// tracing.StartTracingNetworkReq(rsp, now, mmu, walking.req)
	tracing.TraceReqComplete(walking.req, now, mmu)

	return true
}

func (mmu *MMUImpl) parseFromPageWalkCache(now akita.VTimeInSec) bool {
	madeProgress := false
	item := mmu.pageWalkCachePort.Peek()
	if item != nil {
		switch msg := item.(type) {
		case *protocol.DataReadyRsp:
			mmu.handlePageWalkCacheResponse(msg, now)
		case *protocol.WriteDoneRsp:
		default:
			panic("unknown message type")
		}
		madeProgress = true
	}
	mmu.pageWalkCachePort.Retrieve(now)
	return madeProgress
}

func (mmu *MMUImpl) handlePageWalkCacheResponse(
	rsp *protocol.DataReadyRsp,
	now akita.VTimeInSec,
) {
	for i := 0; i < len(mmu.walkingTranslations); i++ {
		trans := &mmu.walkingTranslations[i]
		if trans.msgID == rsp.RespondTo {

			if rsp.Data != nil {
				rspData := binary.LittleEndian.Uint64(rsp.Data)
				trans.PPN = rspData & ^uint64(3)
				level := int(rspData & uint64(3))
				trans.vAddr = mmu.pageTable.MoveToLevel(trans.vAddr, level+1)

				// Hit at the deepest matched level.
				tracing.AddTaskStep(tracing.MsgIDAtReceiver(trans.req, mmu),
					now, mmu, "pwc-hit-level"+strconv.Itoa(level))
				// Longest-prefix semantics: levels deeper than the hit
				// level are confirmed absent from the PWC. Levels above
				// the hit level are not probed by this lookup (unknown),
				// so they are intentionally not counted as hit or miss.
				for l := level + 1; l <= 2; l++ {
					tracing.AddTaskStep(tracing.MsgIDAtReceiver(trans.req, mmu),
						now, mmu, "pwc-miss-level"+strconv.Itoa(l))
				}

				trans.level = level + 1

				// h := fmt.Sprintf("%x", trans.req.VAddr)
				// h1 := fmt.Sprintf("%x", trans.PPN)
				// h2 := fmt.Sprintf("%x", trans.vAddr)
				// fmt.Println("response from pwc:", h, h1, level, h2)
			} else {
				// Complete PWC miss: no cached entry covers this VA at
				// any of the cacheable levels (0/1/2). Level 3 is leaf
				// and never cached in the PWC, so it is excluded.
				for l := 0; l <= 2; l++ {
					tracing.AddTaskStep(tracing.MsgIDAtReceiver(trans.req, mmu),
						now, mmu, "pwc-miss-level"+strconv.Itoa(l))
				}
			}
			trans.state = pageWalkCacheDone
			if trans.lastReqTime != 0 {
				mmu.PwcWaitSum += now - trans.lastReqTime
				mmu.PwcReqCount++
			}
			//trace point
		}
	}
}

func (mmu *MMUImpl) walkPageTable(now akita.VTimeInSec) bool {
	numActiveTransactions := len(mmu.walkingTranslations)
	tracing.StartTask(0, 0, now, mmu, "num_active_walkers", fmt.Sprintf("%d", numActiveTransactions), nil)
	// madeProgress must reflect ACTUAL progress (a state transition / send /
	// finish), not merely "walks exist". The old `len > 0` made walkPageTable
	// return true every tick while any walk was in flight — even when all
	// walks were blocked waiting for a PWC/mem response — so the MMU ticked
	// forever and the engine never quiesced, turning a downstream stall into
	// an undetectable livelock.
	madeProgress := false
	tmp := mmu.walkingTranslations[:0]

	for i := 0; i < numActiveTransactions; i++ {
		// fmt.Println("")
		trans := &mmu.walkingTranslations[i]

		switch trans.state {
		case newTransaction:
			madeProgress = mmu.sendToPageWalkCache(now, i) || madeProgress
		case pageWalkCacheDone:
			madeProgress = mmu.sendToMem(now, i) || madeProgress
		case memDone:
			madeProgress = mmu.sendToMem(now, i) || madeProgress
		}
		if trans.state != transactionFinished {
			tmp = append(tmp, *trans)
		} else {
			madeProgress = true
		}
	}

	mmu.walkingTranslations = tmp
	return madeProgress
}

/* Send address translation request to page walk cache */
func (mmu *MMUImpl) sendToPageWalkCache(now akita.VTimeInSec, i int) bool {
	trans := &mmu.walkingTranslations[i]
	transState := trans.state
	if transState != newTransaction {
		panic("this state shouldn't be here!")
	}
	readReq := protocol.ReadReqBuilder{}.
		WithSendTime(now).
		WithSrc(mmu.pageWalkCachePort).
		WithDst(mmu.PageWalkCache).
		WithPID(trans.req.PID).
		WithAddress(mmu.pageTable.AlignToPage(trans.req.VAddr)).
		WithByteSize(8).
		Build()

	trans.msgID = readReq.ID
	err := mmu.pageWalkCachePort.Send(readReq)
	if err == nil {
		trans.state = sentToPageWalkCache
		trans.lastReqTime = now

		return true
	}

	return false
}

func (mmu *MMUImpl) sendToMem(now akita.VTimeInSec, i int) bool {
	trans := &mmu.walkingTranslations[i]
	transState := trans.state
	if transState != pageWalkCacheDone && transState != memDone {
		panic("this state shouldn't be here!")
	}
	PPN := trans.PPN

	// physical address (PPN) with offset, which is index of page table entry.
	PPNWithOffset := mmu.pageTable.AddOffset(PPN, trans.vAddr)

	// DIAG (bptree wild-VA): PPN==0 at any level = unmapped PTE (or bad
	// root/PID). Next read targets sub-base PA -> "address is smaller than
	// offset" DRAM panic. Print the faulting VA before the crash so the
	// culprit access is identifiable. REMOVE once root cause is fixed.
	if PPN == 0 {
		log.Printf("MMU wild page-walk: PID=%d faultingVAddr=0x%x level=%d "+
			"PPN=0 (unmapped) -> would read PA 0x%x (becomes the DRAM "+
			"'address is smaller than offset' panic)",
			trans.req.PID, trans.req.VAddr, trans.level, PPNWithOffset)
	}

	// dstPort should be DRAM.ToTop or chipRdmaEngine.PwPort
	dstPort := mmu.lowModuleFinder.Find(PPNWithOffset)

	var err *akita.SendError
	if strings.Contains(dstPort.Name(), "RDMA") {
		readReq := protocol.ReadReqBuilder{}.
			WithSendTime(now).
			WithSrc(mmu.RemoteTransPort).
			WithDst(dstPort).
			WithPID(trans.req.PID).
			WithAddress(PPNWithOffset).
			WithByteSize(8).
			Build()
		trans.msgID = readReq.ID

		err = mmu.RemoteTransPort.Send(readReq)

		if err == nil {
			tracing.AddTaskStep(tracing.MsgIDAtReceiver(trans.req, mmu),
				now, mmu, "pw-level-"+strconv.Itoa(trans.level)+"-remote-reqs")
			// tracing.AddTaskStep(tracing.MsgIDAtReceiver(trans.req, mmu),
			// 	now, mmu, "page_walk_req_remote")
			// if trans.level == 3 {
			// 	tracing.AddTaskStep(tracing.MsgIDAtReceiver(trans.req, mmu),
			// 		now, mmu, "pw-level-3-remote-reqs")
			// }
		}
	} else {
		readReq := protocol.ReadReqBuilder{}.
			WithSendTime(now).
			WithSrc(mmu.LocalTransPort).
			WithDst(dstPort).
			WithPID(trans.req.PID).
			WithAddress(PPNWithOffset).
			WithInfo(trans.req.Info).
			WithByteSize(8).
			Build()
		readReq.IsPtw = true
		trans.msgID = readReq.ID

		toDramMsg := &idealmemcontroller.ToDramMsg{
			Msg:       readReq,
			FromLocal: true,
		}
		err = mmu.LocalTransPort.Send(toDramMsg)

		if err == nil {
			// if this condition is false, the platform uses pte caching.
			if strings.Contains(dstPort.Name(), "DRAM") {
				tracing.AddTaskStep(tracing.MsgIDAtReceiver(trans.req, mmu),
					now, mmu, "pw-level-"+strconv.Itoa(trans.level)+"-local-reqs")
			}
		}
	}

	if err == nil {
		tracing.StartTask(
			0,
			0,
			now,
			mmu,
			"pte-trace",
			mmu.VirtualAddrTable.Find(PPNWithOffset).Name(),
			&tracing.PTAccessInfo{
				PPN:      trans.PPN,
				PteIndex: trans.vAddr & 511,
				PC:       trans.req.Info,
				Level:    trans.level,
			},
		)

		trans.state = sentToMem
		trans.lastReqTime = now
		trans.vAddr = mmu.pageTable.NextLevel(trans.vAddr)

		return true
	}
	return false
}

func (mmu *MMUImpl) performCtrlReq(now akita.VTimeInSec) bool {
	item := mmu.ControlPort.Peek()
	if item == nil {
		return false
	}
	madeProgress := false
	switch req := item.(type) {
	default:
		log.Panicf("cannot process request %s", reflect.TypeOf(req))
	}
	if madeProgress {
		mmu.ControlPort.Retrieve(now)
		return true
	}
	panic("something is wrong!")
}

func getChipletNum(component string) (chipletNum string) {
	chipletNum = strings.Split(component, "_")[1][1:2]
	return
}

func uint64ToBytes(data uint64) []byte {
	bytes := make([]byte, 8)
	binary.LittleEndian.PutUint64(bytes, data)
	return bytes
}

// SetLowModuleFinder sets the table recording where to find an address.
func (mmu *MMUImpl) SetLowModuleFinder(lmf lmf.LowModuleFinder) {
	mmu.lowModuleFinder = lmf
}

func (mmu *MMUImpl) GetNumActiveWalkers() int {
	return len(mmu.walkingTranslations)
}
