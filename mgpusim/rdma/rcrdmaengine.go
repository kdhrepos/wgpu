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
	"gitlab.com/akita/protocol"
	"gitlab.com/akita/util/global"
	"gitlab.com/akita/util/tracing"
)

// An Engine is a component that helps one GPU to access the memory on
// another GPU
type RCRDMAEngine struct {
	*RDMAEngineImpl

	RdmaReqPortByChipletId map[int]akita.Port
	RdmaRspPortByChipletId map[int]akita.Port

	localModuleFinderForLookup lmf.LowModuleFinder
}

func (e *RCRDMAEngine) SetLocalModuleFinderForLookup(lmf lmf.LowModuleFinder) {
	e.localModuleFinderForLookup = lmf
}

// Tick checks if make progress
func (e *RCRDMAEngine) Tick(now akita.VTimeInSec) bool {
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

	// return true
	return madeProgress
}

func (e *RCRDMAEngine) processReqFromL2(
	now akita.VTimeInSec,
) bool {
	// if e.pauseIncomingReqsFromL1 {
	// 	return false
	// }

	madeProgress := false
	port := e.ToL2Bottom
	for i := 0; i < e.numTransPerCycle; i++ {
		req := port.Peek()
		if req == nil {
			continue
		}

		switch msg := req.(type) {
		case *protocol.ReadReq:
			if e.processDataReqFromL2(now, msg, port) {
				madeProgress = true
			}
		case *protocol.WriteReq:
			if e.processReqFromL2Aux(now, msg, port) {
				madeProgress = true
			}
		default:
			panic("unknown req type")
		}
	}
	return madeProgress
}

func (e *RCRDMAEngine) processRspFromL2(
	now akita.VTimeInSec,
) bool {
	madeProgress := false
	port := e.ToL2RDMA.(*akita.LimitNumMsgPort)
	for i := 0; i < e.numTransPerCycle; i++ {
		// Drain sink-bound data/write responses FIRST: they finalize
		// transactions (free MSHRs / outstanding reqs / inflight fetches), so
		// real request/response traffic makes forward progress ahead of the
		// NearFetch lookup chain. LookupRsp (lookup-miss forwarding) is handled
		// second.
		if m, idx, ok := port.PeekReady(isSinkBoundRspFromL2); ok {
			if e.processRspFromL2Aux(now, m.(protocol.AccessRsp), port, idx) {
				madeProgress = true
				continue
			}
		}
		if m, idx, ok := port.PeekReady(isLookupRsp); ok {
			if e.processLookupRspFromL2(now, m.(*protocol.LookupRsp), idx) {
				madeProgress = true
				continue
			}
		}
		if head := port.Peek(); head != nil &&
			!isSinkBoundRspFromL2(head) && !isLookupRsp(head) {
			panic("unknown rsp type from L2 (rcr)")
		}
		break
	}
	return madeProgress
}

// isLookupRsp reports whether m is a NearFetch LookupRsp (the non-sink-bound
// response class on ToL2RDMA that may re-inject onto the request network).
func isLookupRsp(m akita.Msg) bool {
	_, ok := m.(*protocol.LookupRsp)
	return ok
}

