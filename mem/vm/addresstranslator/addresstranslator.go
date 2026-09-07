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

type transaction struct {
	incomingReqs    []protocol.AccessReq
	translationReq  *device.TranslationReq
	translationRsp  *device.TranslationRsp
	translationDone bool

	arriveTime   akita.VTimeInSec // req.SendTime (ROB forwarded)
	pickTime     akita.VTimeInSec // translate() picked it from TopPort
	xlatDoneTime akita.VTimeInSec // parseTranslation() got the rsp
}

type reqToBottom struct {
	reqFromTop  protocol.AccessReq
	reqToBottom protocol.AccessReq

	arriveTime   akita.VTimeInSec
	pickTime     akita.VTimeInSec
	xlatDoneTime akita.VTimeInSec
	dataSentTime akita.VTimeInSec // sendToBottom forwarded data req to L1V
}

// AddressTranslator is a element that is being simulated in Akita.
type AddressTranslator interface {
	tracing.NamedHookable
	akita.Component
	GetCtrlPort() akita.Port
	GetTopPort() akita.Port
	GetTranslationPort() akita.Port
	GetBottomPort() akita.Port
	SetLowModuleFinder(lmf.LowModuleFinder)
	SetTranslationProvider(interface{})
}

// DefaultAddressTranslator is a component that forwards the read/write requests with
// the address translated from virtual to physical.
type DefaultAddressTranslator struct {
	*akita.TickingComponent

	TopPort         akita.Port
	BottomPort      akita.Port
	TranslationPort akita.Port
	CtrlPort        akita.Port

	lowModuleFinder     lmf.LowModuleFinder
	translationProvider akita.Port
	log2PageSize        uint64
	deviceID            uint64
	numReqPerCycle      int

	isFlushing bool

	transactions        []*transaction
	inflightReqToBottom []reqToBottom

	// ServiceSum/ServiceCount: per-request time from arrival (req.SendTime,
	// set by the ROB when it forwarded) to the translated req being sent to
	// the L1V — i.e. the address-translation (TLB lookup / page walk) stage.
	ServiceSum   akita.VTimeInSec
	ServiceCount uint64

	// Fine breakdown of one req's residency inside the AT. The first three
	// sum to ServiceSum (the translation stage); DataRTSum is the data
	// round-trip below the AT (L1V$ + L2$ + network + DRAM) as the AT sees it.
	//   IngressQSum   = pick    - arrive    (wait in AT.TopPort queue)
	//   XlatRTSum     = xlatDone - pick      (L1TLB + L2TLB + MMU round-trip)
	//   EgressBlockSum= dataSent - xlatDone  (blocked on L1V$ accepting)
	//   DataRTSum     = dataDone - dataSent  (data fetch round-trip)
	IngressQSum    akita.VTimeInSec
	XlatRTSum      akita.VTimeInSec
	EgressBlockSum akita.VTimeInSec
	DataRTSum      akita.VTimeInSec
	DataRTCount    uint64
}

// DeadlockSummary reports per-port pending flags + in-flight req counts.
func (t *DefaultAddressTranslator) DeadlockSummary() (line string, anyPending bool) {
	topQ := t.TopPort != nil && t.TopPort.Peek() != nil
	bottomQ := t.BottomPort != nil && t.BottomPort.Peek() != nil
	xlatQ := t.TranslationPort != nil && t.TranslationPort.Peek() != nil
	ctrlQ := t.CtrlPort != nil && t.CtrlPort.Peek() != nil
	transN := len(t.transactions)
	inflN := len(t.inflightReqToBottom)
	anyPending = topQ || bottomQ || xlatQ || ctrlQ || transN > 0 || inflN > 0
	line = fmt.Sprintf(
		"top=%v bottom=%v xlat=%v ctrl=%v trans=%d inflight=%d flushing=%v",
		topQ, bottomQ, xlatQ, ctrlQ, transN, inflN, t.isFlushing)
	return
}

// SetTranslationProvider sets the remote port that can translate addresses.
func (t *DefaultAddressTranslator) SetTranslationProvider(p interface{}) {
	t.translationProvider = p.(akita.Port)
}

// SetLowModuleFinder sets the table recording where to find an address.
func (t *DefaultAddressTranslator) SetLowModuleFinder(lmf lmf.LowModuleFinder) {
	t.lowModuleFinder = lmf
}

