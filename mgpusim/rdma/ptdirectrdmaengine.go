// Package rdma provides the implementation of an RDMA engine.
package rdma

import (
	"fmt"
	"log"
	"reflect"
	"strings"

	"gitlab.com/akita/akita"
	"gitlab.com/akita/lmf"
	"gitlab.com/akita/mem/idealmemcontroller"
	"gitlab.com/akita/protocol"
	"gitlab.com/akita/util/global"
	"gitlab.com/akita/util/tracing"
)

// An Engine is a component that helps one GPU to access the memory on
// another GPU
type PtDirectRdmaEngine struct {
	RDMAEngineImpl

	DirectPwReqPort akita.Port
	DirectPwRspPort akita.Port

	DirectPtRdmaReqAddressTable lmf.LowModuleFinder
	DirectPtRdmaRspAddressTable map[akita.Port]akita.Port
}

// Tick checks if make progress
func (e *PtDirectRdmaEngine) Tick(now akita.VTimeInSec) bool {
	madeProgress := false

	madeProgress = e.processFromCtrlPort(now) || madeProgress
	if e.isDraining {
		madeProgress = e.drainRDMA(now) || madeProgress
	}
	madeProgress = e.processTransRspFromOutside(now) || madeProgress
	madeProgress = e.processRspFromOutside(now) || madeProgress
	madeProgress = e.processFromPwPort(now) || madeProgress
	madeProgress = e.processRspFromDram(now) || madeProgress
	madeProgress = e.processRspFromL2(now) || madeProgress
	madeProgress = e.processTransReqFromOutside(now) || madeProgress
	madeProgress = e.processReqFromOutside(now) || madeProgress
	madeProgress = e.processReqFromL2(now) || madeProgress
	madeProgress = e.processRetry(now) || madeProgress

	return madeProgress
}

func (e *PtDirectRdmaEngine) processRspFromDram(
	now akita.VTimeInSec,
) bool {
	// if e.pauseIncomingReqsFromL1 {
	// 	return false
	// }

	madeProgress := false
	for i := 0; i < e.numTransPerCycle; i++ {
		rsp := e.ToDramTop.Peek()
		if rsp == nil {
			return madeProgress
		}

		switch rsp := rsp.(type) {
		case *protocol.DataReadyRsp: // for the translation response
			madeProgress = e.processTransRspFromDram(now, rsp, e.ToDramTop) || madeProgress
		case *protocol.WriteDoneRsp:
			madeProgress = e.processWriteDoneRspFromDram(now, rsp, e.ToDramTop) || madeProgress
		default:
			panic("unknown rsp type")
		}
	}
	return madeProgress
}

