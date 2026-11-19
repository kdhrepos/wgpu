// Package rdma provides the implementation of an RDMA engine.
package rdma

import (
	"fmt"
	"log"
	"reflect"
	"strconv"
	"strings"

	"gitlab.com/akita/akita"
	"gitlab.com/akita/lmf"
	"gitlab.com/akita/mem/idealmemcontroller"
	"gitlab.com/akita/protocol"
	"gitlab.com/akita/util/global"
	"gitlab.com/akita/util/tracing"
)

type transaction struct {
	fromInside  akita.Msg
	fromOutside akita.Msg
	toInside    akita.Msg
	toOutside   akita.Msg
}

// canSender is satisfied by every port type whose inbox capacity we need to
// probe before forwarding a message into it. Both *akita.LimitNumMsgPort and
// *idealmemcontroller.LocalFirstPort implement CanSend(num int) bool, but
// akita.Port itself does not — so canRoute helpers assert this local
// interface instead of a concrete type.
type canSender interface {
	CanSend(num int) bool
}

// An Engine is a component that helps one GPU to access the memory on
// another GPU
type RDMAEngineImpl struct {
	*akita.TickingComponent

	// L2 ports need to be separated since ToL2 needs to be connected
	// both to L2ToDRAMConnection and L1ToL2Connection
	PwPort akita.Port
	// ToL2RDMA is an alias for ToL2RDMAs[0] kept for backward compatibility
	// with single-port wiring used by non-mesh platforms.
	ToL2RDMA akita.Port
	// ToL2RDMAs holds one outbound port per destination L2 cache bank. With
	// numL2Banks=1 (default) only ToL2RDMAs[0] (== ToL2RDMA) is wired, so
	// other platforms see no behavioral change.
	ToL2RDMAs []akita.Port
	// destToL2Idx maps a destination L2 RDMAPort to the index in ToL2RDMAs
	// that should be used as the src port. Empty for single-port wiring.
	destToL2Idx map[akita.Port]int
	// ToL2Bottom is an alias for ToL2Bottoms[0] kept for backward
	// compatibility with single-port wiring.
	ToL2Bottom akita.Port // Connected to L2ToDRAMConnection
	// ToL2Bottoms holds one port per destination L2 cache bank for the
	// L2 <-> RDMA bottom path (L2 evictions arrive here; RDMA writes
	// AccessRsp back to L2 from here). With numL2Banks=1 (default) only
	// ToL2Bottoms[0] (== ToL2Bottom) is wired.
	ToL2Bottoms []akita.Port
	// destToL2BottomIdx maps a destination L2 BottomToRdmaPort to the
	// ToL2Bottoms index that should be used as the src port.
	destToL2BottomIdx map[akita.Port]int
	// ToDramTop is an alias for ToDramTops[0] kept for backward compatibility.
	ToDramTop akita.Port
	// ToDramTops holds one outbound port per destination DRAM bank. With
	// numDramBanks=1 (default) only ToDramTops[0] (== ToDramTop) is wired.
	ToDramTops []akita.Port
	// destToDramIdx maps a destination DRAM TopPort to the ToDramTops index.
	destToDramIdx map[akita.Port]int

	RequestPort  akita.Port
	ResponsePort akita.Port
	CtrlPort     akita.Port

	isDraining              bool
	pauseIncomingReqsFromL1 bool
	currentDrainReq         *DrainReq

	localModuleFinder       lmf.LowModuleFinder       // RDMA port from each of L2 cache
	translationModuleFinder lmf.LowModuleFinder       // Top port from DRAM banks for remote address translation
	RdmaReqAddressTable     lmf.LowModuleFinder       // Request Ports from RDMA engine
	RdmaRspAddressTable     map[akita.Port]akita.Port // Request Ports -> Response Ports

	transactionsFromOutside []transaction
	transactionsFromInside  []transaction

	swReqPort akita.Port
	swRspPort akita.Port

	numTransPerCycle int

	ChipletId  int
	y, x       int
	yDim, xDim int

	// deadlock recovery
	retryPort akita.Port

	// deadlock prevention
	outstandingReqs int

	// flightFirstSeen records, per source msg.ID, the first time the
	// engine peeked that msg on its inbound port. RDMAFlightTracer's
	// StartTask is fired with this timestamp (not the eventual
	// Send-success time) so the outbound-port backpressure stall is
	// included in the measured flight. Entries are cleared on the
	// successful Send path.
	flightFirstSeen map[uint64]akita.VTimeInSec
}

func (e *RDMAEngineImpl) SetSwReqPort(port akita.Port) {
	e.swReqPort = port
}

func (e *RDMAEngineImpl) SetSwRspPort(port akita.Port) {
	e.swRspPort = port
}

// Tick checks if make progress
func (e *RDMAEngineImpl) Tick(now akita.VTimeInSec) bool {
	madeProgress := false

	madeProgress = e.processFromCtrlPort(now) || madeProgress
	if e.isDraining {
		madeProgress = e.drainRDMA(now) || madeProgress
	}
	madeProgress = e.processRspFromOutside(now) || madeProgress
	madeProgress = e.processRspFromDram(now) || madeProgress
	madeProgress = e.processRspFromL2(now) || madeProgress
	madeProgress = e.processReqFromOutside(now) || madeProgress
	madeProgress = e.processReqFromPwPort(now) || madeProgress
	madeProgress = e.processReqFromL2(now) || madeProgress
	madeProgress = e.processRetry(now) || madeProgress

	return madeProgress
}

func (e *RDMAEngineImpl) processFromCtrlPort(
	now akita.VTimeInSec,
) bool {
	req := e.CtrlPort.Peek()
	if req == nil {
		return false
	}

	switch req := req.(type) {
	case *DrainReq:
		e.currentDrainReq = req
		e.isDraining = true
		e.pauseIncomingReqsFromL1 = true
		e.CtrlPort.Retrieve(now)
		return true
	case *RestartReq:
		return e.processRDMARestartReq(now)
	default:
		log.Panicf("cannot process request of type %s", reflect.TypeOf(req))
		return false
	}
}

func (e *RDMAEngineImpl) processRDMARestartReq(
	now akita.VTimeInSec,
) bool {
	restartCompleteRsp := RestartRspBuilder{}.
		WithSendTime(now).
		WithSrc(e.CtrlPort).
		WithDst(e.currentDrainReq.Src).
		Build()
	err := e.CtrlPort.Send(restartCompleteRsp)

	if err != nil {
		return false
	}
	e.currentDrainReq = nil
	e.pauseIncomingReqsFromL1 = false

	e.CtrlPort.Retrieve(now)

	return true
}

