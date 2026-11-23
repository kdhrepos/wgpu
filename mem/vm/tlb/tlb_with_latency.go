package tlb

import (
	"fmt"
	"log"

	// "math"
	"reflect"

	// "strings"

	"gitlab.com/akita/akita"
	"gitlab.com/akita/lmf"
	"gitlab.com/akita/mem/device"
	"gitlab.com/akita/mem/vm/tlb/internal"
	"gitlab.com/akita/util"
	"gitlab.com/akita/util/pipelining"
	"gitlab.com/akita/util/tracing"
)

type tlbPipelineItem struct {
	taskID         uint64
	translationReq *device.TranslationReq
}

func (t tlbPipelineItem) TaskID() uint64 {
	return t.taskID
}

type L2TLB interface {
	tracing.NamedHookable
	GetTopPort() akita.Port
	GetBottomPort() akita.Port
	GetControlPort() akita.Port
	SetLowModuleFinder(lmf.LowModuleFinder)
	GetPipeline() pipelining.Pipeline
	GetFrontQueueLength() int
	GetNumReqPerCycle() int

	SetCommandProcessor(akita.Port)
}

// A LatTLB is a cache that maintains some page information.
type LatTLB struct {
	*akita.TickingComponent

	TopPort     akita.Port
	BottomPort  akita.Port
	ControlPort akita.Port

	LowModule       akita.Port
	LowModuleFinder lmf.LowModuleFinder

	log2PageSize   uint64
	log2NumSets    uint64
	setMask        uint64
	numSets        int
	numWays        int
	pageSize       uint64
	numReqPerCycle int
	latency        int
	indexingMask   uint64

	Sets []internal.Set

	lookupBuffer util.Buffer

	mshr                mshr
	respondingMSHREntry *mshrEntry

	isPaused bool

	CommandProcessor akita.Port

	setsAccessed []uint64 // sized to numSets in reset() (was fixed [256])
	mode4Kstrip  bool
	hysterisis   bool
}

func (tlb *LatTLB) GetPipeline() pipelining.Pipeline {
	return nil
}

// DeadlockSummary reports per-port "any msg pending" flags, lookupBuffer
// occupancy, and respondingMSHR flag.
func (tlb *LatTLB) DeadlockSummary() (line string, anyPending bool) {
	topQ := tlb.TopPort.Peek() != nil
	bottomQ := tlb.BottomPort.Peek() != nil
	ctrlQ := tlb.ControlPort.Peek() != nil
	lookupN := 0
	if tlb.lookupBuffer != nil {
		lookupN = tlb.lookupBuffer.Size()
	}
	respondingMSHR := tlb.respondingMSHREntry != nil
	anyPending = topQ || bottomQ || ctrlQ || lookupN > 0 || respondingMSHR
	line = fmt.Sprintf("top=%v bottom=%v ctrl=%v lookup=%d respondingMSHR=%v",
		topQ, bottomQ, ctrlQ, lookupN, respondingMSHR)
	return
}

// GetPipeline gets the pipeline in the LatTLB
func (tlb *LatTLB) GetTopPort() akita.Port {
	return tlb.TopPort
}

// GetPipeline gets the pipeline in the LatTLB
func (tlb *LatTLB) GetBottomPort() akita.Port {
	return tlb.BottomPort
}

// GetPipeline gets the pipeline in the LatTLB
func (tlb *LatTLB) GetControlPort() akita.Port {
	return tlb.ControlPort
}

// GetNumSets gets the number of sets in the LatTLB
func (tlb *LatTLB) GetNumSets() int {
	return tlb.numSets
}

// GetNumWays gets the number of ways in the LatTLB
func (tlb *LatTLB) GetNumWays() int {
	return tlb.numWays
}

func (tlb *LatTLB) GetNumReqPerCycle() int {
	return tlb.numReqPerCycle
}

func (tlb *LatTLB) SetLowModuleFinder(lmf lmf.LowModuleFinder) {
	tlb.LowModuleFinder = lmf
}

func (tlb *LatTLB) SetCommandProcessor(cp akita.Port) {
	tlb.CommandProcessor = cp
}

// Reset sets all the entries int he LatTLB to be invalid
func (tlb *LatTLB) reset() {
	tlb.Sets = make([]internal.Set, tlb.numSets)
	for i := 0; i < tlb.numSets; i++ {
		set := internal.NewSet(tlb.numWays)
		tlb.Sets[i] = set
	}
	tlb.setsAccessed = make([]uint64, tlb.numSets)
}

