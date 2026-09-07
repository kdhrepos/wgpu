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

// DataIdealRdmaEngine is the symmetric inverse of PtIdealRdmaEngine: the
// engine logic (traffic split by port) is identical, but the data-ideal
// builder wires the data ports (RequestPort / ResponsePort) to an ideal
// 1-hop crossbar and the PTW ports (PtwReqPort / PtwRspPort) to the
// realistic mesh NoC. PTW thus pays the mesh hop-count penalty while data
// is near-zero distance -- the mirror image of pt-ideal.
//
// PTW is identified by the IsPtw flag on protocol.ReadReq (PTE-caching path
// from L2) or by the message type *protocol.TransReadReq (MMU page-walker
// path through PwPort). Because the PTW ports ride the buffer-limited mesh,
// the PtwReqPort / PtwRspPort name strings contain "Request" / "Response" so
// MeshRoutingTable.FindPort classifies them, and the inbound handlers use the
// same ready-aware PeekReady the base RequestPort handlers use.
//
// Used exclusively by the data-ideal platform; other platforms instantiate
// RDMAEngineImpl directly and are unaffected. The package-level transWasPtw
// helper is shared with ptideal_rdmaengine.go.
type DataIdealRdmaEngine struct {
	RDMAEngineImpl

	PtwReqPort akita.Port
	PtwRspPort akita.Port

	PtwRdmaReqAddressTable lmf.LowModuleFinder
}

// Tick checks if make progress.
func (e *DataIdealRdmaEngine) Tick(now akita.VTimeInSec) bool {
	madeProgress := false

	madeProgress = e.processFromCtrlPort(now) || madeProgress
	if e.isDraining {
		madeProgress = e.drainRDMA(now) || madeProgress
	}

	// PTW-network polling (parallel to the regular mesh ports).
	madeProgress = e.processPtwRspFromOutside(now) || madeProgress

	madeProgress = e.processRspFromOutside(now) || madeProgress
	madeProgress = e.processRspFromDram(now) || madeProgress
	madeProgress = e.processRspFromL2(now) || madeProgress
	madeProgress = e.processPtwReqFromOutside(now) || madeProgress
	madeProgress = e.processReqFromOutside(now) || madeProgress
	madeProgress = e.processReqFromPwPort(now) || madeProgress
	madeProgress = e.processReqFromL2(now) || madeProgress
	madeProgress = e.processRetry(now) || madeProgress

	return madeProgress
}

// (transWasPtw is defined in ptideal_rdmaengine.go and shared within the
// rdma package; do not redefine it here.)

// ----- Outgoing request side: L2 / PwPort -> remote chiplet -----