func (e *RDMAEngineImpl) drainRDMA(
	now akita.VTimeInSec,
) bool {
	if e.fullyDrained() {
		drainCompleteRsp := DrainRspBuilder{}.
			WithSendTime(now).
			WithSrc(e.CtrlPort).
			WithDst(e.currentDrainReq.Src).
			Build()

		err := e.CtrlPort.Send(drainCompleteRsp)
		if err != nil {
			return false
		}
		e.isDraining = false
		return true
	}
	return false
}

func (e *RDMAEngineImpl) fullyDrained() bool {
	return len(e.transactionsFromOutside) == 0 &&
		len(e.transactionsFromInside) == 0
}

// DeadlockSummary reports inside/outside transaction counts, every port queue
// flag (including all per-bank slices), drain state, deadlock-prevention
// counters, retry path, and flight tracking. anyPending is true iff any
// bucket is non-empty.
func (e *RDMAEngineImpl) DeadlockSummary() (line string, anyPending bool) {
	insideN := len(e.transactionsFromInside)
	outsideN := len(e.transactionsFromOutside)
	pwQ := e.PwPort != nil && e.PwPort.Peek() != nil
	reqQ := e.RequestPort != nil && e.RequestPort.Peek() != nil
	rspQ := e.ResponsePort != nil && e.ResponsePort.Peek() != nil
	ctrlQ := e.CtrlPort != nil && e.CtrlPort.Peek() != nil
	retryQ := e.retryPort != nil && e.retryPort.Peek() != nil

	// Per-bank port slices: legacy aliases (ToL2RDMA / ToL2Bottom / ToDramTop)
	// are slot 0 of the slices, so iterating slices covers everything.
	l2rdmaQ := 0
	for _, p := range e.ToL2RDMAs {
		if p != nil && p.Peek() != nil {
			l2rdmaQ++
		}
	}
	l2botQ := 0
	for _, p := range e.ToL2Bottoms {
		if p != nil && p.Peek() != nil {
			l2botQ++
		}
	}
	dramTopQ := 0
	for _, p := range e.ToDramTops {
		if p != nil && p.Peek() != nil {
			dramTopQ++
		}
	}

	flightN := len(e.flightFirstSeen)
	anyPending = insideN > 0 || outsideN > 0 ||
		pwQ || reqQ || rspQ || ctrlQ || retryQ ||
		l2rdmaQ > 0 || l2botQ > 0 || dramTopQ > 0 ||
		e.outstandingReqs > 0 || e.isDraining ||
		e.pauseIncomingReqsFromL1 || flightN > 0
	line = fmt.Sprintf(
		"inside=%d outside=%d req=%v rsp=%v pw=%v ctrl=%v retry=%v "+
			"l2rdma=%d/%d l2bot=%d/%d dramTop=%d/%d "+
			"outstanding=%d draining=%v pauseFromL1=%v flightInFlight=%d",
		insideN, outsideN, reqQ, rspQ, pwQ, ctrlQ, retryQ,
		l2rdmaQ, len(e.ToL2RDMAs), l2botQ, len(e.ToL2Bottoms),
		dramTopQ, len(e.ToDramTops),
		e.outstandingReqs, e.isDraining, e.pauseIncomingReqsFromL1, flightN)
	if anyPending {
		line += " || " + e.meshDeadlockDiag()
	}
	return
}

// meshDeadlockDiag probes, for every RDMA port, whether a stuck inbound queue's
// egress can ACTUALLY accept a message right now. If an inbound queue is
// non-empty while its egress HAS ROOM, the engine stopped ticking and was never
// rewoken == lost wakeup; if the egress is full it is legitimate backpressure.
// Pairings: ToL2RDMA inbound (home-serve replies) -> ResponsePort/swRsp egress;
// ToL2Bottom inbound (L2 fetches) -> RequestPort/swReq egress; RequestPort
// inbound (incoming reqs) -> ToL2RDMA/ToDramTop egress; ResponsePort inbound
// (incoming rsps) -> ToL2Bottom egress.
func (e *RDMAEngineImpl) meshDeadlockDiag() string {
	in := func(p akita.Port) bool { return p != nil && p.Peek() != nil }
	out := func(p akita.Port) string {
		lp, ok := p.(*akita.LimitNumMsgPort)
		if !ok {
			return "n/a"
		}
		u, c, has := lp.OutBufLen()
		if !has {
			return "noconn"
		}
		room := ""
		if u < c {
			room = "+ROOM"
		}
		return fmt.Sprintf("%d/%d%s", u, c, room)
	}
	cansend := func(p akita.Port) string {
		lp, ok := p.(*akita.LimitNumMsgPort)
		if !ok {
			return "n/a"
		}
		return fmt.Sprintf("%v", lp.CanSend(1))
	}
	nocRoom := func(p akita.Port) bool {
		lp, ok := p.(*akita.LimitNumMsgPort)
		return ok && lp.CanSend(1)
	}

	diag := fmt.Sprintf(
		"PORTS Req{in=%v out=%s} Rsp{in=%v out=%s} "+
			"swReq.CanSend=%s swRsp.CanSend=%s "+
			"ToL2RDMA{in=%v out=%s} ToL2Bot{in=%v out=%s} ToDram{in=%v out=%s}",
		in(e.RequestPort), out(e.RequestPort),
		in(e.ResponsePort), out(e.ResponsePort),
		cansend(e.swReqPort), cansend(e.swRspPort),
		in(e.ToL2RDMA), out(e.ToL2RDMA),
		in(e.ToL2Bottom), out(e.ToL2Bottom),
		in(e.ToDramTop), out(e.ToDramTop))

	verdict := func(name string, stuck bool, egressRoom bool, egressDesc string) string {
		if !stuck {
			return ""
		}
		if egressRoom {
			return fmt.Sprintf(" [LOST-WAKEUP %s: stuck but %s has room]",
				name, egressDesc)
		}
		return fmt.Sprintf(" [backpressure %s: %s full]", name, egressDesc)
	}
	v := ""
	v += verdict("reply->swRsp", in(e.ToL2RDMA), nocRoom(e.swRspPort), "swRsp")
	v += verdict("fetch->swReq", in(e.ToL2Bottom), nocRoom(e.swReqPort), "swReq")
	v += verdict("inReq->L2", in(e.RequestPort),
		nocRoom(e.ToL2RDMA) || nocRoom(e.ToDramTop), "ToL2RDMA/ToDram")
	v += verdict("inRsp->L2bot", in(e.ResponsePort), nocRoom(e.ToL2Bottom),
		"ToL2Bottom")
	return diag + v
}