func (e *RCRDMAEngine) processLookupRspFromL2(
	now akita.VTimeInSec,
	rsp *protocol.LookupRsp,
	idx int,
) bool {
	transactionIndex := e.findTransactionByRspToID(
		rsp.GetRespondTo(), e.transactionsFromOutside)
	trans := e.transactionsFromOutside[transactionIndex]

	originalReq, ok := trans.fromOutside.(*protocol.LookupReq)
	if !ok {
		panic("processLookupRspFromL2")
	}

	dst := e.RdmaReqAddressTable.Find(originalReq.Address)
	nextHopDst := e.getNextHopChipletId(
		dst.Component().(*RCRDMAEngine).GetChipletId())
	nextHopDstPort := e.RdmaReqPortByChipletId[nextHopDst]

	req := protocol.LookupReqBuilder{}.
		WithSendTime(now).
		WithSrc(originalReq.Meta().Src).
		WithDst(nextHopDstPort).
		WithPID(originalReq.PID).
		WithCornerNodeId(originalReq.CornerNodeId).
		WithAddress(originalReq.Address).
		WithByteSize(originalReq.AccessByteSize).
		WithInfo(originalReq.Info).
		WithHop(originalReq.Hop + 1).     // missed here, advance to next chiplet
		WithHopBase(originalReq.HopBase). // leg base unchanged within a leg
		Build()

	req.Meta().ID = originalReq.Meta().ID
	req.HopTraceID = originalReq.HopTraceID // keep origin task across forwards

	networkMsg := &protocol.NetworkMsg{
		Msg:     req,
		SrcPort: e.RequestPort,
	}

	err := e.RequestPort.Send(networkMsg)
	if err == nil {
		e.ToL2RDMA.(*akita.LimitNumMsgPort).RetrieveAt(now, idx)

		// Close the req_in task opened by TraceReqReceive when this chiplet
		// received originalReq. A relayed lookup miss never returns here, so
		// without this the task lingers in every req_in tracer's inflight map
		// forever (leak scaling with chain length). The engine already drops
		// its own transaction below; this keeps the trace side symmetric.
		tracing.TraceReqComplete(originalReq, now, e)

		e.transactionsFromOutside =
			append(e.transactionsFromOutside[:transactionIndex],
				e.transactionsFromOutside[transactionIndex+1:]...)

		return true
	} else {
		if global.GetInstance().GetVerbose() {
			fmt.Println("Error on processLookupRspFromL2, ",
				e.Name(),
				req.Meta().Src.Name(),
				req.Meta().Dst.Name(),
				req.Meta().ID,
			)
		}
	}

	return false
}

func (e *RCRDMAEngine) processDataReqFromL2(
	now akita.VTimeInSec,
	req *protocol.ReadReq,
	recvPort akita.Port,
) bool {
	e.noteFlightFirstSeen(req.Meta().ID, now)

	if e.outstandingReqs >= maxOutstandingReqs {
		if global.GetInstance().GetVerbose() {
			fmt.Println("Too many outstanding reqs, stop processing data req from l2")
		}
		return false
	}

	dst := e.RdmaReqAddressTable.Find(req.GetAddress())
	nextHopDstPort := e.RdmaReqPortByChipletId[e.getNextHopChipletId(
		dst.Component().(*RCRDMAEngine).ChipletId)]
	cornerNodeId := e.getCornerNodeId(
		e.ChipletId, dst.Component().(*RCRDMAEngine).ChipletId)

	// Every remote read travels as a hop-by-hop LookupReq (NearFetch data
	// forwarding): each intermediate chiplet does an LLC lookup — a hit forwards
	// data straight back to the requestor, a miss relays onward. The requestor
	// already holds its OWN real MSHR (intra-module reply tracking), so a forwarded
	// hit has somewhere to land; non-corner hops only relay (no MSHR); the corner
	// allocates its own MSHR to coalesce concurrent requestors and multicast.
	cloned := protocol.LookupReqBuilder{}.
		WithSendTime(now).
		WithSrc(e.RequestPort).
		WithDst(nextHopDstPort).
		WithCornerNodeId(cornerNodeId).
		WithPID(req.PID).
		WithAddress(req.GetAddress()).
		WithByteSize(req.AccessByteSize).
		WithInfo(req.Info).
		WithHop(1).               // first intermediate-chiplet lookup of this leg
		WithHopBase(req.HopBase). // requestor->leg-start hops (0 on origin leg)
		Build()

	// One logical access spans two network legs: requestor->corner (origin) and
	// corner->home (continuation). The continuation inherits the origin's task ID
	// so BOTH legs' switch hops accumulate onto one HopTracer task — a 0->5->35
	// access traces as a single 10-hop request, not two separate 5-hop requests.
	isContinuation := req.HopTraceID != 0
	if isContinuation {
		cloned.Meta().ID = req.HopTraceID
		cloned.HopTraceID = req.HopTraceID
	} else {
		cloned.HopTraceID = cloned.Meta().ID
	}

	err := e.RequestPort.Send(&protocol.NetworkMsg{
		Msg:     cloned,
		SrcPort: e.RequestPort,
	})
	if err != nil {
		if global.GetInstance().GetVerbose() {
			fmt.Println("Error on processDataReqFromL2, ",
				e.Name(), cloned.Meta().Src.Name(),
				cloned.Meta().Dst.Name(), req.Meta().ID)
		}
		return false
	}

	recvPort.(*akita.LimitNumMsgPort).Retrieve(now)
	tracing.TraceReqReceive(req, now, e)
	tracing.TraceReqInitiate(cloned, now, e, tracing.MsgIDAtReceiver(req, e))
	emitRdmaFlightStartAt(cloned,
		e.consumeFlightFirstSeen(req.Meta().ID, now), e)

	if !req.IsPtw && !isContinuation {
		dstChip := strings.Split(dst.Name(), ".")[1]
		srcChip := strings.Split(e.Name(), ".")[1]
		pc, _ := req.Info.(uint64)
		tracing.StartTask(cloned.Meta().ID, 0, now, e,
			"hop-trace", strconv.FormatUint(pc, 10), srcChip+"."+dstChip)
	}

	e.transactionsFromInside = append(e.transactionsFromInside, transaction{
		fromInside: req,
		toOutside:  cloned,
	})
	e.outstandingReqs++
	return true
}

