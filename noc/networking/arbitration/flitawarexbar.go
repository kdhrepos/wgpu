package arbitration

import (
	"sort"

	"gitlab.com/akita/akita"
	"gitlab.com/akita/noc"
	"gitlab.com/akita/util"
)

// msgReservation tracks an in-flight wormhole packet that has claimed
// a downstream OutputBuf. The output VC is locked to this msg until
// all numFlitInMsg flits have passed through.
type msgReservation struct {
	msg          akita.Msg
	numFlitInMsg int
	flitsSent    int
}

// bufReservation models a single output VC under pure wormhole: at
// most one msg owns the OutputBuf at a time. Body/tail flits stream
// through credit-based (per-flit CanPush) while owner is set; new
// msgs wait until owner clears on tail.
type bufReservation struct {
	owner *msgReservation
}

func NewFlitAwareXBarArbiter(
	maxOutgoingReqPerBuffer int,
) *FlitAwareXBarArbiter {
	return &FlitAwareXBarArbiter{
		outBufReservations:       make(map[util.Buffer]*bufReservation),
		outgoingReqsPerBuffer:    make(map[util.Buffer]int),
		maxOutgoingReqsPerBuffer: maxOutgoingReqPerBuffer,
	}
}

type FlitAwareXBarArbiter struct {
	buffers                  []util.Buffer
	nextPortID               int
	outBufReservations       map[util.Buffer]*bufReservation
	outgoingReqsPerBuffer    map[util.Buffer]int
	maxOutgoingReqsPerBuffer int

	// vct enables Virtual Cut-Through: a NEW message may claim an output only
	// when the output buffer has room for the WHOLE message (NumFlitInMsg), so
	// a packet never partially occupies an output VC waiting for room. With XY
	// dimension-order routing this makes the network provably deadlock-free
	// (no partial-worm holds, acyclic channel dependency). Body/tail flits of
	// an already-reserved message still need only one slot. Off => wormhole.
	vct bool
}

// SetVCT toggles Virtual Cut-Through admission (see the vct field). Enabled by
// the mesh network only; other switch variants keep wormhole behavior.
func (a *FlitAwareXBarArbiter) SetVCT(v bool) { a.vct = v }

// wholeMsgBuffered reports whether every flit of buf's head message is already
// queued in buf. Flits of one message are contiguous from the front (wormhole
// FIFO on the input link), so counting consecutive same-message flits from the
// head yields how many have arrived. A message larger than buf can never be
// fully buffered, so it is clamped to buf capacity to avoid permanent
// starvation (degrades to wormhole for that oversized packet only).
func wholeMsgBuffered(buf util.Buffer, head *noc.Flit) bool {
	need := head.NumFlitInMsg
	if need > buf.Capacity() {
		need = buf.Capacity()
	}
	gb, ok := buf.(interface{ GetBuffer() []interface{} })
	if !ok {
		return true
	}
	count := 0
	for _, e := range gb.GetBuffer() {
		if e.(*noc.Flit).Msg != head.Msg {
			break
		}
		count++
		if count >= need {
			return true
		}
	}
	return count >= need
}

func (a *FlitAwareXBarArbiter) hasReservation(flit *noc.Flit) bool {
	r, ok := a.outBufReservations[flit.OutputBuf]
	if !ok || r.owner == nil {
		return false
	}
	return r.owner.msg == flit.Msg
}

// makeReservation claims the OutputBuf's VC for this flit's msg.
// Pure wormhole: succeeds only when no other msg currently owns the
// OutputBuf. The arbiter has already verified CanPush before calling
// here, so head has at least one slot ready.
func (a *FlitAwareXBarArbiter) makeReservation(flit *noc.Flit) bool {
	r, ok := a.outBufReservations[flit.OutputBuf]
	if !ok {
		r = &bufReservation{}
		a.outBufReservations[flit.OutputBuf] = r
	}
	if r.owner != nil {
		return false
	}
	r.owner = &msgReservation{
		msg:          flit.Msg,
		numFlitInMsg: flit.NumFlitInMsg,
	}
	return true
}

// markOneFlitSent records that one flit of the owning msg has just
// been pushed to its OutputBuf. When the tail flit (flitsSent ==
// numFlitInMsg) leaves, the reservation is released so the next msg
// may claim the VC.
func (a *FlitAwareXBarArbiter) markOneFlitSent(flit *noc.Flit) {
	r := a.outBufReservations[flit.OutputBuf]
	if r == nil || r.owner == nil || r.owner.msg != flit.Msg {
		panic("markOneFlitSent: no reservation for this msg")
	}
	r.owner.flitsSent++
	if r.owner.flitsSent == r.owner.numFlitInMsg {
		r.owner = nil
	}
}