func (e *RDMAEngineImpl) processRetry(
	now akita.VTimeInSec,
) bool {
	madeProgress := false

	for i := 0; i < e.numTransPerCycle; i++ {
		msg := e.retryPort.Peek()
		if msg == nil {
			return madeProgress
		}

		srcName := msg.Meta().Src.Name()
		var srcPort akita.Port
		if strings.Contains(srcName, "Request") {
			srcPort = e.RequestPort
		} else {
			srcPort = e.ResponsePort
		}

		msg.Meta().SendTime = now

		networkMsg := &protocol.NetworkMsg{
			Msg:     msg,
			SrcPort: srcPort,
		}

		err := srcPort.Send(networkMsg)
		if err == nil {
			madeProgress = true

			if global.GetInstance().GetVerbose() {
				fmt.Println("Retried, ",
					e.Name(),
					msg.Meta().Src.Name(),
					msg.Meta().Dst.Name(),
					msg.Meta().ID,
					now,
				)
			}

			srcName := e.Name()
			dstName := msg.Meta().Dst.Name()
			srcChip := strings.Split(srcName, ".")[1]
			dstChip := strings.Split(dstName, ".")[1]

			switch msg := msg.(type) {
			case *protocol.TransReadReq:
				tracing.StartTask(
					msg.Meta().ID,
					0,
					now,
					e,
					"remote-ptw-trace",
					"retry",
					srcChip+"."+dstChip)
			case protocol.AccessReq:
				tracing.StartTask(
					msg.Meta().ID,
					0,
					now,
					e,
					"hop-trace",
					"retry",
					srcChip+"."+dstChip)
				// flight tracing moved to switch boundary
				// (multiflitchipletswitch.recv); see
				// traceFlightStart/traceFlightEnd there.
				// tracing.StartTask(
				// msg.Meta().ID,
				// "",
				// now,
				// e,
				// "pte-caching-remote-ptw-trace",
				// "",
				// srcChip+"."+dstChip)
			}

			e.retryPort.Retrieve(now)
		} else {
			if global.GetInstance().GetVerbose() {
				fmt.Println("Error on retry, ",
					e.Name(),
					msg.Meta().Src.Name(),
					msg.Meta().Dst.Name(),
					msg.Meta().ID,
					now,
				)
			}

		}
	}

	return madeProgress
}

func (e *RDMAEngineImpl) processReqFromPwPort(
	now akita.VTimeInSec,
) bool {
	if e.pauseIncomingReqsFromL1 {
		return false
	}

	madeProgress := false
	for i := 0; i < e.numTransPerCycle; i++ {
		trans := e.PwPort.Peek()
		if trans == nil {
			return madeProgress
		}

		switch trans := trans.(type) {
		case *protocol.ReadReq:
			madeProgress = e.processReqFromPwPortAux(now, trans) || madeProgress
		default:
			log.Panicf("cannot process request of type %s", reflect.TypeOf(trans))
			return false
		}
	}
	return madeProgress
}

// process remote translation request from MMU
// this request directly goes to remote chiplet's local DRAM, not MMU
func (e *RDMAEngineImpl) processReqFromPwPortAux(
	now akita.VTimeInSec,
	req *protocol.ReadReq,
) bool {
	e.noteFlightFirstSeen(req.Meta().ID, now)

	if e.outstandingReqs >= maxOutstandingReqs {
		if global.GetInstance().GetVerbose() {
			fmt.Println("Too many outstanding reqs", e.Name())
		}
		return false
	}
	// if !e.swReqPort.(*akita.LimitNumMsgPort).CanSend(e.numTransPerCycle) {
	// 	return false
	// }

	dst := e.RdmaReqAddressTable.Find(req.GetAddress())

	cloned := protocol.TransReadReqBuilder{}.
		WithSendTime(now).
		WithSrc(e.RequestPort).
		WithDst(dst).
		WithPID(req.PID).
		WithAddress(req.GetAddress()).
		WithByteSize(8).
		WithInfo(req.Info).
		Build()

	networkMsg := &protocol.NetworkMsg{
		Msg:     cloned,
		SrcPort: e.RequestPort,
	}

	err := e.RequestPort.Send(networkMsg)
	if err == nil {
		e.PwPort.Retrieve(now)

		tracing.TraceReqReceive(req, now, e)
		tracing.TraceReqInitiate(cloned, now, e,
			tracing.MsgIDAtReceiver(req, e))

		emitRdmaFlightStartAt(cloned,
			e.consumeFlightFirstSeen(req.Meta().ID, now), e)

		srcName := req.Meta().Src.Name()
		dstName := dst.Name()
		srcChip := strings.Split(srcName, ".")[1]
		dstChip := strings.Split(dstName, ".")[1]

		// tracing.AddTaskStep(tracing.MsgIDAtReceiver(req, e), now, e,
		// srcChip+":PageAccessToChiplet:"+dstChip)

		tracing.StartTask(
			cloned.Meta().ID,
			0,
			now,
			e,
			"remote-ptw-trace",
			"",
			srcChip+"."+dstChip)

		trans := transaction{
			fromInside: req,
			toOutside:  cloned,
		}
		e.transactionsFromInside = append(e.transactionsFromInside, trans)

		e.outstandingReqs++

		return true
	}
	if global.GetInstance().GetVerbose() {
		fmt.Println("Error on processReqFromPwPort, ",
			e.Name(),
			cloned.Meta().Src.Name(),
			cloned.Meta().Dst.Name(),
			req.Meta().ID,
		)
	}

	return false
}