func (e *PtDirectRdmaEngine) processTransRspFromDram(
	now akita.VTimeInSec,
	rsp *protocol.DataReadyRsp,
	recvPort akita.Port,
) bool {
	// if !e.swRspPort.(*akita.LimitNumMsgPort).CanSend(e.numTransPerCycle) {
	// 	return false
	// }

	// before current response, original request must exist before.
	transactionIndex := e.findTransactionByRspToID(
		rsp.GetRespondTo(), e.transactionsFromOutside)

	// this is the original request for the current cloned translation response
	trans := e.transactionsFromOutside[transactionIndex]

	// destination can be gotten from the source of original request.
	dst := e.DirectPtRdmaRspAddressTable[trans.fromOutside.Meta().Src]

	cloned := protocol.TransDataReadyRspBuilder{}.
		WithSendTime(now).
		WithSrc(rsp.Src).
		WithDst(rsp.Dst).
		WithRspTo(trans.fromOutside.Meta().ID).
		WithData(rsp.Data).
		WithInfo(rsp.Info).
		Build()

	cloned.Meta().Src = e.DirectPwRspPort
	cloned.Meta().Dst = dst
	cloned.Meta().SendTime = now

	networkMsg := &protocol.NetworkMsg{
		Msg:     cloned,
		SrcPort: e.DirectPwRspPort,
	}

	err := e.DirectPwRspPort.Send(networkMsg)
	if err == nil {
		recvPort.Retrieve(now)

		// trans.toInside, which is "cloned" is finally done.
		tracing.TraceReqFinalize(trans.toInside, now, e)

		// trans.fromOutside, which is "req" or original request is almost done (completed).
		tracing.TraceReqComplete(trans.fromOutside, now, e)

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

func (e *PtDirectRdmaEngine) processTransReqFromOutside(
	now akita.VTimeInSec,
) bool {
	madeProgress := false
	port := e.DirectPwReqPort.(*akita.LimitNumMsgPort)
	for i := 0; i < e.numTransPerCycle; i++ {
		req, idx, ok := port.PeekReady(e.canRoutePtDirectReqFromOutside)
		if !ok {
			return madeProgress
		}

		switch req := req.(type) {
		case *protocol.TransReadReq: // write requests are only generated from eviction at llc
			madeProgress = e.processDirectTransReqFromOutside(now, req, idx) || madeProgress
		default:
			log.Panicf("cannot process request of type %s", reflect.TypeOf(req))
			return false
		}
	}

	return madeProgress
}

// canRoutePtDirectReqFromOutside reports whether the destination DRAM bank
// (carrying the converted PTW read) has buffer room.
func (e *PtDirectRdmaEngine) canRoutePtDirectReqFromOutside(m akita.Msg) bool {
	switch req := m.(type) {
	case *protocol.TransReadReq:
		dst := e.translationModuleFinder.Find(req.GetAddress())
		return dst.(canSender).CanSend(1)
	}
	return false
}

func (e *PtDirectRdmaEngine) processTransRspFromOutside(now akita.VTimeInSec) bool {
	madeProgress := false
	port := e.DirectPwRspPort.(*akita.LimitNumMsgPort)
	for i := 0; i < e.numTransPerCycle; i++ {
		Rsp, idx, ok := port.PeekReady(e.canRoutePtDirectRspFromOutside)
		if !ok {
			return madeProgress
		}

		switch Rsp := Rsp.(type) {
		case *protocol.TransDataReadyRsp:
			madeProgress = e.processDirectTransRspFromOutside(now, Rsp, idx) || madeProgress
		default:
			log.Panicf("cannot process response of type %s", reflect.TypeOf(Rsp))
			return false
		}
	}
	return madeProgress
}

// canRoutePtDirectRspFromOutside reports whether the inside MMU port (PwPort
// carries the response onward to the original requester) has buffer room.
func (e *PtDirectRdmaEngine) canRoutePtDirectRspFromOutside(m akita.Msg) bool {
	rsp, ok := m.(*protocol.TransDataReadyRsp)
	if !ok {
		return false
	}
	txIdx := e.findTransactionByRspToIDIfAny(
		rsp.GetRespondTo(), e.transactionsFromInside)
	if txIdx < 0 {
		return false
	}
	dst := e.transactionsFromInside[txIdx].fromInside.Meta().Src
	return dst.(canSender).CanSend(1)
}

func (e *PtDirectRdmaEngine) findTransactionByRspToIDIfAny(
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
	return -1
}

func (e *PtDirectRdmaEngine) processFromPwPort(
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
			madeProgress = e.processReqFromPwPort(now, trans) || madeProgress
		default:
			log.Panicf("cannot process request of type %s", reflect.TypeOf(trans))
			return false
		}
	}
	return madeProgress
}