func (e *RCRDMAEngine) processReqFromOutside(
	now akita.VTimeInSec,
) bool {
	madeProgress := false
	port := e.RequestPort.(*akita.LimitNumMsgPort)
	for i := 0; i < e.numTransPerCycle; i++ {
		req, idx, ok := port.PeekReady(e.canRouteRCRReqFromOutside)
		if !ok {
			return madeProgress
		}

		switch req := req.(type) {
		case *protocol.TransReadReq:
			madeProgress = e.processTransReqFromOutside(now, req, idx) || madeProgress
		case *protocol.WriteReq: // write requests are only generated from eviction at llc
			madeProgress = e.processEvictReqFromOutside(now, req, idx) || madeProgress
		case *protocol.LookupReq:
			madeProgress = e.processLookupReqFromOutside(now, req, idx) || madeProgress
		case protocol.AccessReq:
			madeProgress = e.processDataReqFromOutside(now, req, idx) || madeProgress
		default:
			log.Panicf("cannot process request of type %s", reflect.TypeOf(req))
			return false
		}
	}
	return madeProgress
}

// canRouteRCRReqFromOutside reports whether the dst port (DRAM bank / L2
// bank / lookup-table L2) has buffer room. LookupReq picks dst based on
// whether the request already reached its origin chiplet.
func (e *RCRDMAEngine) canRouteRCRReqFromOutside(m akita.Msg) bool {
	switch req := m.(type) {
	case *protocol.TransReadReq:
		dst := e.translationModuleFinder.Find(req.GetAddress())
		return e.dstReadyVia(e.srcPortForDramDst(dst), dst)
	case *protocol.WriteReq:
		dst := e.translationModuleFinder.Find(req.GetAddress())
		return e.dstReadyVia(e.srcPortForDramDst(dst), dst)
	case *protocol.LookupReq:
		originDst := e.RdmaReqAddressTable.Find(req.GetAddress())
		var dst akita.Port
		if req.Meta().Dst == originDst {
			dst = e.localModuleFinder.Find(req.GetAddress())
		} else {
			dst = e.localModuleFinderForLookup.Find(req.GetAddress())
		}
		return e.dstReadyVia(e.ToL2RDMA, dst)
	case protocol.AccessReq:
		dst := e.localModuleFinder.Find(req.GetAddress())
		return e.dstReadyVia(e.srcPortForL2Dst(dst), dst)
	}
	return false
}