func (a *FlitAwareXBarArbiter) AddBuffer(buf util.Buffer) {
	a.buffers = append(a.buffers, buf)
}

// Reset zeros the per-cycle outgoingReqsPerBuffer counters. Called
// once per Tick by the switch before iterating Arbitrate.
func (a *FlitAwareXBarArbiter) Reset() {
	for buf := range a.outgoingReqsPerBuffer {
		a.outgoingReqsPerBuffer[buf] = 0
	}
}

// arbitrateCandidate is a forwardBuffer whose head flit passed the
// per-cycle bandwidth + CanPush gates and is eligible for selection.
type arbitrateCandidate struct {
	buf      util.Buffer
	flit     *noc.Flit
	rrOrder int  // distance from nextPortID — lower = earlier in round-robin
	hasResv bool // body/tail of in-flight msg
}

// Arbitrate selects forwardBuffers that may push one flit each this
// call. Policy:
//  1. Body/tail flits of in-flight msgs go first — never block a held
//     output VC, otherwise wormhole drainage stalls.
//  2. Among new heads, larger NumFlitInMsg wins. Prevents single-flit
//     traffic from starving multi-flit packets at contended outputs.
//  3. Round-robin (rotated by nextPortID) breaks ties.
//
// One input per output port per call (occupiedOutputPort).
func (a *FlitAwareXBarArbiter) Arbitrate(now akita.VTimeInSec) []util.Buffer {
	// Buffers are registered lazily (mesh VOQ creates them on first route), so
	// the arbiter can be empty for the first ticks. Guard the round-robin
	// modulo below against division by zero.
	if len(a.buffers) == 0 {
		return nil
	}
	starting := a.nextPortID

	candidates := make([]arbitrateCandidate, 0, len(a.buffers))
	for i := 0; i < len(a.buffers); i++ {
		currPortID := (starting + i) % len(a.buffers)
		buf := a.buffers[currPortID]
		item := buf.Peek()
		if item == nil {
			continue
		}
		if a.outgoingReqsPerBuffer[buf] >= a.maxOutgoingReqsPerBuffer {
			continue
		}
		flit := item.(*noc.Flit)
		resv := a.hasReservation(flit)
		// VCT store-and-forward: a NEW message (no reservation yet) may start
		// forwarding only once ALL its flits are buffered in this input VOQ, so
		// the packet never spans a link while holding a downstream reservation.
		// With XY routing this makes the network provably deadlock-free. Body/
		// tail flits of an already-reserved message stream one slot at a time.
		if a.vct && !resv && !wholeMsgBuffered(buf, flit) {
			continue
		}
		if flit.OutputBuf.Capacity()-flit.OutputBuf.Size() < 1 {
			continue
		}
		candidates = append(candidates, arbitrateCandidate{
			buf:     buf,
			flit:    flit,
			rrOrder: i,
			hasResv: resv,
		})
	}

	// hasResv first guarantees wormhole drainage (deadlock-freedom). New
	// heads are then chosen by round-robin only. The former NumFlitInMsg
	// size-priority was removed: it starved 1-flit eviction-acks under the
	// multi-flit data-response flood, deadlocking the response network.
	sort.SliceStable(candidates, func(i, j int) bool {
		ci, cj := candidates[i], candidates[j]
		if ci.hasResv != cj.hasResv {
			return ci.hasResv
		}
		return ci.rrOrder < cj.rrOrder
	})

	occupiedOutputPort := make(map[util.Buffer]bool)
	selectedPort := make([]util.Buffer, 0, len(candidates))

	for _, c := range candidates {
		if occupiedOutputPort[c.flit.OutputBuf] {
			continue
		}
		if c.hasResv || a.makeReservation(c.flit) {
			a.markOneFlitSent(c.flit)
		} else {
			continue
		}
		selectedPort = append(selectedPort, c.buf)
		a.outgoingReqsPerBuffer[c.buf]++
		occupiedOutputPort[c.flit.OutputBuf] = true
	}

	a.nextPortID = (a.nextPortID + 1) % len(a.buffers)
	return selectedPort
}
