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
type DataDirectRdmaEngine struct {
	RDMAEngineImpl

	DirectRequestPort  akita.Port
	DirectResponsePort akita.Port

	DirectRdmaReqAddressTable lmf.LowModuleFinder
	DirectRdmaRspAddressTable map[akita.Port]akita.Port
}

// Tick checks if make progress
func (e *DataDirectRdmaEngine) Tick(now akita.VTimeInSec) bool {
	madeProgress := false

	madeProgress = e.processFromCtrlPort(now) || madeProgress
	if e.isDraining {
		madeProgress = e.drainRDMA(now) || madeProgress
	}
	madeProgress = e.processTransRspFromOutsideAux(now) || madeProgress
	madeProgress = e.processAccessRspFromOutside(now) || madeProgress
	madeProgress = e.processRspFromDram(now) || madeProgress
	madeProgress = e.processRspFromL2(now) || madeProgress
	madeProgress = e.processTransReqFromOutsideAux(now) || madeProgress
	madeProgress = e.processDataReqFromOutside(now) || madeProgress
	madeProgress = e.processReqFromPwPort(now) || madeProgress
	madeProgress = e.processReqFromL2(now) || madeProgress
	madeProgress = e.processRetry(now) || madeProgress

	return madeProgress
}

func (e *DataDirectRdmaEngine) processReqFromL2(
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

func (e *DataDirectRdmaEngine) processRspFromL2(
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
			if e.processRspFromL2Aux(now, msg, port) {
				madeProgress = true
			}
		default:
			panic("unknown req type")
		}
	}
	return madeProgress
}