func (e *RCRDMAEngine) processLookupReqFromOutside(
	now akita.VTimeInSec,
	req *protocol.LookupReq,
	idx int,
) bool {
	src := e.ToL2RDMA
	originDst := e.RdmaReqAddressTable.Find(req.GetAddress())

	// arrived at the global home: every intermediate-chiplet lookup along the
	// path missed, so the data-forwarding request now reads directly here.
	reachedGlobalHome := req.Meta().Dst == originDst

	var reqToInside akita.Msg
	// arrived at the destination chiplet, don't lookup, just read.
	if reachedGlobalHome {
		dst := e.localModuleFinder.Find(req.GetAddress()) // rdma port of l2 cache

		reqToInside = protocol.ReadReqBuilder{}.
			WithSendTime(now).
			WithSrc(src).
			WithDst(dst).
			WithPID(req.PID).
			WithAddress(req.GetAddress()).
			WithByteSize(req.AccessByteSize).
			WithInfo(req.Info).
			Build()
	} else {
		dst := e.localModuleFinderForLookup.Find(req.GetAddress()) // rdma port of l2 cache

		lookupToInside := protocol.LookupReqBuilder{}.
			WithSendTime(now).
			WithSrc(src).
			WithDst(dst).
			WithPID(req.PID).
			WithAddress(req.GetAddress()).
			WithCornerNodeId(req.CornerNodeId).
			WithByteSize(req.AccessByteSize).
			WithInfo(req.Info).
			WithHop(req.Hop).         // carry hop into the local L2 lookup
			WithHopBase(req.HopBase). // carry leg base into the local L2 lookup
			Build()
		lookupToInside.HopTraceID = req.HopTraceID // carry origin task to corner
		reqToInside = lookupToInside
	}

	err := src.Send(reqToInside)
	if err == nil {
		e.RequestPort.(*akita.LimitNumMsgPort).RetrieveAt(now, idx)

		if req.Hop == 1 {
			emitRdmaFlightEnd(req, now, e)
		}

		// Start the incoming "req_in" task. It is closed either by
		// processRspFromL2Aux (TraceReqComplete) when this chiplet serves the
		// data locally (LLC hit or global-home read), or by
		// processLookupRspFromL2 when a lookup miss relays onward. Both paths
		// end the task so req_in tracers never leak inflight entries.
		tracing.TraceReqReceive(req, now, e)

		if reachedGlobalHome {
			// Lookup chain failed at every intermediate chiplet and fell
			// back to the global home. Counted once per request.
			tracing.StartTask(0, 0, now, e,
				"data-forwarding-reached-gh", "",
				&tracing.DataForwardingDetail{IsPtw: req.IsPtw, PC: tracing.PCFromInfo(req.Info)})
		}

		trans := transaction{
			fromOutside: req,
			toInside:    reqToInside,
		}
		e.transactionsFromOutside = append(e.transactionsFromOutside, trans)
		return true
	} else {
		if global.GetInstance().GetVerbose() {
			fmt.Println("Error on processLookupReqFromOutside, ",
				e.Name(),
				reqToInside.Meta().Src.Name(),
				reqToInside.Meta().Dst.Name(),
				req.Meta().ID,
			)
		}
	}

	// e.SortReqPortBuffer()
	return false
}