// process
func (e *RDMAEngineImpl) processTransRspFromDram(
	now akita.VTimeInSec,
	rsp *protocol.DataReadyRsp,
	recvPort akita.Port,
) bool {
	// if !e.swRspPort.(*akita.LimitNumMsgPort).CanSend(e.numTransPerCycle) {
	// 	return false
	// }

	e.noteFlightFirstSeen(rsp.Meta().ID, now)

	// before current response, original request must exist before.
	transactionIndex := e.findTransactionByRspToID(
		rsp.GetRespondTo(), e.transactionsFromOutside)

	// this is the original request for the current cloned translation response
	trans := e.transactionsFromOutside[transactionIndex]

	// destination can be gotten from the source of original request.
	dst := e.RdmaRspAddressTable[trans.fromOutside.Meta().Src]

	cloned := protocol.TransDataReadyRspBuilder{}.
		WithSendTime(now).
		WithSrc(rsp.Src).
		WithDst(rsp.Dst).
		WithRspTo(trans.fromOutside.Meta().ID).
		WithData(rsp.Data).
		WithInfo(rsp.Info).
		Build()

	cloned.Meta().Src = e.ResponsePort
	cloned.Meta().Dst = dst
	cloned.Meta().SendTime = now

	networkMsg := &protocol.NetworkMsg{
		Msg:     cloned,
		SrcPort: e.ResponsePort,
	}

	err := e.ResponsePort.Send(networkMsg)
	if err == nil {
		recvPort.Retrieve(now)

		// trans.toInside, which is "cloned" is finally done.
		tracing.TraceReqFinalize(trans.toInside, now, e)

		// trans.fromOutside, which is "req" or original request is almost done (completed).
		tracing.TraceReqComplete(trans.fromOutside, now, e)

		emitRdmaFlightStartAt(cloned,
			e.consumeFlightFirstSeen(rsp.Meta().ID, now), e)

		// transaction from the outside was correctly handled. Thus, pop the old transaction
		e.transactionsFromOutside =
			append(e.transactionsFromOutside[:transactionIndex],
				e.transactionsFromOutside[transactionIndex+1:]...)

		return true
	}
	if global.GetInstance().GetVerbose() {
		fmt.Println("Error on processRspFromPwPort, ",
			e.Name(),
			cloned.Meta().Src.Name(),
			cloned.Meta().Dst.Name(),
			rsp.Meta().ID,
		)
	}

	return false
}

func (e *RDMAEngineImpl) processReqFromL2(
	now akita.VTimeInSec,
) bool {
	if e.pauseIncomingReqsFromL1 {
		return false
	}

	madeProgress := false
	port := e.ToL2Bottom
	for i := 0; i < e.numTransPerCycle; i++ {
		req := port.Peek()
		if req == nil {
			continue
		}

		switch msg := req.(type) {
		case protocol.AccessReq:
			if e.processReqFromL2Aux(now, msg, port) {
				madeProgress = true
			}
		default:
			panic("unknown req type")
		}
	}
	return madeProgress
}

func (e *RDMAEngineImpl) processRspFromL2(
	now akita.VTimeInSec,
) bool {
	// if e.pauseIncomingReqsFromL1 {
	// 	return false
	// }

	madeProgress := false
	port := e.ToL2RDMA
	for i := 0; i < e.numTransPerCycle; i++ {
		req := port.Peek()
		if req == nil {
			continue
		}

		switch msg := req.(type) {
		case protocol.AccessRsp:
			if e.processRspFromL2Aux(now, msg, port, 0) {
				madeProgress = true
			}
		default:
			panic("unknown req type")
		}
	}
	return madeProgress
}

func (e *RDMAEngineImpl) processReqFromL2Aux(
	now akita.VTimeInSec,
	req protocol.AccessReq,
	recvPort akita.Port,
) bool {
	e.noteFlightFirstSeen(req.Meta().ID, now)

	if e.outstandingReqs >= maxOutstandingReqs {
		if global.GetInstance().GetVerbose() {
			fmt.Println("Too many outstanding reqs", e.Name())
		}
		return false
	}

	dst := e.RdmaReqAddressTable.Find(req.GetAddress())

	cloned := e.cloneReq(req)
	cloned.Meta().Src = e.RequestPort
	cloned.Meta().Dst = dst
	cloned.Meta().SendTime = now

	networkMsg := &protocol.NetworkMsg{
		Msg:     cloned,
		SrcPort: e.RequestPort,
	}

	err := e.RequestPort.Send(networkMsg)
	if err == nil {
		recvPort.(*akita.LimitNumMsgPort).Retrieve(now)

		emitRdmaFlightStartAt(cloned,
			e.consumeFlightFirstSeen(req.Meta().ID, now), e)

		tracing.TraceReqReceive(req, now, e)
		tracing.TraceReqInitiate(cloned, now, e,
			tracing.MsgIDAtReceiver(req, e))

		trans := transaction{
			fromInside: req,
			toOutside:  cloned,
		}
		e.transactionsFromInside = append(e.transactionsFromInside, trans)

		if global.GetInstance().GetVerbose() {
			fmt.Println("Send, ",
				e.Name(),
				cloned.Meta().Src.Name(),
				cloned.Meta().Dst.Name(),
				cloned.Meta().ID,
				now,
			)
		}

		e.outstandingReqs++

		srcName := e.Name()
		dstName := dst.Name()
		srcChip := strings.Split(srcName, ".")[1]
		dstChip := strings.Split(dstName, ".")[1]

		pc, _ := req.GetAccessInfo().(uint64)
		tracing.StartTask(
			cloned.Meta().ID,
			0,
			now,
			e,
			"hop-trace",
			strconv.FormatUint(pc, 10),
			srcChip+"."+dstChip)

		return true
	}

	if global.GetInstance().GetVerbose() {
		fmt.Println("Error on processReqFromL2Aux, ",
			e.Name(),
			cloned.Meta().Src.Name(),
			cloned.Meta().Dst.Name(),
			req.Meta().ID,
		)
	}

	return false
}

