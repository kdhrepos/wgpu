package addresstranslator

import (
	"fmt"
	"log"
	"reflect"

	"gitlab.com/akita/akita"
	"gitlab.com/akita/lmf"
	"gitlab.com/akita/mem/device"
	"gitlab.com/akita/protocol"
	"gitlab.com/akita/util/tracing"
)

// AddressTranslator is a component that forwards the read/write requests with
// the address translated from virtual to physical.
type IdealAddressTranslator struct {
	*akita.TickingComponent

	TopPort    akita.Port
	BottomPort akita.Port
	CtrlPort   akita.Port

	lowModuleFinder lmf.LowModuleFinder
	log2PageSize    uint64
	deviceID        uint64
	numReqPerCycle  int

	isFlushing bool

	inflightReqToBottom []reqToBottom

	pageTable device.PageTable
}

// DeadlockSummary reports per-port pending flags + in-flight bottom-bound req
// count.
func (t *IdealAddressTranslator) DeadlockSummary() (line string, anyPending bool) {
	topQ := t.TopPort != nil && t.TopPort.Peek() != nil
	bottomQ := t.BottomPort != nil && t.BottomPort.Peek() != nil
	ctrlQ := t.CtrlPort != nil && t.CtrlPort.Peek() != nil
	inflN := len(t.inflightReqToBottom)
	anyPending = topQ || bottomQ || ctrlQ || inflN > 0
	line = fmt.Sprintf("top=%v bottom=%v ctrl=%v inFlight=%d flushing=%v",
		topQ, bottomQ, ctrlQ, inflN, t.isFlushing)
	return
}

// SetTranslationProvider sets the remote port that can translate addresses.
func (t *IdealAddressTranslator) SetTranslationProvider(p interface{}) {
	t.pageTable = p.(device.PageTable)
}

// SetLowModuleFinder sets the table recording where to find an address.
func (t *IdealAddressTranslator) SetLowModuleFinder(lmf lmf.LowModuleFinder) {
	t.lowModuleFinder = lmf
}

// GetCtrlPort sets the table recording where to find an address.
func (t *IdealAddressTranslator) GetCtrlPort() (port akita.Port) {
	port = t.CtrlPort
	return
}

// GetTopPort sets the table recording where to find an address.
func (t *IdealAddressTranslator) GetTopPort() (port akita.Port) {
	port = t.TopPort
	return
}

// GetTopPort sets the table recording where to find an address.
func (t *IdealAddressTranslator) GetTranslationPort() (port akita.Port) {
	panic("Should not ask for translation port in idealaddresstranslator")
	return
}

// GetBottomPort sets the table recording where to find an address.
func (t *IdealAddressTranslator) GetBottomPort() (port akita.Port) {
	port = t.BottomPort
	return
}

func (t *IdealAddressTranslator) GetNumReqPerCycle() int {
	return t.numReqPerCycle
}

// Tick updates state at each cycle.
func (t *IdealAddressTranslator) Tick(now akita.VTimeInSec) bool {
	madeProgress := false

	if !t.isFlushing {
		madeProgress = t.runPipeline(now)
	}

	madeProgress = t.handleCtrlRequest(now) || madeProgress

	return madeProgress
}

func (t *IdealAddressTranslator) runPipeline(now akita.VTimeInSec) bool {
	madeProgress := false

	for i := 0; i < t.numReqPerCycle; i++ {
		madeProgress = t.respond(now) || madeProgress
	}

	// for i := 0; i < t.numReqPerCycle; i++ {
	// 	madeProgress = t.parseTranslation(now) || madeProgress
	// }

	for i := 0; i < t.numReqPerCycle; i++ {
		madeProgress = t.translate(now) || madeProgress
	}

	return madeProgress
}

