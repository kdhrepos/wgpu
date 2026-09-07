// Package rob implemented an reorder buffer for memory requests.
package rob

import (
	"container/list"
	"fmt"

	"gitlab.com/akita/akita"
	"gitlab.com/akita/protocol"
	"gitlab.com/akita/util/tracing"
)

type transaction struct {
	reqFromTop    protocol.AccessReq
	reqToBottom   protocol.AccessReq
	rspFromBottom protocol.AccessRsp

	// timestamps for the deschedule-independent latency split:
	//   entryWait    = topDown time - req.SendTime (waited at ROB.TopPort
	//                  for room; ROB-full backpressure)
	//   sentToBottom = topDown forwarded the req downstream
	//   rspArrive    = parseBottom received the response
	// downstream round-trip = rspArrive - sentToBottom (AT+L1V+L2+net+DRAM)
	// in-order ROB hold     = release - rspArrive (waiting to reach FIFO head)
	entryWait    akita.VTimeInSec
	sentToBottom akita.VTimeInSec
	rspArrive    akita.VTimeInSec

	// wfID is the issuing wavefront (from the request). blockedCrossWf is set
	// when this transaction's response arrived while a DIFFERENT wavefront's
	// transaction occupied the FIFO head — i.e. its hold time is head-of-line
	// blocking that a per-wavefront ROB split would eliminate.
	wfID           string
	blockedCrossWf bool
}

// ReorderBuffer can maintain the returning order of memory transactions.
type ReorderBuffer struct {
	*akita.TickingComponent

	TopPort     akita.Port
	BottomPort  akita.Port
	ControlPort akita.Port

	BottomUnit akita.Port

	bufferSize     int
	numReqPerCycle int

	toBottomReqIDToTransactionTable map[uint64]*list.Element
	transactions                    *list.List
	isFlushing                      bool

	// outOfOrder releases responses in arrival order rather than strict FIFO.
	// Only the vector-memory ROB sets this; the scalar and instruction CU
	// handlers still assume in-order arrival (InFlight...[0]), so their ROBs
	// must stay in-order or they leak un-matched responses → s_waitcnt hang
	// (unless perWfSubFifo is also set: that path preserves per-WF order).
	outOfOrder bool

	// perWfSubFifo splits transactions into one in-order list per wavefront ID.
	// Each sub-FIFO drains in program order (scalar/inst handler stays correct),
	// but sub-FIFOs are independent — slow head in WF-A no longer blocks WF-B.
	// When set, `transactions` is unused; bookkeeping lives in transactionsByWf
	// + wfOrder + totalLen.
	perWfSubFifo     bool
	transactionsByWf map[string]*list.List
	wfOrder          []string // insertion order of distinct wfIDs (stale entries skipped)
	wfCursor         int      // round-robin scan start position
	totalLen         int      // aggregate count across sub-FIFOs (for isFull)

	// Per-transaction latency split (deschedule-independent: stamped at
	// event time, not sampled per tick). Accumulated at retirement:
	//   DownstreamSum = Σ (rspArrive - sentToBottom)  memory round-trip
	//                   below the ROB (AT + L1V + L2 + network + DRAM)
	//   HoldSum       = Σ (release  - rspArrive)       in-order ROB hold:
	//                   data already back but waiting to reach the FIFO head
	//   RetireCount   = number of transactions retired
	// Hold/(Downstream+Hold) tells us how much of vmem latency is ROB
	// head-of-line blocking vs the actual memory round-trip.
	EntrySum      akita.VTimeInSec
	DownstreamSum akita.VTimeInSec
	HoldSum       akita.VTimeInSec
	RetireCount   uint64

	// HoldCrossWfSum / HoldIntraWfSum split HoldSum by whether the hold was
	// caused by a different wavefront occupying the head (cross) or the same
	// wavefront / no other-wf blocker (intra). HoldCrossWf/(Hold) estimates how
	// much rob_hold a per-wavefront ROB split would remove.
	HoldCrossWfSum akita.VTimeInSec
	HoldIntraWfSum akita.VTimeInSec

	// readyElems is a queue of elements in `transactions` whose rspFromBottom
	// has arrived. Used only when !perWfSubFifo && outOfOrder to avoid the
	// O(N) scan in bottomUp.
	readyElems     []*list.Element
	readyElemsHead int
}