func (e *RDMAEngineImpl) processRspFromL2Aux(
	now akita.VTimeInSec,
	rsp protocol.AccessRsp,
	recvPort akita.Port,
	idx int,
) bool {
	// if !e.swRspPort.(*akita.LimitNumMsgPort).CanSend(e.numTransPerCycle) {
	// 	return false
	// }

	e.noteFlightFirstSeen(rsp.Meta().ID, now)

	transactionIndex := e.findTransactionByRspToID(
		rsp.GetRespondTo(), e.transactionsFromOutside)
	trans := e.transactionsFromOutside[transactionIndex]

	rspToOutside := e.cloneRsp(rsp, trans.fromOutside.Meta().ID)
	rspToOutside.Meta().SendTime = now
	rspToOutside.Meta().Src = e.ResponsePort
	rspToOutside.Meta().Dst = e.RdmaRspAddressTable[trans.fromOutside.Meta().Src]

	networkMsg := &protocol.NetworkMsg{
		Msg:     rspToOutside,
		SrcPort: e.ResponsePort,
	}

	err := e.ResponsePort.Send(networkMsg)
	if err == nil {
		recvPort.(*akita.LimitNumMsgPort).RetrieveAt(now, idx)

		tracing.TraceReqFinalize(trans.toInside, now, e)
		tracing.TraceReqComplete(trans.fromOutside, now, e)

		emitRdmaFlightStartAt(rspToOutside,
			e.consumeFlightFirstSeen(rsp.Meta().ID, now), e)

		e.transactionsFromOutside =
			append(e.transactionsFromOutside[:transactionIndex],
				e.transactionsFromOutside[transactionIndex+1:]...)

		return true
	}

	if global.GetInstance().GetVerbose() {
		fmt.Println("Error on processRspFromL2Aux, ",
			e.Name(),
			rspToOutside.Meta().Src.Name(),
			rspToOutside.Meta().Dst.Name(),
			rsp.Meta().ID,
		)
	}

	return false
}

func (e *RDMAEngineImpl) processRspFromDram(
	now akita.VTimeInSec,
) bool {
	// if e.pauseIncomingReqsFromL1 {
	// 	return false
	// }

	madeProgress := false
	port := e.ToDramTop
	for i := 0; i < e.numTransPerCycle; i++ {
		rsp := port.Peek()
		if rsp == nil {
			continue
		}

		switch rsp := rsp.(type) {
		case *protocol.DataReadyRsp: // for the translation response
			if e.processTransRspFromDram(now, rsp, port) {
				madeProgress = true
			}
		case *protocol.WriteDoneRsp:
			if e.processWriteDoneRspFromDram(now, rsp, port) {
				madeProgress = true
			}
		default:
			panic("unknown rsp type")
		}
	}
	return madeProgress
}

func (e *RDMAEngineImpl) processWriteDoneRspFromDram(
	now akita.VTimeInSec,
	rsp *protocol.WriteDoneRsp,
	recvPort akita.Port,
) bool {
	// if !e.swRspPort.(*akita.LimitNumMsgPort).CanSend(e.numTransPerCycle) {
	// 	return false
	// }

	e.noteFlightFirstSeen(rsp.Meta().ID, now)

	transactionIndex := e.findTransactionByRspToID(
		rsp.GetRespondTo(), e.transactionsFromOutside)
	trans := e.transactionsFromOutside[transactionIndex]

	rspToOutside := e.cloneRsp(rsp, trans.fromOutside.Meta().ID)
	rspToOutside.Meta().SendTime = now
	rspToOutside.Meta().Src = e.ResponsePort
	rspToOutside.Meta().Dst = e.RdmaRspAddressTable[trans.fromOutside.Meta().Src]

	networkMsg := &protocol.NetworkMsg{
		Msg:     rspToOutside,
		SrcPort: e.ResponsePort,
	}

	err := e.ResponsePort.Send(networkMsg)
	if err == nil {
		recvPort.Retrieve(now)

		tracing.TraceReqFinalize(trans.toInside, now, e)
		tracing.TraceReqComplete(trans.fromOutside, now, e)

		emitRdmaFlightStartAt(rspToOutside,
			e.consumeFlightFirstSeen(rsp.Meta().ID, now), e)

		e.transactionsFromOutside =
			append(e.transactionsFromOutside[:transactionIndex],
				e.transactionsFromOutside[transactionIndex+1:]...)

		return true
	}
	if global.GetInstance().GetVerbose() {
		fmt.Println("Error on processWriteDoneRspFromDram, ",
			e.Name(),
			rspToOutside.Meta().Src.Name(),
			rspToOutside.Meta().Dst.Name(),
			rsp.Meta().ID,
		)
	}

	return false
}

func (e *RDMAEngineImpl) processReqFromOutside(
	now akita.VTimeInSec,
) bool {
	madeProgress := false
	port := e.RequestPort.(*akita.LimitNumMsgPort)
	for i := 0; i < e.numTransPerCycle; i++ {
		req, idx, ok := port.PeekReady(e.canRouteReqFromOutside)
		if !ok {
			return madeProgress
		}

		switch req := req.(type) {
		case *protocol.TransReadReq:
			madeProgress = e.processTransReqFromOutside(now, req, idx) || madeProgress
		case *protocol.WriteReq:
			madeProgress = e.processEvictReqFromOutside(now, req, idx) || madeProgress
		case protocol.AccessReq:
			madeProgress = e.processDataReqFromOutside(now, req, idx) || madeProgress
		default:
			log.Panicf("cannot process request of type %s", reflect.TypeOf(req))
			return false
		}
	}
	return madeProgress
}

// canRouteReqFromOutside reports whether the outbound src port that would
// carry this request currently has buffer room. Used by RequestPort
// PeekReady to skip head-of-line messages whose destination is backpressured.
func (e *RDMAEngineImpl) canRouteReqFromOutside(m akita.Msg) bool {
	switch req := m.(type) {
	case *protocol.TransReadReq:
		dst := e.translationModuleFinder.Find(req.GetAddress())
		return e.dstReadyVia(e.srcPortForDramDst(dst), dst)
	case *protocol.WriteReq:
		dst := e.translationModuleFinder.Find(req.GetAddress())
		return e.dstReadyVia(e.srcPortForDramDst(dst), dst)
	case protocol.AccessReq:
		dst := e.localModuleFinder.Find(req.GetAddress())
		return e.dstReadyVia(e.srcPortForL2Dst(dst), dst)
	}
	return false
}

// dstReadyVia reports whether dst can accept one more message routed through
// src, reserving room for messages already queued in src's source buffer for
// dst. This stops the shared srcEnd from over-committing one dst and leaving
// undeliverable messages that head-of-line-block traffic to other dsts.
func (e *RDMAEngineImpl) dstReadyVia(src, dst akita.Port) bool {
	pending := 0
	if lp, ok := src.(*akita.LimitNumMsgPort); ok {
		if n, has := lp.OutBufDstCount(dst); has {
			pending = n
		}
	}
	return dst.(canSender).CanSend(1 + pending)
}