func (e *DataDirectRdmaEngine) processReqFromL2Aux(
	now akita.VTimeInSec,
	req protocol.AccessReq,
	recvPort akita.Port,
) bool {
	if e.outstandingReqs >= maxOutstandingReqs {
		if global.GetInstance().GetVerbose() {
			fmt.Println("Too many outstanding reqs, stop processing req from l2")
		}
		return false
	}

	dst := e.DirectRdmaReqAddressTable.Find(req.GetAddress())

	cloned := e.cloneReq(req)
	cloned.Meta().Src = e.DirectRequestPort
	cloned.Meta().Dst = dst
	cloned.Meta().SendTime = now

	err := e.DirectRequestPort.Send(cloned)
	if err == nil {
		recvPort.(*akita.LimitNumMsgPort).Retrieve(now)

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

		srcName := req.Meta().Src.Name()
		dstName := dst.Name()
		srcChip := strings.Split(srcName, ".")[1]
		dstChip := strings.Split(dstName, ".")[1]

		if req, ok := req.(*protocol.ReadReq); ok {
			if req.IsPtw {
				// tracing.StartTask(
				// cloned.Meta().ID,
				// "",
				// now,
				// e,
				// "pte-caching-remote-ptw-trace",
				// "",
				// srcChip+"."+dstChip)

				return true
			}
		}

		tracing.StartTask(
			cloned.Meta().ID,
			0,
			now,
			e,
			"hop-trace",
			"",
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

func (e *DataDirectRdmaEngine) processRspFromL2Aux(
	now akita.VTimeInSec,
	rsp protocol.AccessRsp,
	recvPort akita.Port,
) bool {
	transactionIndex := e.findTransactionByRspToID(
		rsp.GetRespondTo(), e.transactionsFromOutside)
	trans := e.transactionsFromOutside[transactionIndex]

	rspToOutside := e.cloneRsp(rsp, trans.fromOutside.Meta().ID)
	rspToOutside.Meta().SendTime = now
	rspToOutside.Meta().Src = e.DirectResponsePort
	rspToOutside.Meta().Dst = e.DirectRdmaRspAddressTable[trans.fromOutside.Meta().Src]

	err := e.DirectResponsePort.Send(rspToOutside)
	if err == nil {
		recvPort.Retrieve(now)

		tracing.TraceReqFinalize(trans.toInside, now, e)
		tracing.TraceReqComplete(trans.fromOutside, now, e)

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

func (e *DataDirectRdmaEngine) processRspFromDram(
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
			panic("unknown req type")
		}
	}
	return madeProgress
}

func (e *DataDirectRdmaEngine) processWriteDoneRspFromDram(
	now akita.VTimeInSec,
	rsp *protocol.WriteDoneRsp,
	recvPort akita.Port,
) bool {
	transactionIndex := e.findTransactionByRspToID(
		rsp.GetRespondTo(), e.transactionsFromOutside)
	trans := e.transactionsFromOutside[transactionIndex]

	rspToOutside := e.cloneRsp(rsp, trans.fromOutside.Meta().ID)
	rspToOutside.Meta().SendTime = now
	rspToOutside.Meta().Src = e.DirectResponsePort
	rspToOutside.Meta().Dst = e.DirectRdmaRspAddressTable[trans.fromOutside.Meta().Src]

	err := e.DirectResponsePort.Send(rspToOutside)
	if err == nil {
		recvPort.Retrieve(now)

		tracing.TraceReqFinalize(trans.toInside, now, e)
		tracing.TraceReqComplete(trans.fromOutside, now, e)

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

func (e *DataDirectRdmaEngine) processTransReqFromOutsideAux(
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
		default:
			log.Panicf("cannot process request of type %s", reflect.TypeOf(req))
			return false
		}
	}
	return madeProgress
}

func (e *DataDirectRdmaEngine) processDataReqFromOutside(
	now akita.VTimeInSec,
) bool {
	madeProgress := false
	port := e.DirectRequestPort.(*akita.LimitNumMsgPort)
	for i := 0; i < e.numTransPerCycle; i++ {
		req, idx, ok := port.PeekReady(e.canRouteDataDirectReqFromOutside)
		if !ok {
			return madeProgress
		}

		switch req := req.(type) {
		case *protocol.WriteReq: // write requests are only generated from eviction at llc
			madeProgress = e.processEvictReqFromOutside(now, req, idx) || madeProgress
		case *protocol.ReadReq:
			madeProgress = e.processReadReqFromOutside(now, req, idx) || madeProgress
		default:
			log.Panicf("cannot process request of type %s", reflect.TypeOf(req))
			return false
		}
	}
	return madeProgress
}

// canRouteDataDirectReqFromOutside reports whether the per-bank destination
// (ToDramTops[i] for evict writes, ToL2RDMAs[i] for read forwards) has room.
func (e *DataDirectRdmaEngine) canRouteDataDirectReqFromOutside(m akita.Msg) bool {
	switch req := m.(type) {
	case *protocol.WriteReq:
		dst := e.translationModuleFinder.Find(req.GetAddress())
		return dst.(canSender).CanSend(1)
	case *protocol.ReadReq:
		dst := e.localModuleFinder.Find(req.GetAddress())
		return dst.(canSender).CanSend(1)
	}
	return false
}

func (e *DataDirectRdmaEngine) processEvictReqFromOutside(
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
		e.DirectRequestPort.(*akita.LimitNumMsgPort).RetrieveAt(now, idx)

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

	return false
}

func (e *DataDirectRdmaEngine) processReadReqFromOutside(
	now akita.VTimeInSec,
	req *protocol.ReadReq,
	idx int,
) bool {
	dst := e.localModuleFinder.Find(req.GetAddress()) // rdma port of l2 cache
	src := e.srcPortForL2Dst(dst)

	cloned := e.cloneReq(req)
	cloned.Meta().SendTime = now
	cloned.Meta().Src = src
	cloned.Meta().Dst = dst // rdma port of local L2 cache.

	err := src.Send(cloned)
	if err == nil {
		e.DirectRequestPort.(*akita.LimitNumMsgPort).RetrieveAt(now, idx)

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

func (e *DataDirectRdmaEngine) processTransRspFromOutsideAux(now akita.VTimeInSec) bool {
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
		default:
			log.Panicf("cannot process request of type %s", reflect.TypeOf(req))
			return false
		}
	}
	return madeProgress
}

func (e *DataDirectRdmaEngine) processAccessRspFromOutside(now akita.VTimeInSec) bool {
	madeProgress := false
	port := e.DirectResponsePort.(*akita.LimitNumMsgPort)
	for i := 0; i < e.numTransPerCycle; i++ {
		req, idx, ok := port.PeekReady(e.canRouteDataDirectRspFromOutside)
		if !ok {
			return madeProgress
		}

		switch req := req.(type) {
		case protocol.AccessRsp:
			madeProgress = e.processAccessRspFromOutsideAux(now, req, idx) || madeProgress
		default:
			log.Panicf("cannot process request of type %s", reflect.TypeOf(req))
			return false
		}
	}
	return madeProgress
}

// canRouteDataDirectRspFromOutside reports whether the per-bank ToL2Bottoms[i]
// resolved from the original requester has buffer room.
func (e *DataDirectRdmaEngine) canRouteDataDirectRspFromOutside(m akita.Msg) bool {
	rsp, ok := m.(protocol.AccessRsp)
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

func (e *DataDirectRdmaEngine) findTransactionByRspToIDIfAny(
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

func (e *DataDirectRdmaEngine) processAccessRspFromOutsideAux(
	now akita.VTimeInSec,
	rsp protocol.AccessRsp,
	idx int,
) bool {
	transactionIndex := e.findTransactionByRspToID(
		rsp.GetRespondTo(), e.transactionsFromInside)

	// get the orignial request
	trans := e.transactionsFromInside[transactionIndex]

	src := e.srcPortForL2BottomDst(trans.fromInside.Meta().Src)
	rspToInside := e.cloneRsp(rsp, trans.fromInside.Meta().ID)
	rspToInside.Meta().SendTime = now
	rspToInside.Meta().Src = src
	rspToInside.Meta().Dst = trans.fromInside.Meta().Src

	err := src.Send(rspToInside)
	if err == nil {
		e.DirectResponsePort.(*akita.LimitNumMsgPort).RetrieveAt(now, idx)

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

	return false
}

func (e *DataDirectRdmaEngine) findTransactionByRspToID(
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

func (e *DataDirectRdmaEngine) cloneReq(origin protocol.AccessReq) protocol.AccessReq {
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

func (e *DataDirectRdmaEngine) cloneRsp(origin protocol.AccessRsp, rspTo uint64) protocol.AccessRsp {
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
func (e *DataDirectRdmaEngine) SetFreq(freq akita.Freq) {
	e.TickingComponent.Freq = freq
}

// NewEngine creates new engine
func NewDataDirectRdmaEngine(
	name string,
	engine akita.Engine,
	numTransPerCycle int,
) RDMAEngine {
	e := new(DataDirectRdmaEngine)
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

	e.DirectRequestPort = akita.NewLimitNumMsgPort(e, 4*numTransPerCycle, name+".DirectRequestPort")
	e.DirectResponsePort = akita.NewLimitNumMsgPort(e, 4*numTransPerCycle, name+".DirectResponsePort")

	e.RequestPort = akita.NewLimitNumMsgPort(e, 4*numTransPerCycle, name+".RequestPort")
	e.ResponsePort = akita.NewLimitNumMsgPort(e, 4*numTransPerCycle, name+".ResponsePort")

	e.numTransPerCycle = numTransPerCycle

	e.retryPort = akita.NewLimitNumMsgPort(e, 100000000, name+".RetryPort")

	return e
}