// Tick defines how LatTLB update states at each cycle
func (tlb *LatTLB) Tick(now akita.VTimeInSec) bool {
	madeProgress := false

	madeProgress = tlb.performCtrlReq(now) || madeProgress

	if tlb.GetFrontQueueLength() > 0 {
		tracing.StartTask(0, 0, now, tlb, "imbalance", "", nil)
	}

	if !tlb.isPaused {
		for i := 0; i < tlb.numReqPerCycle; i++ {
			madeProgress = tlb.respondMSHREntry(now) || madeProgress
		}

		for i := 0; i < tlb.numReqPerCycle; i++ {
			madeProgress = tlb.parseBottom(now) || madeProgress
		}

		for i := 0; i < tlb.numReqPerCycle; i++ {
			madeProgress = tlb.lookup(now) || madeProgress
		}

		for i := 0; i < tlb.numReqPerCycle; i++ {
			madeProgress = tlb.parseFromTop(now) || madeProgress
		}
	}
	return madeProgress
}

func (tlb *LatTLB) respondMSHREntry(now akita.VTimeInSec) bool {
	if tlb.respondingMSHREntry == nil {
		return false
	}

	mshrEntry := tlb.respondingMSHREntry
	page := mshrEntry.page
	req := mshrEntry.Requests[0]
	var accessResult device.AccessResult
	if mshrEntry.NumResponded() == 0 {
		accessResult = device.TLBMiss
	} else {
		accessResult = device.TLBMshrHit
	}
	rspToTop := device.TranslationRspBuilder{}.
		WithSendTime(now).
		WithSrc(tlb.TopPort).
		WithDst(req.Src).
		WithRspTo(req.ID).
		WithPage(page).
		WithAccessResult(accessResult).
		WithSrcL2TLB(tlb.Name()).
		Build()
	err := tlb.TopPort.Send(rspToTop)
	if err != nil {
		return false
	}
	mshrEntry.IncNumRespondedByOne()
	mshrEntry.Requests = mshrEntry.Requests[1:]
	if len(mshrEntry.Requests) == 0 {
		tlb.respondingMSHREntry = nil
	}
	tracing.StartTracingNetworkReq(rspToTop, now, tlb, req)
	tracing.TraceReqComplete(req, now, tlb)
	return true
}

func (tlb *LatTLB) parseFromTop(now akita.VTimeInSec) bool {
	msg := tlb.TopPort.Peek()
	if msg == nil {
		return false
	}

	req := msg.(*device.TranslationReq)

	if !tlb.lookupBuffer.CanPush() {
		return false
	}

	pipelineItem := tlbPipelineItem{
		taskID:         akita.GetIDGenerator().Generate(),
		translationReq: req,
	}
	tlb.lookupBuffer.Push(pipelineItem)
	tlb.TopPort.Retrieve(now)
	tracing.TraceReqReceive(req, now, tlb)
	tracing.StopTracingNetworkReq(req, now, tlb)
	tracing.StartTask(0, 0, now, tlb, "entropy", "", req)
	return true
}

func balanceCheck(perEpochWhatIf [4]uint64) bool {
	sum := uint64(0)
	for i := 0; i < 4; i++ {
		sum += perEpochWhatIf[i]
	}
	if sum == 0 {
		return false
	}
	for i := 0; i < 4; i++ {
		if (float32(perEpochWhatIf[i]) / float32(sum)) > 0.5 {
			return false
		}
	}
	return true
}

func (tlb *LatTLB) lookup(now akita.VTimeInSec) bool {
	// pop from waiting queue
	item := tlb.lookupBuffer.Peek()
	if item == nil {
		return false
	}
	pipelineItem := item.(tlbPipelineItem)
	req := pipelineItem.translationReq
	// if req.VAddr == 49152 {
	// fmt.Println(req.VAddr)
	// panic("oh no! dbchbbhw")
	// }
	mshrEntry := tlb.mshr.Query(req.PID, req.VAddr)
	if mshrEntry != nil {
		ok := tlb.processTLBMSHRHit(now, mshrEntry, req)
		if ok {
			tracing.AddTaskStep(
				tracing.MsgIDAtReceiver(req, tlb),
				now, tlb,
				"tlb-mshr-hit",
			)
			tlb.lookupBuffer.Pop()
			return true
		}
		return false
	}

	setID := tlb.vAddrToSetID(req.VAddr)
	// setID := tlb.vAddrToSetIDxor7(req.VAddr)
	set := tlb.Sets[setID]
	wayID, page, found := set.Lookup(req.PID, req.VAddr)
	if found && page.Valid {
		return tlb.handleTranslationHit(now, req, setID, wayID, page)
	}

	// passing setID for tracing purposes
	// TODO: use the return value of the following function to avoid passing setID
	return tlb.handleTranslationMiss(now, req, setID)
}