func (e *RDMAEngineImpl) processEvictReqFromOutside(
	now akita.VTimeInSec,
	req *protocol.WriteReq,
	idx int,
) bool {
	dst := e.translationModuleFinder.Find(req.GetAddress()) // ports from DRAM bank
	src := e.srcPortForDramDst(dst)

	cloned := e.cloneReq(req)
	cloned.Meta().Src = src
	cloned.Meta().Dst = dst
	cloned.Meta().SendTime = now

	toDramMsg := &idealmemcontroller.ToDramMsg{
		Msg:       cloned,
		FromLocal: false,
	}

	err := src.Send(toDramMsg)
	if err == nil {
		e.RequestPort.(*akita.LimitNumMsgPort).RetrieveAt(now, idx)
		emitRdmaFlightEnd(req, now, e)

		tracing.TraceReqReceive(req, now, e)
		tracing.TraceReqInitiate(cloned, now, e,
			tracing.MsgIDAtReceiver(req, e))

		trans := transaction{
			fromOutside: req,
			toInside:    cloned,
		}
		e.transactionsFromOutside =
			append(e.transactionsFromOutside, trans)

		return true
	}

	if global.GetInstance().GetVerbose() {
		fmt.Println("Error on processEvictReqFromOutside, ",
			e.Name(),
			cloned.Meta().Src.Name(),
			cloned.Meta().Dst.Name(),
			req.Meta().ID,
		)
	}

	// send transaction at head to tail to prevent head-of-line blocking.
	// e.SortReqPortBuffer()
	return false
}

func (e *RDMAEngineImpl) processTransReqFromOutside(
	now akita.VTimeInSec,
	req *protocol.TransReadReq,
	idx int,
) bool {
	// DEBUG: detect PTW requests for addresses below GPU memory base.
	// translationModuleFinder is a *lmf.StripedLowModuleFinder; check its offset.
	if f, ok := e.translationModuleFinder.(*lmf.StripedLowModuleFinder); ok {
		if f.MemAddrOffset != 0 && req.GetAddress() < f.MemAddrOffset {
			log.Printf("[debug] RDMA %s processTransReqFromOutside: addr=0x%x below offset=0x%x, src=%s, info=%+v",
				e.Name(), req.GetAddress(), f.MemAddrOffset,
				req.Meta().Src.Name(), req.Info)
		}
	}
	dst := e.translationModuleFinder.Find(req.GetAddress()) // port from DRAM bank
	src := e.srcPortForDramDst(dst)

	// Convert from TransReadReq to ReadReq
	cloned := protocol.ReadReqBuilder{}.
		WithSendTime(now).
		WithSrc(src).
		WithDst(dst).
		WithPID(req.PID).
		WithAddress(req.GetAddress()).
		WithByteSize(8).
		WithInfo(req.Info).
		Build()

	toDramMsg := &idealmemcontroller.ToDramMsg{
		Msg:       cloned,
		FromLocal: false,
	}

	err := src.Send(toDramMsg)
	if err == nil {
		e.RequestPort.(*akita.LimitNumMsgPort).RetrieveAt(now, idx)
		emitRdmaFlightEnd(req, now, e)

		tracing.TraceReqReceive(req, now, e)
		tracing.TraceReqInitiate(cloned, now, e,
			tracing.MsgIDAtReceiver(req, e))

		trans := transaction{
			fromOutside: req,
			toInside:    cloned,
		}
		e.transactionsFromOutside =
			append(e.transactionsFromOutside, trans)

		return true
	}

	if global.GetInstance().GetVerbose() {
		fmt.Println("Error on processTransReqFromOutside, ",
			e.Name(),
			cloned.Meta().Src.Name(),
			cloned.Meta().Dst.Name(),
			req.Meta().ID,
		)
	}

	// e.SortReqPortBuffer()
	return false
}

func (e *RDMAEngineImpl) processDataReqFromOutside(
	now akita.VTimeInSec,
	req protocol.AccessReq,
	idx int,
) bool {
	dst := e.localModuleFinder.Find(req.GetAddress()) // rdma port of l2 cache
	src := e.srcPortForL2Dst(dst)

	// if !dst.(*akita.LimitNumMsgPort).CanSend(e.numTransPerCycle) {
	// 	return false
	// }

	cloned := e.cloneReq(req)
	cloned.Meta().SendTime = now
	cloned.Meta().Src = src
	cloned.Meta().Dst = dst // rdma port of local L2 cache.

	err := src.Send(cloned)
	if err == nil {
		e.RequestPort.(*akita.LimitNumMsgPort).RetrieveAt(now, idx)
		emitRdmaFlightEnd(req, now, e)

		tracing.TraceReqReceive(req, now, e)
		tracing.TraceReqInitiate(cloned, now, e,
			tracing.MsgIDAtReceiver(req, e))

		trans := transaction{
			fromOutside: req,
			toInside:    cloned,
		}
		e.transactionsFromOutside = append(e.transactionsFromOutside, trans)

		return true
	}
	if global.GetInstance().GetVerbose() {
		fmt.Println("Error on processDataReqFromOutside, ",
			e.Name(),
			cloned.Meta().Src.Name(),
			cloned.Meta().Dst.Name(),
			req.Meta().ID,
		)
	}

	return false
}

func (e *RDMAEngineImpl) processRspFromOutside(now akita.VTimeInSec) bool {
	madeProgress := false
	port := e.ResponsePort.(*akita.LimitNumMsgPort)
	for i := 0; i < e.numTransPerCycle; i++ {
		req, idx, ok := port.PeekReady(e.canRouteRspFromOutside)
		if !ok {
			return madeProgress
		}

		switch req := req.(type) {
		case *protocol.TransDataReadyRsp:
			madeProgress = e.processTransRspFromOutside(now, req, idx) || madeProgress
		case protocol.AccessRsp:
			madeProgress = e.processAccessRspFromOutside(now, req, idx) || madeProgress
		default:
			log.Panicf("cannot process request of type %s", reflect.TypeOf(req))
			return false
		}
	}
	return madeProgress
}