// processReqFromL2 overrides the base method to route PTE-caching PTW reads
// (ReadReq with IsPtw=true) onto the PTW network.
func (e *DataIdealRdmaEngine) processReqFromL2(now akita.VTimeInSec) bool {
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

func (e *DataIdealRdmaEngine) processReqFromL2Aux(
	now akita.VTimeInSec,
	req protocol.AccessReq,
	recvPort akita.Port,
) bool {
	if e.outstandingReqs >= maxOutstandingReqs {
		if global.GetInstance().GetVerbose() {
			fmt.Println("Too many outstanding reqs", e.Name())
		}
		return false
	}

	usePtw := false
	if r, ok := req.(*protocol.ReadReq); ok && r.IsPtw {
		usePtw = true
	}

	reqPort := e.RequestPort
	addrTable := e.RdmaReqAddressTable
	if usePtw {
		reqPort = e.PtwReqPort
		addrTable = e.PtwRdmaReqAddressTable
	}

	dst := addrTable.Find(req.GetAddress())

	cloned := e.cloneReq(req)
	cloned.Meta().Src = reqPort
	cloned.Meta().Dst = dst
	cloned.Meta().SendTime = now

	// Preserve IsPtw on the cloned ReadReq so the receiver can identify
	// the transaction as PTW when sending the response back. The base
	// cloneReq() does not copy IsPtw, which would otherwise misroute
	// the response onto the data mesh.
	if usePtw {
		if c, ok := cloned.(*protocol.ReadReq); ok {
			c.IsPtw = true
		}
	}

	networkMsg := &protocol.NetworkMsg{
		Msg:     cloned,
		SrcPort: reqPort,
	}

	err := reqPort.Send(networkMsg)
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

		srcName := e.Name()
		dstName := dst.Name()
		srcChip := strings.Split(srcName, ".")[1]
		dstChip := strings.Split(dstName, ".")[1]

		if usePtw {
			// tracing.StartTask(
			// cloned.Meta().ID,
			// "",
			// now,
			// e,
			// "pte-caching-remote-ptw-trace",
			// "",
			// srcChip+"."+dstChip)
		} else {
			tracing.StartTask(
				cloned.Meta().ID,
				0,
				now,
				e,
				"hop-trace",
				"",
				srcChip+"."+dstChip)
		}

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

// processReqFromPwPort overrides the base method to route MMU page-walker
// PTW requests (TransReadReq) onto the PTW network. In current production
// code this PwPort path is not exercised, but routing PTW consistently
// keeps the engine correct should it ever fire.
func (e *DataIdealRdmaEngine) processReqFromPwPort(
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

func (e *DataIdealRdmaEngine) processReqFromPwPortAux(
	now akita.VTimeInSec,
	req *protocol.ReadReq,
) bool {
	if e.outstandingReqs >= maxOutstandingReqs {
		if global.GetInstance().GetVerbose() {
			fmt.Println("Too many outstanding reqs", e.Name())
		}
		return false
	}

	// MMU page-walker traffic is always PTW.
	reqPort := e.PtwReqPort
	addrTable := e.PtwRdmaReqAddressTable

	dst := addrTable.Find(req.GetAddress())

	cloned := protocol.TransReadReqBuilder{}.
		WithSendTime(now).
		WithSrc(reqPort).
		WithDst(dst).
		WithPID(req.PID).
		WithAddress(req.GetAddress()).
		WithByteSize(8).
		WithInfo(req.Info).
		Build()

	networkMsg := &protocol.NetworkMsg{
		Msg:     cloned,
		SrcPort: reqPort,
	}

	err := reqPort.Send(networkMsg)
	if err == nil {
		e.PwPort.Retrieve(now)

		tracing.TraceReqReceive(req, now, e)
		tracing.TraceReqInitiate(cloned, now, e,
			tracing.MsgIDAtReceiver(req, e))

		srcName := req.Meta().Src.Name()
		dstName := dst.Name()
		srcChip := strings.Split(srcName, ".")[1]
		dstChip := strings.Split(dstName, ".")[1]

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

// ----- Incoming request/response side: PTW network -----

// processPtwReqFromOutside drains the parallel PTW request port using a
// ready-aware peek so a head-of-line PTW request whose per-bank downstream
// (ToDramTops[i] for TransReadReq, ToL2RDMAs[i] for PTE-caching AccessReq)
// is backpressured does not block other in-flight requests targeting
// different banks.
func (e *DataIdealRdmaEngine) processPtwReqFromOutside(
	now akita.VTimeInSec,
) bool {
	madeProgress := false
	port := e.PtwReqPort.(*akita.LimitNumMsgPort)
	for i := 0; i < e.numTransPerCycle; i++ {
		req, idx, ok := port.PeekReady(e.canRoutePtwReqFromOutside)
		if !ok {
			return madeProgress
		}

		switch req := req.(type) {
		case *protocol.TransReadReq:
			madeProgress = e.processPtwTransReqFromOutside(now, req, idx) || madeProgress
		case protocol.AccessReq:
			madeProgress = e.processPtwDataReqFromOutside(now, req, idx) || madeProgress
		default:
			log.Panicf("cannot process request of type %s", reflect.TypeOf(req))
			return false
		}
	}
	return madeProgress
}

// canRoutePtwReqFromOutside reports whether the downstream port that would
// carry this incoming PTW request (per-bank ToDramTops[i] for translation
// reads, per-bank ToL2RDMAs[i] for PTE-caching reads) currently has buffer
// room. Mirrors the base canRouteReqFromOutside.
func (e *DataIdealRdmaEngine) canRoutePtwReqFromOutside(m akita.Msg) bool {
	switch req := m.(type) {
	case *protocol.TransReadReq:
		dst := e.translationModuleFinder.Find(req.GetAddress())
		return dst.(canSender).CanSend(1)
	case protocol.AccessReq:
		dst := e.localModuleFinder.Find(req.GetAddress())
		return dst.(canSender).CanSend(1)
	}
	return false
}

// processPtwTransReqFromOutside mirrors processTransReqFromOutside in the
// base engine but Retrieves from PtwReqPort.
func (e *DataIdealRdmaEngine) processPtwTransReqFromOutside(
	now akita.VTimeInSec,
	req *protocol.TransReadReq,
	idx int,
) bool {
	dst := e.translationModuleFinder.Find(req.GetAddress())
	src := e.srcPortForDramDst(dst)

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
		e.PtwReqPort.(*akita.LimitNumMsgPort).RetrieveAt(now, idx)

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
		fmt.Println("Error on processPtwTransReqFromOutside, ",
			e.Name(),
			cloned.Meta().Src.Name(),
			cloned.Meta().Dst.Name(),
			req.Meta().ID,
		)
	}
	return false
}

// processPtwDataReqFromOutside mirrors processDataReqFromOutside but
// Retrieves from PtwReqPort. Handles PTE-caching PTW reads that arrive as
// regular AccessReq with IsPtw=true.
func (e *DataIdealRdmaEngine) processPtwDataReqFromOutside(
	now akita.VTimeInSec,
	req protocol.AccessReq,
	idx int,
) bool {
	dst := e.localModuleFinder.Find(req.GetAddress())
	src := e.srcPortForL2Dst(dst)

	cloned := e.cloneReq(req)
	cloned.Meta().SendTime = now
	cloned.Meta().Src = src
	cloned.Meta().Dst = dst

	err := src.Send(cloned)
	if err == nil {
		e.PtwReqPort.(*akita.LimitNumMsgPort).RetrieveAt(now, idx)

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
		fmt.Println("Error on processPtwDataReqFromOutside, ",
			e.Name(),
			cloned.Meta().Src.Name(),
			cloned.Meta().Dst.Name(),
			req.Meta().ID,
		)
	}
	return false
}

// processPtwRspFromOutside drains the parallel PTW response port using a
// ready-aware peek (PwPort for TransDataReadyRsp, per-bank ToL2Bottoms[i]
// for AccessRsp) so a head-of-line response cannot block other responses
// whose destinations are still drainable.
func (e *DataIdealRdmaEngine) processPtwRspFromOutside(
	now akita.VTimeInSec,
) bool {
	madeProgress := false
	port := e.PtwRspPort.(*akita.LimitNumMsgPort)
	for i := 0; i < e.numTransPerCycle; i++ {
		rsp, idx, ok := port.PeekReady(e.canRoutePtwRspFromOutside)
		if !ok {
			return madeProgress
		}

		switch rsp := rsp.(type) {
		case *protocol.TransDataReadyRsp:
			madeProgress = e.processPtwTransRspFromOutside(now, rsp, idx) || madeProgress
		case protocol.AccessRsp:
			madeProgress = e.processPtwAccessRspFromOutside(now, rsp, idx) || madeProgress
		default:
			log.Panicf("cannot process response of type %s", reflect.TypeOf(rsp))
			return false
		}
	}
	return madeProgress
}

// canRoutePtwRspFromOutside reports whether the inbound port that will carry
// this PTW response into the chiplet (PwPort for translation responses, the
// per-bank ToL2Bottoms[i] resolved from the original requester for PTE-
// caching access responses) has buffer room.
func (e *DataIdealRdmaEngine) canRoutePtwRspFromOutside(m akita.Msg) bool {
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

func (e *DataIdealRdmaEngine) processPtwTransRspFromOutside(
	now akita.VTimeInSec,
	rsp *protocol.TransDataReadyRsp,
	idx int,
) bool {
	transactionIndex := e.findTransactionByRspToID(
		rsp.GetRespondTo(), e.transactionsFromInside)
	trans := e.transactionsFromInside[transactionIndex]

	src := e.PwPort
	dst := trans.fromInside.Meta().Src

	transRspToInside := protocol.DataReadyRspBuilder{}.
		WithSendTime(rsp.SendTime).
		WithSrc(src).
		WithDst(dst).
		WithRspTo(trans.fromInside.Meta().ID).
		WithData(rsp.Data).
		WithInfo(rsp.Info).
		Build()

	transRspToInside.Meta().Src = src
	transRspToInside.Meta().Dst = dst
	transRspToInside.Meta().SendTime = now

	err := src.Send(transRspToInside)
	if err == nil {
		e.PtwRspPort.(*akita.LimitNumMsgPort).RetrieveAt(now, idx)

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
		fmt.Println("Error on processPtwTransRspFromOutside, ",
			e.Name(),
			transRspToInside.Meta().Src.Name(),
			transRspToInside.Meta().Dst.Name(),
			rsp.Meta().ID,
		)
	}
	return false
}

func (e *DataIdealRdmaEngine) processPtwAccessRspFromOutside(
	now akita.VTimeInSec,
	rsp protocol.AccessRsp,
	idx int,
) bool {
	transactionIndex := e.findTransactionByRspToID(
		rsp.GetRespondTo(), e.transactionsFromInside)
	trans := e.transactionsFromInside[transactionIndex]

	src := e.srcPortForL2BottomDst(trans.fromInside.Meta().Src)
	rspToInside := e.cloneRsp(rsp, trans.fromInside.Meta().ID)
	rspToInside.Meta().SendTime = now
	rspToInside.Meta().Src = src
	rspToInside.Meta().Dst = trans.fromInside.Meta().Src

	err := src.Send(rspToInside)
	if err == nil {
		e.PtwRspPort.(*akita.LimitNumMsgPort).RetrieveAt(now, idx)

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
		fmt.Println("Error on processPtwAccessRspFromOutside, ",
			e.Name(),
			rspToInside.Meta().Src.Name(),
			rspToInside.Meta().Dst.Name(),
			rsp.Meta().ID,
		)
	}
	return false
}

// ----- Outgoing response side: DRAM / L2 -> remote chiplet -----
//
// These override the base engine handlers so PTW transactions (identified
// from trans.fromOutside) reply over PtwRspPort instead of ResponsePort.

func (e *DataIdealRdmaEngine) processRspFromDram(now akita.VTimeInSec) bool {
	madeProgress := false
	port := e.ToDramTop
	for i := 0; i < e.numTransPerCycle; i++ {
		rsp := port.Peek()
		if rsp == nil {
			continue
		}

		switch rsp := rsp.(type) {
		case *protocol.DataReadyRsp:
			if e.processTransRspFromDramPtw(now, rsp, port) {
				madeProgress = true
			}
		case *protocol.WriteDoneRsp:
			if e.processWriteDoneRspFromDramPtw(now, rsp, port) {
				madeProgress = true
			}
		default:
			panic("unknown rsp type")
		}
	}
	return madeProgress
}

func (e *DataIdealRdmaEngine) processTransRspFromDramPtw(
	now akita.VTimeInSec,
	rsp *protocol.DataReadyRsp,
	recvPort akita.Port,
) bool {
	transactionIndex := e.findTransactionByRspToID(
		rsp.GetRespondTo(), e.transactionsFromOutside)
	trans := e.transactionsFromOutside[transactionIndex]

	dst := e.RdmaRspAddressTable[trans.fromOutside.Meta().Src]

	// TransReadReq from outside is always PTW.
	rspPort := e.PtwRspPort

	cloned := protocol.TransDataReadyRspBuilder{}.
		WithSendTime(now).
		WithSrc(rsp.Src).
		WithDst(rsp.Dst).
		WithRspTo(trans.fromOutside.Meta().ID).
		WithData(rsp.Data).
		WithInfo(rsp.Info).
		Build()

	cloned.Meta().Src = rspPort
	cloned.Meta().Dst = dst
	cloned.Meta().SendTime = now

	networkMsg := &protocol.NetworkMsg{
		Msg:     cloned,
		SrcPort: rspPort,
	}

	err := rspPort.Send(networkMsg)
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
		fmt.Println("Error on processTransRspFromDramPtw, ",
			e.Name(),
			cloned.Meta().Src.Name(),
			cloned.Meta().Dst.Name(),
			rsp.Meta().ID,
		)
	}
	return false
}

func (e *DataIdealRdmaEngine) processWriteDoneRspFromDramPtw(
	now akita.VTimeInSec,
	rsp *protocol.WriteDoneRsp,
	recvPort akita.Port,
) bool {
	transactionIndex := e.findTransactionByRspToID(
		rsp.GetRespondTo(), e.transactionsFromOutside)
	trans := e.transactionsFromOutside[transactionIndex]

	rspPort := e.ResponsePort
	if transWasPtw(trans) {
		rspPort = e.PtwRspPort
	}

	rspToOutside := e.cloneRsp(rsp, trans.fromOutside.Meta().ID)
	rspToOutside.Meta().SendTime = now
	rspToOutside.Meta().Src = rspPort
	rspToOutside.Meta().Dst = e.RdmaRspAddressTable[trans.fromOutside.Meta().Src]

	networkMsg := &protocol.NetworkMsg{
		Msg:     rspToOutside,
		SrcPort: rspPort,
	}

	err := rspPort.Send(networkMsg)
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
		fmt.Println("Error on processWriteDoneRspFromDramPtw, ",
			e.Name(),
			rspToOutside.Meta().Src.Name(),
			rspToOutside.Meta().Dst.Name(),
			rsp.Meta().ID,
		)
	}
	return false
}

func (e *DataIdealRdmaEngine) processRspFromL2(now akita.VTimeInSec) bool {
	madeProgress := false
	port := e.ToL2RDMA
	for i := 0; i < e.numTransPerCycle; i++ {
		req := port.Peek()
		if req == nil {
			continue
		}

		switch msg := req.(type) {
		case protocol.AccessRsp:
			if e.processRspFromL2AuxPtw(now, msg, port) {
				madeProgress = true
			}
		default:
			panic("unknown req type")
		}
	}
	return madeProgress
}

func (e *DataIdealRdmaEngine) processRspFromL2AuxPtw(
	now akita.VTimeInSec,
	rsp protocol.AccessRsp,
	recvPort akita.Port,
) bool {
	transactionIndex := e.findTransactionByRspToID(
		rsp.GetRespondTo(), e.transactionsFromOutside)
	trans := e.transactionsFromOutside[transactionIndex]

	rspPort := e.ResponsePort
	if transWasPtw(trans) {
		rspPort = e.PtwRspPort
	}

	rspToOutside := e.cloneRsp(rsp, trans.fromOutside.Meta().ID)
	rspToOutside.Meta().SendTime = now
	rspToOutside.Meta().Src = rspPort
	rspToOutside.Meta().Dst = e.RdmaRspAddressTable[trans.fromOutside.Meta().Src]

	networkMsg := &protocol.NetworkMsg{
		Msg:     rspToOutside,
		SrcPort: rspPort,
	}

	err := rspPort.Send(networkMsg)
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
		fmt.Println("Error on processRspFromL2AuxPtw, ",
			e.Name(),
			rspToOutside.Meta().Src.Name(),
			rspToOutside.Meta().Dst.Name(),
			rsp.Meta().ID,
		)
	}

	return false
}

// SetPtwRdmaReqAddressTable installs the PTW-only address table whose
// LowModules are the destination chiplets' PtwReqPort ports.
func (e *DataIdealRdmaEngine) SetPtwRdmaReqAddressTable(t lmf.LowModuleFinder) {
	e.PtwRdmaReqAddressTable = t
}

// GetPtwReqPort returns the PTW request port. Used by builders/ptideal.go
// to plug each chiplet's PTW ports into the parallel ideal NoC.
func (e *DataIdealRdmaEngine) GetPtwReqPort() akita.Port {
	return e.PtwReqPort
}

// GetPtwRspPort returns the PTW response port.
func (e *DataIdealRdmaEngine) GetPtwRspPort() akita.Port {
	return e.PtwRspPort
}

// NewDataIdealRdmaEngine constructs a DataIdealRdmaEngine with the same port
// inventory as RDMAEngineImpl plus the parallel PTW request/response ports.
func NewDataIdealRdmaEngine(
	name string,
	engine akita.Engine,
	numTransPerCycle int,
) RDMAEngine {
	e := new(DataIdealRdmaEngine)
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

	e.RequestPort = akita.NewLimitNumMsgPort(e, 4*numTransPerCycle, name+".RequestPort")
	e.ResponsePort = akita.NewLimitNumMsgPort(e, 4*numTransPerCycle, name+".ResponsePort")

	e.PtwReqPort = akita.NewLimitNumMsgPort(e, 4*numTransPerCycle, name+".PtwRequestPort")
	e.PtwRspPort = akita.NewLimitNumMsgPort(e, 4*numTransPerCycle, name+".PtwResponsePort")

	e.numTransPerCycle = numTransPerCycle

	e.retryPort = akita.NewLimitNumMsgPort(e, 100000000, name+".RetryPort")

	return e
}
