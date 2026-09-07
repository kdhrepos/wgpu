package tlb

import (
	"fmt"

	"log"
	"reflect"
	"strings"

	"gitlab.com/akita/akita"
	"gitlab.com/akita/lmf"
	"gitlab.com/akita/mem/device"
	"gitlab.com/akita/mem/vm/tlb/internal"
	"gitlab.com/akita/util/tracing"
)

// A TLB is a cache that maintains some page information.
type TLB struct {
	*akita.TickingComponent

	TopPort     akita.Port
	BottomPort  akita.Port
	ControlPort akita.Port

	LowModule       akita.Port
	LowModuleFinder lmf.LowModuleFinder

	numSets        int
	numWays        int
	pageSize       uint64
	numReqPerCycle int

	Sets []internal.Set

	mshr                mshr
	respondingMSHREntry *mshrEntry

	isPaused bool
}

// GetNumSets gets the number of sets in the TLB
func (tlb *TLB) GetNumSets() int {
	return tlb.numSets
}

// GetNumWays gets the number of ways in the TLB
func (tlb *TLB) GetNumWays() int {
	return tlb.numWays
}

func (tlb *TLB) GetNumReqPerCycle() int {
	return tlb.numReqPerCycle
}

// DeadlockSummary reports per-port "any msg pending" flags. anyPending is true
// iff at least one port has a queued msg or an MSHR-responding entry is still
// being drained.
func (tlb *TLB) DeadlockSummary() (line string, anyPending bool) {
	topQ := tlb.TopPort.Peek() != nil
	bottomQ := tlb.BottomPort.Peek() != nil
	ctrlQ := tlb.ControlPort.Peek() != nil
	respondingMSHR := tlb.respondingMSHREntry != nil
	anyPending = topQ || bottomQ || ctrlQ || respondingMSHR
	line = fmt.Sprintf("top=%v bottom=%v ctrl=%v respondingMSHR=%v",
		topQ, bottomQ, ctrlQ, respondingMSHR)
	return
}

func (tlb *TLB) SetLowModuleFinder(lmf lmf.LowModuleFinder) {
	tlb.LowModuleFinder = lmf
}

// Reset sets all the entries int he TLB to be invalid
func (tlb *TLB) reset() {
	tlb.Sets = make([]internal.Set, tlb.numSets)
	for i := 0; i < tlb.numSets; i++ {
		set := internal.NewSet(tlb.numWays)
		tlb.Sets[i] = set
	}
}

// Tick defines how TLB update states at each cycle
func (tlb *TLB) Tick(now akita.VTimeInSec) bool {
	madeProgress := false

	madeProgress = tlb.performCtrlReq(now) || madeProgress

	if !tlb.isPaused {
		for i := 0; i < tlb.numReqPerCycle; i++ {
			madeProgress = tlb.respondMSHREntry(now) || madeProgress
		}

		for i := 0; i < tlb.numReqPerCycle; i++ {
			madeProgress = tlb.lookup(now) || madeProgress
		}

		for i := 0; i < tlb.numReqPerCycle; i++ {
			madeProgress = tlb.parseBottom(now) || madeProgress
		}
	}

	return madeProgress
}