// canRouteRspFromOutside reports whether the destination port that will
// forward this response into the chiplet (PwPort for translation responses,
// the per-bank ToL2Bottoms[i] resolved from the original requester for
// access responses) has buffer room.
func (e *RDMAEngineImpl) canRouteRspFromOutside(m akita.Msg) bool {
	switch rsp := m.(type) {
	case *protocol.TransDataReadyRsp:
		txIdx := e.findTransactionByRspToID(
			rsp.GetRespondTo(), e.transactionsFromInside)
		if txIdx < 0 {
			return false
		}
		dst := e.transactionsFromInside[txIdx].fromInside.Meta().Src
		return dst.(canSender).CanSend(1)
	case protocol.AccessRsp:
		txIdx := e.findTransactionByRspToID(
			rsp.GetRespondTo(), e.transactionsFromInside)
		if txIdx < 0 {
			return false
		}
		dst := e.transactionsFromInside[txIdx].fromInside.Meta().Src
		return dst.(canSender).CanSend(1)
	}
	return false
}

func (e *RDMAEngineImpl) processTransRspFromOutside(
	now akita.VTimeInSec,
	rsp *protocol.TransDataReadyRsp,
	idx int,
) bool {
	transactionIndex := e.findTransactionByRspToID(
		rsp.GetRespondTo(), e.transactionsFromInside)
	trans := e.transactionsFromInside[transactionIndex]

	src := e.PwPort
	dst := trans.fromInside.Meta().Src

	// Convert TransReadyRsp to DataReadyRsp
	transRspToInside := protocol.DataReadyRspBuilder{}.
		WithSendTime(rsp.SendTime).
		WithSrc(src).
		WithDst(dst).
		WithRspTo(trans.fromInside.Meta().ID).
		WithData(rsp.Data). // data is the physical address of the page
		WithInfo(rsp.Info).
		Build()

	transRspToInside.Meta().Src = src
	transRspToInside.Meta().Dst = dst
	transRspToInside.Meta().SendTime = now

	err := src.Send(transRspToInside)
	if err == nil {
		e.ResponsePort.(*akita.LimitNumMsgPort).RetrieveAt(now, idx)
		emitRdmaFlightEnd(rsp, now, e)

		tracing.TraceReqFinalize(trans.toOutside, now, e)
		tracing.TraceReqComplete(trans.fromInside, now, e)

		e.transactionsFromInside =
			append(e.transactionsFromInside[:transactionIndex],
				e.transactionsFromInside[transactionIndex+1:]...)

		tracing.EndTask(
			rsp.GetRespondTo(),
			now,
			rsp.Meta().Src.Component().(RDMAEngine))

		e.outstandingReqs--

		return true
	}
	if global.GetInstance().GetVerbose() {
		fmt.Println("Error on processTransRspFromOutside, ",
			e.Name(),
			transRspToInside.Meta().Src.Name(),
			transRspToInside.Meta().Dst.Name(),
			rsp.Meta().ID,
		)
	}

	// e.translationModuleFinder.(*cache.StripedLowModuleFinder).LowModules

	// e.SortRspPortBuffer()
	return false
}

func (e *RDMAEngineImpl) processAccessRspFromOutside(
	now akita.VTimeInSec,
	rsp protocol.AccessRsp,
	idx int,
) bool {
	transactionIndex := e.findTransactionByRspToID(
		rsp.GetRespondTo(), e.transactionsFromInside)

	// get the orignial request
	trans := e.transactionsFromInside[transactionIndex]

	// if !trans.fromInside.Meta().Src.(*akita.LimitNumMsgPort).CanSend(e.numTransPerCycle) {
	// 	return false
	// }

	src := e.srcPortForL2BottomDst(trans.fromInside.Meta().Src)
	rspToInside := e.cloneRsp(rsp, trans.fromInside.Meta().ID)
	rspToInside.Meta().SendTime = now
	rspToInside.Meta().Src = src
	rspToInside.Meta().Dst = trans.fromInside.Meta().Src

	err := src.Send(rspToInside)
	if err == nil {
		e.ResponsePort.(*akita.LimitNumMsgPort).RetrieveAt(now, idx)
		emitRdmaFlightEnd(rsp, now, e)

		tracing.TraceReqFinalize(trans.toOutside, now, e)
		tracing.TraceReqComplete(trans.fromInside, now, e)

		tracing.EndTask(
			rsp.GetRespondTo(),
			now,
			rsp.Meta().Src.Component().(RDMAEngine))

		e.transactionsFromInside =
			append(e.transactionsFromInside[:transactionIndex],
				e.transactionsFromInside[transactionIndex+1:]...)

		e.outstandingReqs--

		return true
	}
	if global.GetInstance().GetVerbose() {
		fmt.Println("Error on processAccessRspFromOutside, ",
			e.Name(),
			rspToInside.Meta().Src.Name(),
			rspToInside.Meta().Dst.Name(),
			rsp.Meta().ID,
		)
	}

	// e.SortRspPortBuffer()
	return false
}

func (e *RDMAEngineImpl) findTransactionByRspToID(
	rspTo uint64,
	transactions []transaction,
) int {
	for i, trans := range transactions {
		if trans.toOutside != nil && trans.toOutside.Meta().ID == rspTo {
			return i
		}

		if trans.toInside != nil && trans.toInside.Meta().ID == rspTo {
			return i
		}
	}

	log.Panicf("transaction %d not found", rspTo)
	return 0
}

func (e *RDMAEngineImpl) cloneReq(origin protocol.AccessReq) protocol.AccessReq {
	switch origin := origin.(type) {
	case *protocol.ReadReq:
		read := protocol.ReadReqBuilder{}.
			WithSendTime(origin.SendTime).
			WithSrc(origin.Src).
			WithDst(origin.Dst).
			WithPID(origin.GetPID()).
			WithAddress(origin.Address).
			WithByteSize(origin.AccessByteSize).
			WithInfo(origin.Info).
			Build()
		read.IsPtw = origin.IsPtw
		return read
	case *protocol.WriteReq:
		write := protocol.WriteReqBuilder{}.
			WithSendTime(origin.SendTime).
			WithSrc(origin.Src).
			WithDst(origin.Dst).
			WithPID(origin.GetPID()).
			WithAddress(origin.Address).
			WithData(origin.Data).
			WithDirtyMask(origin.DirtyMask).
			Build()
		return write
	default:
		log.Panicf("cannot clone request of type %s",
			reflect.TypeOf(origin))
	}
	return nil
}

