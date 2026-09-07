package writeback

import (
	"gitlab.com/akita/akita"
	"gitlab.com/akita/mem/idealmemcontroller"
	"gitlab.com/akita/protocol"
	"gitlab.com/akita/util/tracing"
)

type topParser struct {
	cache *Cache

	// preferTop round-robins which ingress port is tried first each tick so
	// neither TopPort nor RDMAPort can starve the other at admission.
	preferTop bool
}

// Tick admits one request per call. Instead of feeding a shift register, it
// stamps trans.pipelineReadyCycle = now + pipelineLatency and pushes directly
// into the per-source lookup buffer; the directory stage gates by that
// deadline. A single TickLater(readyTime) wake replaces the per-cycle tick the
// shift register required, modelling tag-lookup latency as an event.
func (p *topParser) Tick(now akita.VTimeInSec) bool {
	if p.cache.state != cacheStateRunning {
		return false
	}

	// Round-robin the two ingress ports so neither starves the other, and
	// fall through to the other port when the first one's target buffer is
	// full or empty (no single-gate head-of-line block). Admission order does
	// NOT change processing order — each LookupBuffer re-sorts by (priority,
	// id), so the directory stage still serves local-home / low-id first
	// regardless of which port admitted when.
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

// tryAdmit pulls one request from port into buf when buf has room. Returns
// false (no progress) if the port is empty or its target buffer is full, so
// the caller can try the other port instead of stalling.
func (p *topParser) tryAdmit(
	now akita.VTimeInSec,
	port akita.Port,
	buf *LookupBuffer,
	fromLocal bool,
) bool {
	// PeekReady scans past head-of-line blocked messages. buildTransaction has
	// no tracing side effects here, so it's safe to call in the closure.
	admittable := func(req akita.Msg) bool {
		t := p.buildTransaction(req)
		return t != nil && buf.CanPushTransClass(t.toRemote)
	}
	req, idx, ok := port.(*akita.LimitNumMsgPort).PeekReady(admittable)
	if !ok {
		return false
	}
	trans := p.buildTransaction(req)
	p.stampAndAdmit(now, trans, buf)
	p.retrieveAndTrace(now, req, trans, fromLocal, idx)
	return true
}

// stampAndAdmit stamps the tag-lookup deadline, pushes the transaction into
// the lookup buffer, and schedules a single wake-up at the deadline.
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

func (p *topParser) buildTransaction(req akita.Msg) *transaction {
	trans := &transaction{}
	var addr uint64
	switch r := req.(type) {
	case *idealmemcontroller.ToDramMsg:
		trans.read = r.Msg.(*protocol.ReadReq)
		addr = trans.read.Address
	case *protocol.ReadReq:
		trans.read = r
		addr = trans.read.Address
	case *protocol.WriteReq:
		trans.write = r
		addr = trans.write.Address
	default:
		return nil
	}
	trans.toRemote = p.cache.isReqToRemote(addr)
	return trans
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