func (tlb *TLB) respondMSHREntry(now akita.VTimeInSec) bool {
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
		Build()
	err := tlb.TopPort.Send(rspToTop)
	if err != nil {
		return false
	}
	if mshrEntry.NumResponded() > 0 {
		tracing.EndTask(tracing.TLBMshrWaitTaskID(req.Meta().ID), now, tlb)
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

func (tlb *TLB) lookup(now akita.VTimeInSec) bool {
	msg := tlb.TopPort.Peek()
	if msg == nil {
		return false
	}
	req := msg.(*device.TranslationReq)

	mshrEntry := tlb.mshr.Query(req.PID, req.VAddr)
	if mshrEntry != nil {
		ok := tlb.processTLBMSHRHit(now, mshrEntry, req)
		if ok {
			tracing.TraceReqReceive(req, now, tlb)
			tracing.AddTaskStep(
				tracing.MsgIDAtReceiver(req /*mshrEntry.Requests[0]*/, tlb),
				now, tlb,
				"tlb-mshr-hit",
			)
			tlb.TopPort.Retrieve(now)
			tracing.TraceReqReceive(req, now, tlb)
			tracing.StopTracingNetworkReq(req, now, tlb)
			return true
		}
		return false
	}

	setID := tlb.vAddrToSetID(req.VAddr)
	set := tlb.Sets[setID]
	wayID, page, found := set.Lookup(req.PID, req.VAddr)
	if found && page.Valid {
		return tlb.handleTranslationHit(now, req, setID, wayID, page)
	}

	return tlb.handleTranslationMiss(now, req)
}

func (tlb *TLB) handleTranslationHit(
	now akita.VTimeInSec,
	req *device.TranslationReq,
	setID, wayID int,
	page device.Page,
) bool {
	ok := tlb.sendRspToTop(now, req, page)
	if !ok {
		return false
	}

	// mruPos := tlb.visit(setID, wayID)
	tlb.visit(setID, wayID)
	tlb.TopPort.Retrieve(now)

	tracing.TraceReqReceive(req, now, tlb)
	tracing.StopTracingNetworkReq(req, now, tlb)
	tracing.AddTaskStep(
		tracing.MsgIDAtReceiver(req, tlb),
		now, tlb,
		"tlb-hit",
	)
	// tracing.AddTaskStep(
	// 	tracing.MsgIDAtReceiver(req, tlb),
	// 	now, tlb,
	// 	fmt.Sprintf("tlb-hit-at-set-%d-mru-pos-%d", setID, mruPos),
	// )
	// s := fmt.Sprintf("tlb-hit-at-set-%d-mru-pos-%d\n", setID, mruPos)
	// fmt.Println(s)
	tracing.TraceReqComplete(req, now, tlb)

	return true
}

func (tlb *TLB) handleTranslationMiss(
	now akita.VTimeInSec,
	req *device.TranslationReq,
) bool {
	// if tlb.shouldDoPageWalk(req.VAddr) {
	if tlb.mshr.IsFull() {
		tracing.StartTask(0, 0, now, tlb, "tlb-mshr-stall", "", nil)
		return false
	}
	fetched := tlb.fetchBottom(now, req)
	if fetched {
		tlb.TopPort.Retrieve(now)
		tracing.StopTracingNetworkReq(req, now, tlb)
		tracing.TraceReqReceive(req, now, tlb)
		tracing.AddTaskStep(
			tracing.MsgIDAtReceiver(req, tlb),
			now, tlb,
			"tlb-miss",
		)
		return true
	}
	return false
}

func (tlb *TLB) vAddrToSetID(vAddr uint64) (setID int) {
	return int(vAddr / tlb.pageSize % uint64(tlb.numSets))
}

func (tlb *TLB) sendRspToTop(
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
		Build()

	err := tlb.TopPort.Send(rsp)
	if err == nil {
		tracing.StartTracingNetworkReq(rsp, now, tlb, req)
		return true
	}
	return false
}

func (tlb *TLB) processTLBMSHRHit(
	now akita.VTimeInSec,
	mshrEntry *mshrEntry,
	req *device.TranslationReq,
) bool {
	// if len(mshrEntry.Requests) < 8 {
	// 	mshrEntry.Requests = append(mshrEntry.Requests, req)
	// 	return true
	// }
	// return false
	mshrEntry.Requests = append(mshrEntry.Requests, req)
	tracing.StartTask(
		tracing.TLBMshrWaitTaskID(req.Meta().ID),
		tracing.MsgIDAtReceiver(req, tlb),
		now, tlb, "tlb-mshr-hit-wait", "", nil,
	)
	return true
	/*	tracing.AddTaskStep(
		tracing.MsgIDAtReceiver(req /*mshrEntry.Requests[0], tlb),
		now, tlb,
		"tlb-mshr-hit",
	) */
}

func (tlb *TLB) fetchBottom(now akita.VTimeInSec, req *device.TranslationReq) bool {
	dstPort := tlb.LowModuleFinder.Find(req.VAddr)

	fetchBottom := device.TranslationReqBuilder{}.
		WithSendTime(now).
		WithSrc(tlb.BottomPort).
		WithDst(dstPort).
		WithInfo(req.Info).
		WithPID(req.PID).
		WithVAddr(req.VAddr).
		WithDeviceID(req.DeviceID).
		Build()
	err := tlb.BottomPort.Send(fetchBottom)
	if err != nil {
		return false
	}

	mshrEntry := tlb.mshr.Add(req.PID, req.VAddr)
	mshrEntry.Requests = append(mshrEntry.Requests, req)
	mshrEntry.reqToBottom = fetchBottom

	tracing.StartTask(
		tracing.L2TLBStatsTaskID(fetchBottom.Meta().ID),
		tracing.MsgIDAtReceiver(req, tlb),
		now,
		tlb,
		"L2TLB_stats",
		reflect.TypeOf(req).String(),
		req,
	)
	tracing.StartTracingNetworkReq(fetchBottom, now, tlb, req)
	// tracing.TraceReqReceive(req, now, tlb)
	tracing.TraceReqInitiate(fetchBottom, now, tlb,
		tracing.MsgIDAtReceiver(req, tlb))

	return true
}

func (tlb *TLB) getRemoteVLocal(rspSrc string) (str string) {
	if getChipletNum(tlb.Name()) != getChipletNum(rspSrc) {
		str = "remote"
	} else {
		str = "local"
	}
	return
}

func getChipletNum(srcL2TLB string) (i string) {
	i = "chiplet-" + strings.Split(srcL2TLB, "_")[1][1:2]
	return
}

func getTaskStep(origin string, accessResult device.AccessResult) (step string) {
	step = origin + "-"
	switch accessResult {
	case device.TLBHit:
		step += "TLBHit"
	case device.TLBMiss:
		step += "TLBMiss"
	case device.TLBMshrHit:
		step += "TLBMshrHit"
	}
	return
}

func (tlb *TLB) parseBottom(now akita.VTimeInSec) bool {
	if tlb.respondingMSHREntry != nil {
		return false
	}

	item := tlb.BottomPort.Peek()
	if item == nil {
		return false
	}

	rsp := item.(*device.TranslationRsp)
	page := rsp.Page
	//	fmt.Println(rsp.Meta().ID)//, req.Meta().ID)

	mshrEntryPresent := tlb.mshr.IsEntryPresent(rsp.Page.PID, rsp.Page.VAddr)
	if !mshrEntryPresent {
		panic("cannot find mshr entry when receiving translation response")
	}

	setID := tlb.vAddrToSetID(page.VAddr)
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

	tracing.StopTracingNetworkReq(rsp, now, tlb)
	tracing.TraceReqFinalize(mshrEntry.reqToBottom, now, tlb)

	return true
}

func (tlb *TLB) performCtrlReq(now akita.VTimeInSec) bool {
	item := tlb.ControlPort.Peek()
	if item == nil {
		return false
	}

	item = tlb.ControlPort.Retrieve(now)

	switch req := item.(type) {
	case *TLBFlushReq:
		return tlb.handleTLBFlush(now, req)
	case *TLBRestartReq:
		return tlb.handleTLBRestart(now, req)
	default:
		log.Panicf("cannot process request %s", reflect.TypeOf(req))
	}

	return true
}

func (tlb *TLB) visit(setID, wayID int) int {
	set := tlb.Sets[setID]
	mruPosition := set.Visit(wayID)
	return mruPosition
}

func (tlb *TLB) handleTLBFlush(now akita.VTimeInSec, req *TLBFlushReq) bool {
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
		setID := tlb.vAddrToSetID(vAddr)
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

func (tlb *TLB) handleTLBRestart(now akita.VTimeInSec, req *TLBRestartReq) bool {
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

	for tlb.TopPort.Peek() != nil {
		tlb.TopPort.Retrieve(now)
	}

	for tlb.BottomPort.Peek() != nil {
		tlb.BottomPort.Retrieve(now)
	}

	return true
}