func (t *IdealAddressTranslator) translate(now akita.VTimeInSec) bool {
	item := t.TopPort.Peek()
	if item == nil {
		return false
	}

	reqFromTop := item.(protocol.AccessReq)
	vAddr := reqFromTop.GetAddress()
	PID := reqFromTop.GetPID()
	vPageID := t.addrToPageID(vAddr)
	page, ok := t.pageTable.Find(PID, vPageID)
	if !ok {
		panic("Could not find virtual to physical address translation!")
	}
	translatedReq := t.createTranslatedReq(
		reqFromTop,
		page)
	translatedReq.Meta().SendTime = now
	err := t.BottomPort.Send(translatedReq)
	if err != nil {
		return false
	}

	t.inflightReqToBottom = append(t.inflightReqToBottom,
		reqToBottom{
			reqFromTop:  reqFromTop,
			reqToBottom: translatedReq,
		})

	t.TopPort.Retrieve(now)

	// tracing.StopTracingNetworkReq(reqFromTop, now, t)
	// tracing.StartTracingNetworkReq(translatedReq, now, t, reqFromTop)
	tracing.StartTask(
		tracing.AddrTransStatsTaskID(reqFromTop.Meta().ID),
		tracing.MsgIDAtReceiver(reqFromTop, t),
		now,
		t,
		"addr-translator-stats",
		reflect.TypeOf(reqFromTop).String(),
		reqFromTop,
	)
	tracing.TraceReqFinalize(reqFromTop, now, t)
	tracing.TraceReqInitiate(translatedReq, now, t,
		tracing.MsgIDAtReceiver(reqFromTop, t))

	return true
}

func (t *IdealAddressTranslator) respond(now akita.VTimeInSec) bool {
	rsp := t.BottomPort.Peek()
	if rsp == nil {
		return false
	}

	reqInBottom := false

	var reqFromTop protocol.AccessReq
	var reqToBottomCombo reqToBottom
	var rspToTop protocol.AccessRsp
	switch rsp := rsp.(type) {
	case *protocol.DataReadyRsp:
		reqInBottom = t.isReqInBottomByID(rsp.RespondTo)
		if reqInBottom {
			reqToBottomCombo = t.findReqToBottomByID(rsp.RespondTo)
			reqFromTop = reqToBottomCombo.reqFromTop
			drToTop := protocol.DataReadyRspBuilder{}.
				WithSendTime(now).
				WithSrc(t.TopPort).
				WithDst(reqFromTop.Meta().Src).
				WithRspTo(reqFromTop.Meta().ID).
				WithData(rsp.Data).
				Build()
			rspToTop = drToTop
		}
	case *protocol.WriteDoneRsp:
		reqInBottom = t.isReqInBottomByID(rsp.RespondTo)
		if reqInBottom {
			reqToBottomCombo = t.findReqToBottomByID(rsp.RespondTo)
			reqFromTop = reqToBottomCombo.reqFromTop
			rspToTop = protocol.WriteDoneRspBuilder{}.
				WithSendTime(now).
				WithSrc(t.TopPort).
				WithDst(reqFromTop.Meta().Src).
				WithRspTo(reqFromTop.Meta().ID).
				Build()
		}
	default:
		log.Panicf("cannot handle respond of type %s", reflect.TypeOf(rsp))
	}

	if reqInBottom {
		err := t.TopPort.Send(rspToTop)
		if err != nil {
			return false
		}

		t.removeReqToBottomByID(rsp.(protocol.AccessRsp).GetRespondTo())

		tracing.AddTaskStep(
			tracing.AddrTransStatsTaskID(reqToBottomCombo.reqFromTop.Meta().ID),
			now, t,
			"req-latency",
		)
		tracing.EndTask(
			tracing.AddrTransStatsTaskID(reqToBottomCombo.reqFromTop.Meta().ID),
			now,
			t,
		)
		tracing.TraceReqFinalize(reqToBottomCombo.reqToBottom, now, t)
		tracing.TraceReqComplete(reqToBottomCombo.reqFromTop, now, t)
	}

	t.BottomPort.Retrieve(now)
	return true
}

func (t *IdealAddressTranslator) createTranslatedReq(
	req protocol.AccessReq,
	page device.Page,
) protocol.AccessReq {
	switch req := req.(type) {
	case *protocol.ReadReq:
		return t.createTranslatedReadReq(req, page)
	case *protocol.WriteReq:
		return t.createTranslatedWriteReq(req, page)
	default:
		log.Panicf("cannot translate request of type %s", reflect.TypeOf(req))
		return nil
	}
}

func (t *IdealAddressTranslator) createTranslatedReadReq(
	req *protocol.ReadReq,
	page device.Page,
) *protocol.ReadReq {
	offset := req.Address % (1 << t.log2PageSize)
	addr := page.PAddr + offset
	clone := protocol.ReadReqBuilder{}.
		WithSrc(t.BottomPort).
		WithDst(t.lowModuleFinder.Find(addr)).
		WithAddress(addr).
		WithByteSize(req.AccessByteSize).
		WithPID(1).
		WithInfo(req.Info).
		Build()
	clone.CanWaitForCoalesce = req.CanWaitForCoalesce
	return clone
}