// Tick updates the status of the ReorderBuffer.
func (b *ReorderBuffer) Tick(now akita.VTimeInSec) (madeProgress bool) {
	madeProgress = b.processControlMsg(now) || madeProgress

	if !b.isFlushing {
		madeProgress = b.runPipeline(now) || madeProgress
	}

	return madeProgress
}

// pendingLen returns the number of transactions currently held in the ROB.
func (b *ReorderBuffer) pendingLen() int {
	if b.perWfSubFifo {
		return b.totalLen
	}
	return b.transactions.Len()
}

// DeadlockSummary mirrors the per-component DeadlockSummary convention used
// by the rest of the simulator (cache, MMU, TLB, RDMA, switch, …) so the
// driver's checkQuiesceDeadlock can dump every component through one method
// name. Wraps PendingSummary().
func (b *ReorderBuffer) DeadlockSummary() (line string, anyPending bool) {
	pending, ready, awaiting := b.PendingSummary()
	topQ := b.TopPort != nil && b.TopPort.Peek() != nil
	bottomQ := b.BottomPort != nil && b.BottomPort.Peek() != nil
	ctrlQ := b.ControlPort != nil && b.ControlPort.Peek() != nil
	anyPending = pending > 0 || topQ || bottomQ || ctrlQ
	line = fmt.Sprintf(
		"pending=%d readyHeldAbove=%d awaitingBelow=%d top=%v bottom=%v ctrl=%v",
		pending, ready, awaiting, topQ, bottomQ, ctrlQ)
	return
}

// PendingSummary reports, for deadlock diagnosis: total held transactions,
// how many already have their downstream response in (readyHeld — the stall is
// ABOVE the ROB: the top/CU side is not draining) vs still awaiting a
// downstream response (awaiting — the stall is BELOW: cache/L2/network/MMU).
func (b *ReorderBuffer) PendingSummary() (pending, readyHeld, awaiting int) {
	walk := func(l *list.List) {
		for e := l.Front(); e != nil; e = e.Next() {
			t := e.Value.(*transaction)
			pending++
			if t.rspFromBottom != nil {
				readyHeld++
			} else {
				awaiting++
			}
		}
	}
	if b.perWfSubFifo {
		for _, sub := range b.transactionsByWf {
			walk(sub)
		}
	} else {
		walk(b.transactions)
	}
	return
}

func (b *ReorderBuffer) processControlMsg(
	now akita.VTimeInSec,
) (madeProgress bool) {
	item := b.ControlPort.Peek()
	if item == nil {
		return false
	}

	msg := item.(*protocol.ControlMsg)
	if msg.DiscardTransations {
		return b.discardTransactions(now, msg)
	} else if msg.Restart {
		return b.restart(now, msg)
	}

	panic("never")
}

func (b *ReorderBuffer) discardTransactions(
	now akita.VTimeInSec,
	msg *protocol.ControlMsg,
) (madeProgress bool) {
	rsp := protocol.ControlMsgBuilder{}.
		WithSrc(b.ControlPort).
		WithDst(msg.Src).
		WithSendTime(now).
		ToNotifyDone().
		Build()

	err := b.ControlPort.Send(rsp)
	if err != nil {
		return false
	}

	b.isFlushing = true
	b.toBottomReqIDToTransactionTable = make(map[uint64]*list.Element)
	b.transactions.Init()
	b.readyElems = b.readyElems[:0]
	b.readyElemsHead = 0
	if b.perWfSubFifo {
		b.transactionsByWf = make(map[string]*list.List)
		b.wfOrder = nil
		b.wfCursor = 0
		b.totalLen = 0
	}
	b.ControlPort.Retrieve(now)

	// fmt.Printf("%.10f, %s, rob flushed\n", now, b.Name())

	return true
}