func (tlb *LatTLB) handleTranslationHit(
	now akita.VTimeInSec,
	req *device.TranslationReq,
	setID, wayID int,
	page device.Page,
) bool {
	ok := tlb.sendRspToTop(now, req, page)
	if !ok {
		return false
	}
	tlb.visit(setID, wayID)
	tlb.lookupBuffer.Pop()

	tracing.AddTaskStep(
		tracing.MsgIDAtReceiver(req, tlb),
		now, tlb,
		"tlb-hit",
	)
	tracing.TraceReqComplete(req, now, tlb)

	return true
}

func (tlb *LatTLB) handleTranslationMiss(
	now akita.VTimeInSec,
	req *device.TranslationReq,
	setID int,
) bool {
	if tlb.mshr.IsFull() {
		return false
	}

	fetched := tlb.fetchBottom(now, req)
	if !fetched {
		return false
	}

	tlb.lookupBuffer.Pop()
	tracing.AddTaskStep(
		tracing.MsgIDAtReceiver(req, tlb),
		now, tlb,
		"tlb-miss",
	)

	return true
}

// we are assuimg 4K pages here. TODO: FIX THIS!
func (tlb *LatTLB) vAddrToSetIDxor7(vAddr uint64) (setID int) {
	index := uint64(0)
	sp1 := (vAddr >> 12) & 0x7f
	sp2 := (vAddr >> 19) & 0x7f
	sp3 := (vAddr >> 26) & 0x7f
	sp4 := (vAddr >> 33) & 0x7f
	index = uint64(sp1 ^ sp2 ^ sp3 ^ sp4)
	return int(index)
}

func (tlb *LatTLB) vAddrToSetID(vAddr uint64) (setID int) {
	index := uint64(0)
	vpn := vAddr >> tlb.log2PageSize
	// usefulBits := (uint64(1) << 28) - 1
	for i := 0; i < 4; i++ {
		index ^= (vpn & tlb.setMask)
		vpn >>= tlb.log2NumSets
	}
	setID = int(index)
	tlb.setsAccessed[setID]++
	return
}

func (tlb *LatTLB) sendRspToTop(
	now akita.VTimeInSec,
	req *device.TranslationReq,
	page device.Page,
) bool {
	rsp := device.TranslationRspBuilder{}.
		WithSendTime(now).
		WithSrc(tlb.TopPort).
		WithDst(req.Src).
		WithRspTo(req.ID).
		WithPage(page).
		WithAccessResult(device.TLBHit).
		WithSrcL2TLB(tlb.Name()).
		Build()

	err := tlb.TopPort.Send(rsp)
	if err == nil {
		tracing.StartTracingNetworkReq(rsp, now, tlb, req)
		return true
	}
	return false
}

func (tlb *LatTLB) processTLBMSHRHit(
	now akita.VTimeInSec,
	mshrEntry *mshrEntry,
	req *device.TranslationReq,
) bool {
	// if len(mshrEntry.Requests) < 8 {
	mshrEntry.Requests = append(mshrEntry.Requests, req)
	return true
	// }
	// return false
}

func (tlb *LatTLB) fetchBottom(now akita.VTimeInSec, req *device.TranslationReq) bool {
	dstPort := tlb.LowModuleFinder.Find(req.VAddr)

	fetchBottom := device.TranslationReqBuilder{}.
		WithSendTime(now).
		WithSrc(tlb.BottomPort).
		WithDst(dstPort).
		WithPID(req.PID).
		WithVAddr(req.VAddr).
		WithInfo(req.Info).
		WithDeviceID(req.DeviceID).
		Build()
	err := tlb.BottomPort.Send(fetchBottom)
	if err != nil {
		return false
	}

	mshrEntry := tlb.mshr.Add(req.PID, req.VAddr)
	mshrEntry.Requests = append(mshrEntry.Requests, req)
	mshrEntry.reqToBottom = fetchBottom

	tracing.TraceReqInitiate(fetchBottom, now, tlb,
		tracing.MsgIDAtReceiver(req, tlb))

	return true
}