func (e *PtDirectRdmaEngine) processReqFromPwPort(
	now akita.VTimeInSec,
	req *protocol.ReadReq,
) bool {
	if e.outstandingReqs >= maxOutstandingReqs {
		if global.GetInstance().GetVerbose() {
			fmt.Println("Too many outstanding reqs", e.Name())
		}
		return false
	}

	dst := e.DirectPtRdmaReqAddressTable.Find(req.GetAddress())

	cloned := protocol.TransReadReqBuilder{}.
		WithSendTime(now).
		WithSrc(req.Src).
		WithDst(req.Dst).
		WithPID(req.PID).
		WithAddress(req.GetAddress()).
		WithByteSize(8).
		WithInfo(req.Info).
		Build()

	cloned.Meta().Src = e.DirectPwReqPort
	cloned.Meta().Dst = dst
	cloned.Meta().SendTime = now

	err := e.DirectPwReqPort.Send(cloned)
	if err == nil {
		e.PwPort.Retrieve(now)

		tracing.TraceReqReceive(req, now, e)
		tracing.TraceReqInitiate(cloned, now, e,
			tracing.MsgIDAtReceiver(req, e))

		srcName := req.Meta().Src.Name()
		dstName := dst.Name()
		srcChip := strings.Split(srcName, ".")[1]
		dstChip := strings.Split(dstName, ".")[1]

		tracing.AddTaskStep(tracing.MsgIDAtReceiver(req, e), now, e,
			srcChip+":PageAccessToChiplet:"+dstChip)

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

func (e *PtDirectRdmaEngine) processDirectTransReqFromOutside(
	now akita.VTimeInSec,
	req *protocol.TransReadReq,
	idx int,
) bool {
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
		e.DirectPwReqPort.(*akita.LimitNumMsgPort).RetrieveAt(now, idx)

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
		fmt.Println("Error on processDirectTransReqFromOutside, ",
			e.Name(),
			cloned.Meta().Src.Name(),
			cloned.Meta().Dst.Name(),
			req.Meta().ID,
		)
	}

	// e.SortReqPortBuffer()
	return false
}

func (e *PtDirectRdmaEngine) processDirectTransRspFromOutside(
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
		e.DirectPwRspPort.(*akita.LimitNumMsgPort).RetrieveAt(now, idx)

		tracing.TraceReqFinalize(trans.toOutside, now, e)
		tracing.TraceReqComplete(trans.fromInside, now, e)

		e.transactionsFromInside =
			append(e.transactionsFromInside[:transactionIndex],
				e.transactionsFromInside[transactionIndex+1:]...)

		e.outstandingReqs--

		return true
	}
	if global.GetInstance().GetVerbose() {
		fmt.Println("Error on processDirectTransRspFromOutside, ",
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

func (e *PtDirectRdmaEngine) findTransactionByRspToID(
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

// SetFreq sets freq
func (e *PtDirectRdmaEngine) SetFreq(freq akita.Freq) {
	e.TickingComponent.Freq = freq
}

// NewEngine creates new engine
func NewPtDirectRdmaEngine(
	name string,
	engine akita.Engine,
	numTransPerCycle int,
) RDMAEngine {
	e := new(PtDirectRdmaEngine)
	e.TickingComponent = akita.NewTickingComponent(
		name,
		engine,
		1*akita.GHz,
		e)

	e.ToL2RDMA = akita.NewLimitNumMsgPort(e, 4*numTransPerCycle, name+".ToL2RDMA")
	e.ToL2RDMAs = []akita.Port{e.ToL2RDMA}
	e.destToL2Idx = map[akita.Port]int{}
	e.ToL2Bottom = akita.NewLimitNumMsgPort(e, 4*numTransPerCycle, name+".ToL2Bottom")
	e.ToL2Bottoms = []akita.Port{e.ToL2Bottom}
	e.destToL2BottomIdx = map[akita.Port]int{}
	e.ToDramTop = akita.NewLimitNumMsgPort(e, 4*numTransPerCycle, name+".ToDramTop")
	e.ToDramTops = []akita.Port{e.ToDramTop}
	e.destToDramIdx = map[akita.Port]int{}
	e.CtrlPort = akita.NewLimitNumMsgPort(e, numTransPerCycle, name+".CtrlPort")
	e.PwPort = akita.NewLimitNumMsgPort(e, 4*numTransPerCycle, name+".PwPort")

	e.DirectPwReqPort = akita.NewLimitNumMsgPort(e, 4*numTransPerCycle, name+".DirectPwReqPort")
	e.DirectPwRspPort = akita.NewLimitNumMsgPort(e, 4*numTransPerCycle, name+".DirectPwRspPort")

	e.RequestPort = akita.NewLimitNumMsgPort(e, 4*numTransPerCycle, name+".RequestPort")
	e.ResponsePort = akita.NewLimitNumMsgPort(e, 4*numTransPerCycle, name+".ResponsePort")

	e.numTransPerCycle = numTransPerCycle

	e.retryPort = akita.NewLimitNumMsgPort(e, 100000000, name+".RetryPort")

	return e
}
