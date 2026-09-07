package writeback_dynamic_dist

import (
	"gitlab.com/akita/akita"
	"gitlab.com/akita/mem/idealmemcontroller"
	"gitlab.com/akita/protocol"
	"gitlab.com/akita/util/tracing"
)

type topParser struct {
	cache *Cache

	// preferTop round-robins which ingress port is tried first each tick so
	// neither RDMAPort nor TopPort can starve the other at admission.
	preferTop bool
}

// Tick stamps trans.pipelineReadyCycle and pushes directly into the
// matching lookup buffer. A single TickLater wake replaces the per-cycle
// pipelining shift register. Mirrors writeback/topparser.go.
func (p *topParser) Tick(now akita.VTimeInSec) bool {
	if p.cache.state != cacheStateRunning {
		return false
	}

	// Round-robin the two ingress ports so neither starves the other, and fall
	// through to the other port when admission is refused. Both feed ONE
	// reserve-aware buffer: a remote-forward (toRemote=true) request is refused
	// once within localReserve of full, so local-home work always keeps room. The
	// buffer re-sorts by (toRemote-tier, id), so the directory stage still serves
	// local-home / low-id first regardless of admission order.
	buf := p.cache.lookupBuf.(*LookupBuffer)
	p.preferTop = !p.preferTop

	if p.preferTop {
		if p.tryAdmit(now, p.cache.TopPort, buf, true) {
			return true
		}
		return p.tryAdmit(now, p.cache.RDMAPort, buf, false)
	}
	if p.tryAdmit(now, p.cache.RDMAPort, buf, false) {
		return true
	}
	return p.tryAdmit(now, p.cache.TopPort, buf, true)
}

// tryAdmit pulls one request from port into buf when the request's class fits
// the buffer's reserve. Returns false (no progress) if the port is empty or
// admission is refused, so the caller can try the other port instead of
// stalling.
func (p *topParser) tryAdmit(
	now akita.VTimeInSec,
	port akita.Port,
	buf *LookupBuffer,
	fromLocal bool,
) bool {
	// PeekReady scans past head-of-line blocked messages (e.g. a remote request
	// that can't fit the reserve, blocking a local request behind it).
	// buildTransaction has a tracing side effect for LookupReq, so use reqAddr
	// for the admittability test to avoid emitting a task for refused requests.
	admittable := func(req akita.Msg) bool {
		_, isLookup := req.(*protocol.LookupReq)
		return buf.CanPushTransClass(
			p.cache.isReqToRemote(reqAddr(req)), isLookup)
	}
	req, idx, ok := port.(*akita.LimitNumMsgPort).PeekReady(admittable)
	if !ok {
		return false
	}

	trans := p.buildTransaction(now, req)
	p.stampAndAdmit(now, trans, buf)
	p.retrieveAndTrace(now, req, trans, fromLocal, idx)
	return true
}

// reqAddr extracts the target address from an ingress request without building
// (or tracing) a transaction, so admission can be tested cheaply.
func reqAddr(req akita.Msg) uint64 {
	switch r := req.(type) {
	case *idealmemcontroller.ToDramMsg:
		return r.Msg.(*protocol.ReadReq).Address
	case *protocol.ReadReq:
		return r.Address
	case *protocol.LookupReq:
		return r.Address
	case *protocol.WriteReq:
		return r.Address
	}
	return 0
}

func (p *topParser) buildTransaction(
	now akita.VTimeInSec, req akita.Msg,
) *transaction {
	trans := &transaction{}
	switch r := req.(type) {
	case *idealmemcontroller.ToDramMsg:
		trans.read = r.Msg.(*protocol.ReadReq)
		trans.toRemote = p.cache.isReqToRemote(trans.read.Address)
	case *protocol.ReadReq:
		trans.read = r
		trans.toRemote = p.cache.isReqToRemote(r.Address)
	case *protocol.LookupReq:
		p.fillLookupTrans(now, trans, r)
	case *protocol.WriteReq:
		trans.write = r
		trans.toRemote = p.cache.isReqToRemote(r.Address)
	default:
		panic("unknown request type")
	}
	return trans
}

func (p *topParser) fillLookupTrans(
	now akita.VTimeInSec,
	trans *transaction,
	req *protocol.LookupReq,
) {
	trans.read = protocol.ReadReqBuilder{}.
		WithSendTime(req.SendTime).
		WithSrc(req.Meta().Src).
		WithDst(req.Meta().Dst).
		WithAddress(req.Address).
		WithByteSize(req.AccessByteSize).
		WithInfo(req.Info).
		Build()
	trans.read.IsPtw = req.IsPtw
	trans.read.Meta().ID = req.Meta().ID
	trans.isLookup = true
	trans.peekOnly = req.PeekOnly
	trans.toRemote = p.cache.isReqToRemote(trans.read.Address)
	tracing.StartTask(
		0, 0, now, p.cache, "lookup", "", p.cache.ChipletId)
}

func (p *topParser) stampAndAdmit(
	now akita.VTimeInSec,
	trans *transaction,
	buf *LookupBuffer,
) {
	nowCycle := p.cache.Freq.Cycle(now)
	trans.pipelineReadyCycle = nowCycle + uint64(p.cache.pipelineLatency)
	buf.Push(cachePipelineItem{
		taskID: akita.GetIDGenerator().Generate(),
		trans:  trans,
	})
	p.cache.inFlightTransactions = append(p.cache.inFlightTransactions, trans)
	readyTime := p.cache.Freq.NCyclesLater(p.cache.pipelineLatency, now)
	p.cache.TickLater(readyTime)
}

func (p *topParser) retrieveAndTrace(
	now akita.VTimeInSec,
	req akita.Msg,
	trans *transaction,
	fromLocal bool,
	idx int,
) {
	tracing.TraceReqReceive(req, now, p.cache)
	if fromLocal {
		p.cache.TopPort.(*akita.LimitNumMsgPort).RetrieveAt(now, idx)
		trans.fromLocal = true
		return
	}
	p.cache.RDMAPort.(*akita.LimitNumMsgPort).RetrieveAt(now, idx)
}