func (b *ReorderBuffer) restart(
	now akita.VTimeInSec,
	msg *protocol.ControlMsg,
) (madeProgress bool) {
	rsp := protocol.ControlMsgBuilder{}.
		WithSrc(b.ControlPort).
		WithDst(msg.Src).
		WithSendTime(now).
		ToNotifyDone().
		Build()

	err := b.ControlPort.Send(rsp)
	if err != nil {
		return false
	}

	b.isFlushing = false
	b.toBottomReqIDToTransactionTable = make(map[uint64]*list.Element)
	b.transactions.Init()
	b.readyElems = b.readyElems[:0]
	b.readyElemsHead = 0
	if b.perWfSubFifo {
		b.transactionsByWf = make(map[string]*list.List)
		b.wfOrder = nil
		b.wfCursor = 0
		b.totalLen = 0
	}

	for b.TopPort.Retrieve(now) != nil {
	}

	for b.BottomPort.Retrieve(now) != nil {
	}

	b.ControlPort.Retrieve(now)

	// fmt.Printf("%.10f, %s, rob restarted\n", now, b.Name())

	return true
}

func (b *ReorderBuffer) runPipeline(now akita.VTimeInSec) (madeProgress bool) {
	for i := 0; i < b.numReqPerCycle; i++ {
		madeProgress = b.bottomUp(now) || madeProgress
	}

	for i := 0; i < b.numReqPerCycle; i++ {
		madeProgress = b.parseBottom(now) || madeProgress
	}

	for i := 0; i < b.numReqPerCycle; i++ {
		madeProgress = b.topDown(now) || madeProgress
	}

	return madeProgress
}

func (b *ReorderBuffer) topDown(now akita.VTimeInSec) bool {
	if b.isFull() {
		return false
	}

	item := b.TopPort.Peek()
	if item == nil {
		return false
	}

	req := item.(protocol.AccessReq)
	trans := b.createTransaction(req)

	trans.reqToBottom.Meta().Src = b.BottomPort
	trans.reqToBottom.Meta().SendTime = now
	err := b.BottomPort.Send(trans.reqToBottom)
	if err != nil {
		return false
	}

	trans.sentToBottom = now
	if st := req.Meta().SendTime; st != 0 {
		trans.entryWait = now - st
	}
	b.addTransaction(trans)
	b.TopPort.Retrieve(now)

	tracing.TraceReqReceive(req, now, b)
	tracing.TraceReqInitiate(trans.reqToBottom, now, b,
		tracing.MsgIDAtReceiver(req, b))

	return true
}

func (b *ReorderBuffer) parseBottom(now akita.VTimeInSec) bool {
	item := b.BottomPort.Peek()
	if item == nil {
		return false
	}

	rsp := item.(protocol.AccessRsp)
	rspTo := rsp.GetRespondTo()

	transElement, found := b.toBottomReqIDToTransactionTable[rspTo]

	if found {
		trans := transElement.Value.(*transaction)
		trans.rspFromBottom = rsp
		trans.rspArrive = now

		if !b.perWfSubFifo {
			if front := b.transactions.Front(); front != nil {
				headTrans := front.Value.(*transaction)
				if headTrans != trans && headTrans.wfID != trans.wfID {
					trans.blockedCrossWf = true
				}
			}
		}
		if b.outOfOrder {
			b.readyElems = append(b.readyElems, transElement)
		}

		tracing.TraceReqFinalize(trans.reqToBottom, now, b)
	}

	b.BottomPort.Retrieve(now)

	return true
}