func (tlb *LatTLB) parseBottom(now akita.VTimeInSec) bool {
	if tlb.respondingMSHREntry != nil {
		return false
	}

	item := tlb.BottomPort.Peek()
	if item == nil {
		return false
	}

	rsp := item.(*device.TranslationRsp)
	page := rsp.Page

	mshrEntryPresent := tlb.mshr.IsEntryPresent(rsp.Page.PID, rsp.Page.VAddr)
	if !mshrEntryPresent {
		panic("oh no!")
		tlb.BottomPort.Retrieve(now)
		tracing.TraceReqFinalize(rsp, now, tlb)
		return true
	}

	setID := tlb.vAddrToSetID(page.VAddr)
	// setID := tlb.vAddrToSetIDxor7(page.VAddr)
	set := tlb.Sets[setID]
	wayID, ok := tlb.Sets[setID].Evict()
	if !ok {
		panic("failed to evict")
	}
	set.Update(wayID, page)
	set.Visit(wayID)

	mshrEntry := tlb.mshr.GetEntry(rsp.Page.PID, rsp.Page.VAddr)
	tlb.respondingMSHREntry = mshrEntry
	mshrEntry.page = page

	tlb.mshr.Remove(rsp.Page.PID, rsp.Page.VAddr)

	tlb.BottomPort.Retrieve(now)
	tracing.TraceReqFinalize(mshrEntry.reqToBottom, now, tlb)

	tracing.EndTask(tracing.StallTaskID(tlb.Name()), now, tlb)
	return true
}

func (tlb *LatTLB) performCtrlReq(now akita.VTimeInSec) bool {
	item := tlb.ControlPort.Peek()
	if item == nil {
		return false
	}

	var madeProgress bool
	switch req := item.(type) {
	case *TLBFlushReq:
		madeProgress = tlb.handleTLBFlush(now, req)
	case *TLBRestartReq:
		madeProgress = tlb.handleTLBRestart(now, req)
	default:
		log.Panicf("cannot process request %s", reflect.TypeOf(req))
	}
	if madeProgress {
		tlb.ControlPort.Retrieve(now)
		return true
	}
	return false
}

func (tlb *LatTLB) visit(setID, wayID int) int {
	set := tlb.Sets[setID]
	mruPosition := set.Visit(wayID)
	return mruPosition
}

func (tlb *LatTLB) handleTLBFlush(now akita.VTimeInSec, req *TLBFlushReq) bool {
	rsp := TLBFlushRspBuilder{}.
		WithSrc(tlb.ControlPort).
		WithDst(req.Src).
		WithSendTime(now).
		Build()

	err := tlb.ControlPort.Send(rsp)
	if err != nil {
		return false
	}

	for _, vAddr := range req.VAddr {
		// setID := tlb.vAddrToSetID(vAddr)
		setID := tlb.vAddrToSetIDxor7(vAddr)
		set := tlb.Sets[setID]
		wayID, page, found := set.Lookup(req.PID, vAddr)
		if !found {
			continue
		}

		page.Valid = false
		set.Update(wayID, page)
	}

	tlb.mshr.Reset()
	tlb.isPaused = true
	return true
}

func (tlb *LatTLB) handleTLBRestart(now akita.VTimeInSec, req *TLBRestartReq) bool {
	rsp := TLBRestartRspBuilder{}.
		WithSendTime(now).
		WithSrc(tlb.ControlPort).
		WithDst(req.Src).
		Build()

	err := tlb.ControlPort.Send(rsp)
	if err != nil {
		return false
	}

	tlb.isPaused = false

	for tlb.TopPort.Retrieve(now) != nil {
		tlb.TopPort.Retrieve(now)
	}

	for tlb.BottomPort.Retrieve(now) != nil {
		tlb.BottomPort.Retrieve(now)
	}

	return true
}

func (tlb *LatTLB) GetFrontQueueLength() int {
	bufContainer := tlb.GetTopPort().(akita.MsgBufferContainer) //.(*akita.LimitNumMsgPort)
	buf := bufContainer.GetBuffer()
	return len(buf)
}

func div(x, y float64) float64 {
	if y == 0 {
		return 0
	}
	return x / y
}