// GetCtrlPort sets the table recording where to find an address.
func (t *DefaultAddressTranslator) GetCtrlPort() (port akita.Port) {
	port = t.CtrlPort
	return
}

// GetTopPort sets the table recording where to find an address.
func (t *DefaultAddressTranslator) GetTopPort() (port akita.Port) {
	port = t.TopPort
	return
}

// GetTranslationPort sets the table recording where to find an address.
func (t *DefaultAddressTranslator) GetTranslationPort() (port akita.Port) {
	port = t.TranslationPort
	return
}

// GetBottomPort sets the table recording where to find an address.
func (t *DefaultAddressTranslator) GetBottomPort() (port akita.Port) {
	port = t.BottomPort
	return
}

// Tick updates state at each cycle.
func (t *DefaultAddressTranslator) Tick(now akita.VTimeInSec) bool {
	madeProgress := false

	if !t.isFlushing {
		madeProgress = t.runPipeline(now)
	}

	madeProgress = t.handleCtrlRequest(now) || madeProgress

	return madeProgress
}

func (t *DefaultAddressTranslator) runPipeline(now akita.VTimeInSec) bool {
	madeProgress := false

	for i := 0; i < t.numReqPerCycle; i++ {
		madeProgress = t.respond(now) || madeProgress
	}

	for i := 0; i < t.numReqPerCycle; i++ {
		madeProgress = t.sendToBottom(now) || madeProgress
	}

	for i := 0; i < t.numReqPerCycle; i++ {
		madeProgress = t.parseTranslation(now) || madeProgress
	}

	for i := 0; i < t.numReqPerCycle; i++ {
		madeProgress = t.translate(now) || madeProgress
	}

	return madeProgress
}

// func getKind(t *DefaultAddressTranslator) (s string) {
// 	// fmt.Println(t.Name())
// 	s = "sent-" + strings.Split(strings.Split(t.Name(), ".")[3], "_")[0]
// 	// fmt.Println(s)
// 	return
// }

func (t *DefaultAddressTranslator) translate(now akita.VTimeInSec) bool {
	// if len(t.transactions) >= 64 {
	// return false
	// }
	item := t.TopPort.Peek()
	if item == nil {
		return false
	}

	req := item.(protocol.AccessReq)
	vAddr := req.GetAddress()
	vPageID := t.addrToPageID(vAddr)

	transReq := device.TranslationReqBuilder{}.
		WithSendTime(now).
		WithSrc(t.TranslationPort).
		WithDst(t.translationProvider).
		WithPID(req.GetPID()).
		WithVAddr(vPageID).
		WithInfo(req.GetAccessInfo()).
		WithDeviceID(t.deviceID).
		Build()
	err := t.TranslationPort.Send(transReq)
	if err != nil {
		return false
	}

	translation := &transaction{
		incomingReqs:   []protocol.AccessReq{req},
		translationReq: transReq,
		arriveTime:     req.Meta().SendTime,
		pickTime:       now,
	}
	t.transactions = append(t.transactions, translation)

	tracing.StartTask(
		tracing.AddrTransStatsTaskID(transReq.Meta().ID),
		tracing.MsgIDAtReceiver(req, t),
		now,
		t,
		"addr-translator-stats",
		reflect.TypeOf(transReq).String(),
		transReq,
	)
	tracing.StartTask(
		tracing.AddrTransStatsTaskID(req.Meta().ID),
		tracing.MsgIDAtReceiver(req, t),
		now,
		t,
		"addr-translator-stats",
		reflect.TypeOf(req).String(),
		req,
	)

	tracing.StartTracingNetworkReq(transReq, now, t, req)
	tracing.TraceReqReceive(req, now, t)
	tracing.TraceReqInitiate(transReq, now, t, tracing.MsgIDAtReceiver(req, t))

	t.TopPort.Retrieve(now)

	return true
}