func (b *ReorderBuffer) bottomUp(now akita.VTimeInSec) bool {
	// Out-of-order completion (vector path only): release the first
	// transaction whose response has returned, regardless of FIFO position.
	// This removes the in-order head-of-line blocking where a fast (L1-hit)
	// access waits for an older slow (remote) one. Correctness holds because
	// the CU vector handler matches responses by request ID and counts
	// per-instruction completion. Mirrors real GPU vmcnt (counter-based, OOO).
	//
	// The scalar/instruction ROBs keep strict FIFO (outOfOrder == false): their
	// CU handlers pop InFlight...[0] and assume in-order arrival, so releasing
	// out of order there would drop unmatched responses and hang on s_waitcnt.
	//
	// perWfSubFifo: round-robin across per-WF sub-FIFOs; within each sub-FIFO
	// retire by outOfOrder semantics. Each WF's program order is preserved when
	// outOfOrder=false, so scalar/inst handlers remain correct.
	var elem *list.Element
	var chosenSubIdx int = -1
	if b.outOfOrder {
		if b.readyElemsHead < len(b.readyElems) {
			// Peek only — do NOT consume the readyElems slot here. It is advanced
			// ONLY after TopPort.Send succeeds (below). Consuming up front and then
			// bailing on a Send failure orphans the element: it stays in the list
			// with rspFromBottom!=nil but is never re-enqueued (parseBottom appends
			// to readyElems only on NEW arrivals), so bottomUp would never retire
			// it -> permanent ROB stall under CU-port backpressure (deadlock).
			elem = b.readyElems[b.readyElemsHead]
		}
	} else if b.perWfSubFifo {
		n := len(b.wfOrder)
		for i := 0; i < n; i++ {
			idx := (b.wfCursor + i) % n
			sub := b.transactionsByWf[b.wfOrder[idx]]
			if sub == nil || sub.Len() == 0 {
				continue
			}
			front := sub.Front()
			if front.Value.(*transaction).rspFromBottom != nil {
				elem = front
				chosenSubIdx = idx
				break
			}
		}
	} else {
		elem = b.transactions.Front()
		if elem != nil && elem.Value.(*transaction).rspFromBottom == nil {
			elem = nil
		}
	}
	if elem == nil {
		return false
	}

	trans := elem.Value.(*transaction)

	rsp := b.duplicateRsp(trans.rspFromBottom, trans.reqFromTop.Meta().ID)
	rsp.Meta().Dst = trans.reqFromTop.Meta().Src
	rsp.Meta().Src = b.TopPort
	rsp.Meta().SendTime = now

	err := b.TopPort.Send(rsp)
	if err != nil {
		return false
	}

	// Send succeeded — NOW consume the readyElems slot (OOO path). Mirrors the
	// in-order paths, which likewise only drop the head after a successful send.
	if b.outOfOrder {
		b.readyElems[b.readyElemsHead] = nil
		b.readyElemsHead++
		if b.readyElemsHead == len(b.readyElems) {
			b.readyElems = b.readyElems[:0]
			b.readyElemsHead = 0
		}
	}

	if b.perWfSubFifo && chosenSubIdx >= 0 {
		b.wfCursor = (chosenSubIdx + 1) % len(b.wfOrder)
	}

	if trans.sentToBottom != 0 && trans.rspArrive != 0 {
		b.EntrySum += trans.entryWait
		b.DownstreamSum += trans.rspArrive - trans.sentToBottom
		holdTime := now - trans.rspArrive
		b.HoldSum += holdTime
		if trans.blockedCrossWf {
			b.HoldCrossWfSum += holdTime
		} else {
			b.HoldIntraWfSum += holdTime
		}
		b.RetireCount++
	}

	b.deleteTransaction(elem)

	tracing.TraceReqComplete(trans.reqFromTop, now, b)

	return true
}

func (b *ReorderBuffer) isFull() bool {
	if b.perWfSubFifo {
		return b.totalLen >= b.bufferSize
	}
	return b.transactions.Len() >= b.bufferSize
}

func (b *ReorderBuffer) createTransaction(req protocol.AccessReq) *transaction {
	return &transaction{
		reqFromTop:  req,
		reqToBottom: b.duplicateReq(req),
		wfID:        reqWfID(req),
	}
}

// reqWfID extracts the issuing wavefront ID tagged on a vector-memory request.
func reqWfID(req protocol.AccessReq) string {
	switch r := req.(type) {
	case *protocol.ReadReq:
		return r.WfID
	case *protocol.WriteReq:
		return r.WfID
	default:
		return ""
	}
}

func (b *ReorderBuffer) addTransaction(trans *transaction) {
	var elem *list.Element
	if b.perWfSubFifo {
		sub, ok := b.transactionsByWf[trans.wfID]
		if !ok {
			sub = list.New()
			b.transactionsByWf[trans.wfID] = sub
			b.wfOrder = append(b.wfOrder, trans.wfID)
		}
		elem = sub.PushBack(trans)
		b.totalLen++
	} else {
		elem = b.transactions.PushBack(trans)
	}
	b.toBottomReqIDToTransactionTable[trans.reqToBottom.Meta().ID] = elem
}