func (t *IdealAddressTranslator) createTranslatedWriteReq(
	req *protocol.WriteReq,
	page device.Page,
) *protocol.WriteReq {
	offset := req.Address % (1 << t.log2PageSize)
	addr := page.PAddr + offset
	clone := protocol.WriteReqBuilder{}.
		WithSrc(t.BottomPort).
		WithDst(t.lowModuleFinder.Find(addr)).
		WithData(req.Data).
		WithDirtyMask(req.DirtyMask).
		WithAddress(addr).
		WithPID(1).
		WithInfo(req.Info).
		Build()
	clone.CanWaitForCoalesce = req.CanWaitForCoalesce
	return clone
}

func (t *IdealAddressTranslator) addrToPageID(addr uint64) uint64 {
	return (addr >> t.log2PageSize) << t.log2PageSize
}

// func (t *IdealAddressTranslator) findTranslationByReqID(id uint64) *transaction {
// 	for _, t := range t.transactions {
// 		if t.translationReq.ID == id {
// 			return t
// 		}
// 	}
// 	return nil
// }

// func (t *IdealAddressTranslator) removeExistingTranslation(trans *transaction) {
// 	for i, tr := range t.transactions {
// 		if tr == trans {
// 			t.transactions = append(t.transactions[:i], t.transactions[i+1:]...)
// 			return
// 		}
// 	}
// 	panic("translation not found")
// }

func (t *IdealAddressTranslator) isReqInBottomByID(id uint64) bool {
	for _, r := range t.inflightReqToBottom {
		if r.reqToBottom.Meta().ID == id {
			return true
		}
	}
	return false
}

func (t *IdealAddressTranslator) findReqToBottomByID(id uint64) reqToBottom {
	for _, r := range t.inflightReqToBottom {
		if r.reqToBottom.Meta().ID == id {
			return r
		}
	}
	panic("req to bottom not found")
}

func (t *IdealAddressTranslator) removeReqToBottomByID(id uint64) {
	for i, r := range t.inflightReqToBottom {
		if r.reqToBottom.Meta().ID == id {
			t.inflightReqToBottom = append(
				t.inflightReqToBottom[:i],
				t.inflightReqToBottom[i+1:]...)
			return
		}
	}
	panic("req to bottom not found")
}

func (t *IdealAddressTranslator) handleCtrlRequest(now akita.VTimeInSec) bool {
	req := t.CtrlPort.Peek()
	if req == nil {
		return false
	}

	msg := req.(*protocol.ControlMsg)

	if msg.DiscardTransations {
		return t.handleFlushReq(now, msg)
	} else if msg.Restart {
		return t.handleRestartReq(now, msg)
	}

	panic("never")
}

func (t *IdealAddressTranslator) handleFlushReq(
	now akita.VTimeInSec,
	req *protocol.ControlMsg,
) bool {
	rsp := protocol.ControlMsgBuilder{}.
		WithSrc(t.CtrlPort).
		WithDst(req.Src).
		WithSendTime(now).
		ToNotifyDone().
		Build()

	err := t.CtrlPort.Send(rsp)
	if err != nil {
		return false
	}

	t.CtrlPort.Retrieve(now)

	// t.transactions = nil
	t.inflightReqToBottom = nil
	t.isFlushing = true

	return true
}

func (t *IdealAddressTranslator) handleRestartReq(
	now akita.VTimeInSec,
	req *protocol.ControlMsg,
) bool {
	rsp := protocol.ControlMsgBuilder{}.
		WithSrc(t.CtrlPort).
		WithDst(req.Src).
		WithSendTime(now).
		ToNotifyDone().
		Build()

	err := t.CtrlPort.Send(rsp)

	if err != nil {
		return false
	}

	for t.TopPort.Retrieve(now) != nil {
	}

	for t.BottomPort.Retrieve(now) != nil {
	}

	// for t.TranslationPort.Retrieve(now) != nil {
	// }

	t.isFlushing = false

	t.CtrlPort.Retrieve(now)

	return true
}