func (t *DefaultAddressTranslator) parseTranslation(now akita.VTimeInSec) bool {
	rsp := t.TranslationPort.Peek()
	if rsp == nil {
		return false
	}

	transRsp := rsp.(*device.TranslationRsp)
	transaction := t.findTranslationByReqID(transRsp.RespondTo)
	if transaction == nil {
		t.TranslationPort.Retrieve(now)
		return true
	}

	transaction.translationRsp = transRsp
	transaction.translationDone = true
	transaction.xlatDoneTime = now

	t.TranslationPort.Retrieve(now)

	tracing.AddTaskStep(
		tracing.AddrTransStatsTaskID(transaction.translationReq.Meta().ID),
		now, t,
		"translation-latency",
	)
	tracing.EndTask(
		tracing.AddrTransStatsTaskID(transaction.translationReq.Meta().ID),
		now,
		t,
	)

	tracing.StopTracingNetworkReq(transRsp, now, t)
	tracing.TraceReqFinalize(transaction.translationReq, now, t)

	return true
}
func (t *DefaultAddressTranslator) sendToBottom(now akita.VTimeInSec) bool {
	for _, transaction := range t.transactions {
		if transaction.translationDone {
			reqFromTop := transaction.incomingReqs[0]
			translatedReq := t.createTranslatedReq(
				reqFromTop,
				transaction.translationRsp.Page)
			translatedReq.Meta().SendTime = now
			err := t.BottomPort.Send(translatedReq)
			if err != nil {
				return false
			}

			if a := transaction.arriveTime; a != 0 {
				t.ServiceSum += now - a
				t.ServiceCount++
				t.IngressQSum += transaction.pickTime - a
				t.XlatRTSum += transaction.xlatDoneTime - transaction.pickTime
				t.EgressBlockSum += now - transaction.xlatDoneTime
			}

			t.inflightReqToBottom = append(t.inflightReqToBottom,
				reqToBottom{
					reqFromTop:   reqFromTop,
					reqToBottom:  translatedReq,
					arriveTime:   transaction.arriveTime,
					pickTime:     transaction.pickTime,
					xlatDoneTime: transaction.xlatDoneTime,
					dataSentTime: now,
				})
			transaction.incomingReqs = transaction.incomingReqs[1:]
			if len(transaction.incomingReqs) == 0 {
				t.removeExistingTranslation(transaction)
			}
			tracing.TraceReqInitiate(translatedReq, now, t,
				tracing.MsgIDAtReceiver(reqFromTop, t))

			return true
		}
	}
	return false
}

func (t *DefaultAddressTranslator) respond(now akita.VTimeInSec) bool {
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

		if d := reqToBottomCombo.dataSentTime; d != 0 {
			t.DataRTSum += now - d
			t.DataRTCount++
		}

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

func (t *DefaultAddressTranslator) createTranslatedReq(
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

func (t *DefaultAddressTranslator) createTranslatedReadReq(
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

func (t *DefaultAddressTranslator) createTranslatedWriteReq(
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

func (t *DefaultAddressTranslator) addrToPageID(addr uint64) uint64 {
	// make address page aligned
	return (addr >> t.log2PageSize) << t.log2PageSize
}

func (t *DefaultAddressTranslator) findTranslationByReqID(id uint64) *transaction {
	for _, t := range t.transactions {
		if t.translationReq.ID == id {
			return t
		}
	}
	return nil
}

func (t *DefaultAddressTranslator) removeExistingTranslation(trans *transaction) {
	for i, tr := range t.transactions {
		if tr == trans {
			t.transactions = append(t.transactions[:i], t.transactions[i+1:]...)
			return
		}
	}
	panic("translation not found")
}

func (t *DefaultAddressTranslator) isReqInBottomByID(id uint64) bool {
	for _, r := range t.inflightReqToBottom {
		if r.reqToBottom.Meta().ID == id {
			return true
		}
	}
	return false
}

func (t *DefaultAddressTranslator) findReqToBottomByID(id uint64) reqToBottom {
	for _, r := range t.inflightReqToBottom {
		if r.reqToBottom.Meta().ID == id {
			return r
		}
	}
	panic("req to bottom not found")
}

func (t *DefaultAddressTranslator) removeReqToBottomByID(id uint64) {
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

func (t *DefaultAddressTranslator) handleCtrlRequest(now akita.VTimeInSec) bool {
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

func (t *DefaultAddressTranslator) handleFlushReq(
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

	t.transactions = nil
	t.inflightReqToBottom = nil
	t.isFlushing = true

	return true
}

func (t *DefaultAddressTranslator) handleRestartReq(
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

	for t.TranslationPort.Retrieve(now) != nil {
	}

	t.isFlushing = false

	t.CtrlPort.Retrieve(now)

	return true
}