func (b *ReorderBuffer) deleteTransaction(elem *list.Element) {
	trans := elem.Value.(*transaction)
	if b.perWfSubFifo {
		if sub, ok := b.transactionsByWf[trans.wfID]; ok {
			sub.Remove(elem)
			if sub.Len() == 0 {
				// Prune the drained sub-FIFO so transactionsByWf/wfOrder stay
				// bounded by the number of *concurrently active* wavefronts.
				// wf.UID is unique per dispatch, so a fully-drained wavefront
				// never reappears under the same ID; leaving its empty list in
				// place would grow wfOrder to the total number of wavefronts
				// ever seen, turning the round-robin scan in bottomUp into
				// O(all wavefronts) — a progressive slowdown that looks like a
				// hang on large grids.
				delete(b.transactionsByWf, trans.wfID)
				b.removeFromWfOrder(trans.wfID)
			}
		}
		b.totalLen--
	} else {
		b.transactions.Remove(elem)
	}
	delete(b.toBottomReqIDToTransactionTable, trans.reqToBottom.Meta().ID)
}

// removeFromWfOrder drops wfID from the round-robin order slice while keeping
// wfCursor pointing at the same logical "next" sub-FIFO.
func (b *ReorderBuffer) removeFromWfOrder(wfID string) {
	for i, id := range b.wfOrder {
		if id == wfID {
			b.wfOrder = append(b.wfOrder[:i], b.wfOrder[i+1:]...)
			if b.wfCursor > i {
				b.wfCursor--
			}
			break
		}
	}
	if len(b.wfOrder) == 0 || b.wfCursor >= len(b.wfOrder) {
		b.wfCursor = 0
	}
}

func (b *ReorderBuffer) duplicateReq(req protocol.AccessReq) protocol.AccessReq {
	switch req := req.(type) {
	case *protocol.ReadReq:
		return b.duplicateReadReq(req)
	case *protocol.WriteReq:
		return b.duplicateWriteReq(req)
	default:
		panic("unsupported type")
	}
}

func (b *ReorderBuffer) duplicateReadReq(req *protocol.ReadReq) *protocol.ReadReq {
	return protocol.ReadReqBuilder{}.
		WithAddress(req.Address).
		WithByteSize(req.AccessByteSize).
		WithPID(req.PID).
		WithDst(b.BottomUnit).
		WithInfo(req.Info).
		Build()
}

func (b *ReorderBuffer) duplicateWriteReq(req *protocol.WriteReq) *protocol.WriteReq {
	return protocol.WriteReqBuilder{}.
		WithAddress(req.Address).
		WithPID(req.PID).
		WithData(req.Data).
		WithDirtyMask(req.DirtyMask).
		WithDst(b.BottomUnit).
		WithInfo(req.Info).
		Build()
}

func (b *ReorderBuffer) duplicateRsp(
	rsp protocol.AccessRsp,
	rspTo uint64,
) protocol.AccessRsp {
	switch rsp := rsp.(type) {
	case *protocol.DataReadyRsp:
		return b.duplicateDataReadyRsp(rsp, rspTo)
	case *protocol.WriteDoneRsp:
		return b.duplicateWriteDoneRsp(rsp, rspTo)
	default:
		panic("type not supported")
	}
}

func (b *ReorderBuffer) duplicateDataReadyRsp(
	rsp *protocol.DataReadyRsp,
	rspTo uint64,
) *protocol.DataReadyRsp {
	return protocol.DataReadyRspBuilder{}.
		WithData(rsp.Data).
		WithRspTo(rspTo).
		Build()
}

func (b *ReorderBuffer) duplicateWriteDoneRsp(
	rsp *protocol.WriteDoneRsp,
	rspTo uint64,
) *protocol.WriteDoneRsp {
	return protocol.WriteDoneRspBuilder{}.
		WithRspTo(rspTo).
		Build()
}