func (e *RDMAEngineImpl) cloneRsp(origin protocol.AccessRsp, rspTo uint64) protocol.AccessRsp {
	switch origin := origin.(type) {
	case *protocol.DataReadyRsp:
		rsp := protocol.DataReadyRspBuilder{}.
			WithSendTime(origin.SendTime).
			WithSrc(origin.Src).
			WithDst(origin.Dst).
			WithRspTo(rspTo).
			WithData(origin.Data).
			WithInfo(origin.Info).
			Build()
		return rsp
	case *protocol.WriteDoneRsp:
		rsp := protocol.WriteDoneRspBuilder{}.
			WithSendTime(origin.SendTime).
			WithSrc(origin.Src).
			WithDst(origin.Dst).
			WithRspTo(rspTo).
			Build()
		return rsp
	default:
		log.Panicf("cannot clone request of type %s",
			reflect.TypeOf(origin))
	}
	return nil
}

// SetFreq sets freq
func (e *RDMAEngineImpl) SetFreq(freq akita.Freq) {
	e.TickingComponent.Freq = freq
}

// NewEngine creates new engine
func NewRDMAEngine(
	name string,
	engine akita.Engine,
	numTransPerCycle int,
) RDMAEngine {
	e := new(RDMAEngineImpl)
	e.TickingComponent = akita.NewTickingComponent(
		name,
		engine,
		1*akita.GHz,
		e)

	e.ToL2RDMA = akita.NewLimitNumMsgPort(e, 16*numTransPerCycle, name+".ToL2RDMA")
	e.ToL2RDMAs = []akita.Port{e.ToL2RDMA}
	e.destToL2Idx = map[akita.Port]int{}
	e.ToL2Bottom = akita.NewLimitNumMsgPort(e, 16*numTransPerCycle, name+".ToL2Bottom")
	e.ToL2Bottoms = []akita.Port{e.ToL2Bottom}
	e.destToL2BottomIdx = map[akita.Port]int{}
	e.ToDramTop = akita.NewLimitNumMsgPort(e, 16*numTransPerCycle, name+".ToDramTop")
	e.ToDramTops = []akita.Port{e.ToDramTop}
	e.destToDramIdx = map[akita.Port]int{}
	e.CtrlPort = akita.NewLimitNumMsgPort(e, numTransPerCycle, name+".CtrlPort")
	e.PwPort = akita.NewLimitNumMsgPort(e, 16*numTransPerCycle, name+".PwPort")

	e.RequestPort = akita.NewLimitNumMsgPort(e, 16*numTransPerCycle, name+".RequestPort")
	e.ResponsePort = akita.NewLimitNumMsgPort(e, 16*numTransPerCycle, name+".ResponsePort")

	e.numTransPerCycle = numTransPerCycle

	e.retryPort = akita.NewLimitNumMsgPort(e, 100000000, name+".RetryPort")

	return e
}

// AddL2BankPorts grows ToL2RDMAs to numBanks by appending additional outbound
// ports (the first slot is the original ToL2RDMA). Idempotent for numBanks=1.
func (e *RDMAEngineImpl) AddL2BankPorts(numBanks int) {
	if numBanks <= len(e.ToL2RDMAs) {
		return
	}
	bufCap := 4 * e.numTransPerCycle
	for i := len(e.ToL2RDMAs); i < numBanks; i++ {
		p := akita.NewLimitNumMsgPort(e, bufCap,
			fmt.Sprintf("%s.ToL2RDMA[%d]", e.Name(), i))
		e.ToL2RDMAs = append(e.ToL2RDMAs, p)
	}
}

// MapL2Dest associates an L2 RDMAPort with the index of its dedicated
// outbound src port in ToL2RDMAs.
func (e *RDMAEngineImpl) MapL2Dest(dst akita.Port, idx int) {
	e.destToL2Idx[dst] = idx
}

// srcPortForL2Dst returns the outbound port that should carry messages
// addressed to dst. Always returns the singular ToL2RDMA — per-bank slice
// form (ToL2RDMAs) is dead apparatus retained as struct field reference only.
func (e *RDMAEngineImpl) srcPortForL2Dst(dst akita.Port) akita.Port {
	return e.ToL2RDMA
}

// AddDramBankPorts grows ToDramTops to numBanks by appending additional
// outbound ports. Idempotent for numBanks=1.
func (e *RDMAEngineImpl) AddDramBankPorts(numBanks int) {
	if numBanks <= len(e.ToDramTops) {
		return
	}
	bufCap := 4 * e.numTransPerCycle
	for i := len(e.ToDramTops); i < numBanks; i++ {
		p := akita.NewLimitNumMsgPort(e, bufCap,
			fmt.Sprintf("%s.ToDramTop[%d]", e.Name(), i))
		e.ToDramTops = append(e.ToDramTops, p)
	}
}

// MapDramDest associates a DRAM TopPort with the index of its dedicated
// outbound src port in ToDramTops.
func (e *RDMAEngineImpl) MapDramDest(dst akita.Port, idx int) {
	e.destToDramIdx[dst] = idx
}

// srcPortForDramDst returns the outbound port that should carry messages
// addressed to dst (a DRAM TopPort). Always returns the singular ToDramTop
// — per-bank slice form (ToDramTops) is dead apparatus.
func (e *RDMAEngineImpl) srcPortForDramDst(dst akita.Port) akita.Port {
	return e.ToDramTop
}

// AddL2BottomBankPorts grows ToL2Bottoms to numBanks by appending additional
// ports (the first slot is the original ToL2Bottom). Idempotent for
// numBanks=1.
func (e *RDMAEngineImpl) AddL2BottomBankPorts(numBanks int) {
	if numBanks <= len(e.ToL2Bottoms) {
		return
	}
	bufCap := 4 * e.numTransPerCycle
	for i := len(e.ToL2Bottoms); i < numBanks; i++ {
		p := akita.NewLimitNumMsgPort(e, bufCap,
			fmt.Sprintf("%s.ToL2Bottom[%d]", e.Name(), i))
		e.ToL2Bottoms = append(e.ToL2Bottoms, p)
	}
}

// MapL2BottomDest associates an L2 BottomToRdmaPort with the index of its
// dedicated src port in ToL2Bottoms.
func (e *RDMAEngineImpl) MapL2BottomDest(dst akita.Port, idx int) {
	e.destToL2BottomIdx[dst] = idx
}

// srcPortForL2BottomDst returns the port that should carry RDMA->L2 bottom
// traffic addressed to dst (a per-L2 BottomToRdmaPort). Always returns the
// singular ToL2Bottom — per-bank slice form (ToL2Bottoms) is dead apparatus.
func (e *RDMAEngineImpl) srcPortForL2BottomDst(dst akita.Port) akita.Port {
	return e.ToL2Bottom
}