func (e *RCRDMAEngine) findTransactionByRspToID(
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

// srcEndStr formats the connection source-side buffer occupancy of an
// outbound port (what Send actually gates on) as "used/cap". This is the
// backpressure point the dst-based readiness check does not observe.
func srcEndStr(p akita.Port) (string, int) {
	lp, ok := p.(*akita.LimitNumMsgPort)
	if !ok {
		return "?", 0
	}
	used, capacity, ok := lp.OutBufLen()
	if !ok {
		return "-", 0
	}
	return fmt.Sprintf("%d/%d", used, capacity), used
}

// DeadlockSummary augments the embedded RDMAEngineImpl summary with the
// connection source-side buffer occupancy of the outbound ports, so a full
// ToL2RDMA/ToDramTop srcEnd (invisible to port.Peek) is reported.
func (e *RCRDMAEngine) DeadlockSummary() (string, bool) {
	base, basePending := e.RDMAEngineImpl.DeadlockSummary()
	l2s, l2u := srcEndStr(e.ToL2RDMA)
	dramS, dramU := srcEndStr(e.ToDramTop)
	botS, botU := srcEndStr(e.ToL2Bottom)
	line := fmt.Sprintf("%s srcEnd[l2rdma=%s dramTop=%s l2bot=%s]",
		base, l2s, dramS, botS)
	return line, basePending || l2u > 0 || dramU > 0 || botU > 0
}

// SetFreq sets freq
func (e *RCRDMAEngine) SetFreq(freq akita.Freq) {
	e.TickingComponent.Freq = freq
}

// NewEngine creates new engine
func NewRCRDMAEngine(
	name string,
	engine akita.Engine,
	numTransPerCycle int,
) RDMAEngine {
	rdmaEngineImpl := new(RDMAEngineImpl)
	rcRdmaEngine := new(RCRDMAEngine)
	rcRdmaEngine.RDMAEngineImpl = rdmaEngineImpl
	rcRdmaEngine.TickingComponent = akita.NewTickingComponent(
		name,
		engine,
		1*akita.GHz,
		rcRdmaEngine)

	rcRdmaEngine.ToL2RDMA = akita.NewLimitNumMsgPort(rcRdmaEngine, 16*numTransPerCycle, name+".ToL2RDMA")
	rcRdmaEngine.ToL2RDMAs = []akita.Port{rcRdmaEngine.ToL2RDMA}
	rcRdmaEngine.destToL2Idx = map[akita.Port]int{}
	rcRdmaEngine.ToL2Bottom = akita.NewLimitNumMsgPort(rcRdmaEngine, 16*numTransPerCycle, name+".ToL2Bottom")
	rcRdmaEngine.ToL2Bottoms = []akita.Port{rcRdmaEngine.ToL2Bottom}
	rcRdmaEngine.destToL2BottomIdx = map[akita.Port]int{}
	rcRdmaEngine.ToDramTop = akita.NewLimitNumMsgPort(rcRdmaEngine, 16*numTransPerCycle, name+".ToDramTop")
	rcRdmaEngine.ToDramTops = []akita.Port{rcRdmaEngine.ToDramTop}
	rcRdmaEngine.destToDramIdx = map[akita.Port]int{}
	rcRdmaEngine.CtrlPort = akita.NewLimitNumMsgPort(rcRdmaEngine, numTransPerCycle, name+".CtrlPort")
	rcRdmaEngine.PwPort = akita.NewLimitNumMsgPort(rcRdmaEngine, 16*numTransPerCycle, name+".PwPort")

	rcRdmaEngine.RequestPort = akita.NewLimitNumMsgPort(rcRdmaEngine, 16*numTransPerCycle, name+".RequestPort")
	rcRdmaEngine.ResponsePort = akita.NewLimitNumMsgPort(rcRdmaEngine, 16*numTransPerCycle, name+".ResponsePort")

	rcRdmaEngine.numTransPerCycle = numTransPerCycle

	rcRdmaEngine.retryPort = akita.NewLimitNumMsgPort(rcRdmaEngine, 100000000, name+".RetryPort")

	return rcRdmaEngine
}

func (e *RCRDMAEngine) getCornerNodeId(
	srcChipId, dstChipId int,
) int {
	srcY := srcChipId / e.xDim
	dstX := dstChipId % e.xDim

	return (srcY*e.xDim + dstX)
}

func (e *RCRDMAEngine) getNextHopChipletId(
	dstChipId int,
) int {
	srcChipId := e.GetChipletId()
	srcX := srcChipId % e.xDim
	srcY := srcChipId / e.xDim
	dstX := dstChipId % e.xDim
	dstY := dstChipId / e.xDim

	if srcX < dstX {
		return srcChipId + 1
	} else if srcX > dstX {
		return srcChipId - 1
	}

	if srcY < dstY {
		return srcChipId + e.xDim
	} else if srcY > dstY {
		return srcChipId - e.xDim
	}

	return srcChipId
}
