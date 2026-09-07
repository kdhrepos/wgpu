package writebackrcr

import (
	"gitlab.com/akita/akita"
	"gitlab.com/akita/mem/idealmemcontroller"
	"gitlab.com/akita/protocol"
	"gitlab.com/akita/util/tracing"
)

type topParser struct {
	cache  *Cache
	rrNext int // round-robin: source index to peek first this tick
}

// Three input sources, arbitrated round-robin: RDMAPort (external data),
// LookupPort (rcr lookup chain), TopPort (local CU traffic). Each feeds its
// own lookup buffer. rrNext rotates after every admitted request so no source
// can starve the others.
const (
	srcRDMA   = 0
	srcLookup = 1
	srcLocal  = 2
	srcCount  = 3
)

// Tick stamps trans.pipelineReadyCycle = now + pipelineLatency, pushes
// directly into the source-specific lookup buffer, and schedules one
// TickLater wake. Replaces the per-source pipelining shift registers.
func (p *topParser) Tick(now akita.VTimeInSec) bool {
	if p.cache.state != cacheStateRunning {
		return false
	}

	buf := p.cache.lookupBuf.(*LookupBuffer)
	// Admit the first request on a port whose class FITS the buffer's reserve,
	// scanning the whole port (PeekReady), not just its head. A home read
	// (toRemote=false) is the drain — it reads local DRAM and frees numToRemote —
	// and always fits (localReserve is kept free for it). A corner-serve read
	// (toRemote=true) is refused once the realFetch band is full. Both arrive on
	// the SAME port (RDMAPort), so head-only peeking lets a blocked corner-serve
	// read head-of-line block the home read queued behind it, stalling the drain
	// and deadlocking. PeekReady lets the home read bypass it. buildTransaction
	// has no side effect until stampAndAdmit, so building once to test the class
	// and again to admit is safe.
	admittable := func(req akita.Msg) bool {
		t := p.buildTransaction(req)
		return t != nil && buf.CanPushTransClass(t.toRemote, t.isCoalesce)
	}
	for i := 0; i < srcCount; i++ {
		src := (p.rrNext + i) % srcCount
		port := p.portForSrc(src).(*akita.LimitNumMsgPort)
		req, idx, ok := port.PeekReady(admittable)
		if !ok {
			continue
		}
		trans := p.buildTransaction(req)
		// Mark origin-CU (TopPort) traffic so emit{Read,Write}MemoryTrace fires.
		// Lookup-chain probes and RDMA home-serves are not fromLocal.
		trans.fromLocal = src == srcLocal
		p.stampAndAdmit(now, trans, buf)
		tracing.TraceReqReceive(req, now, p.cache)
		port.RetrieveAt(now, idx)
		p.rrNext = (src + 1) % srcCount
		return true
	}
	return false
}

func (p *topParser) portForSrc(src int) akita.Port {
	switch src {
	case srcLookup:
		return p.cache.LookupPort
	case srcLocal:
		return p.cache.TopPort
	}
	return p.cache.RDMAPort
}

func (p *topParser) buildTransaction(req akita.Msg) *transaction {
	trans := &transaction{}
	switch r := req.(type) {
	case *idealmemcontroller.ToDramMsg:
		trans.read = r.Msg.(*protocol.ReadReq)
		trans.toRemote = p.cache.isReqToRemote(trans.read.Address)
		trans.hopTraceID = trans.read.HopTraceID
	case *protocol.ReadReq:
		trans.read = r
		trans.toRemote = p.cache.isReqToRemote(r.Address)
		trans.hopTraceID = r.HopTraceID
	case *protocol.LookupReq:
		p.fillLookupTrans(trans, r)
	case *protocol.WriteReq:
		trans.write = r
		trans.toRemote = p.cache.isReqToRemote(r.Address)
	default:
		return nil
	}

	// Stamp the TRUE coalesce class once toRemote/address are known. An L-shaped
	// remote path (hasCornerNode) will delegate to a corner (writeBufferMshrCoalesce);
	// a straight-line remote path (incl. an incoming corner-serve lookup, whose
	// home is straight-line so hasCornerNode=false) does a real fetch. Local-home
	// work is never coalesce. This is what every downstream buffer's coalesce
	// reserve must key on — see transaction.isCoalesce.
	if trans.toRemote {
		trans.isCoalesce = p.cache.hasCornerNode(transAddr(trans))
	}
	return trans
}

// transAddr returns the request address carried by the transaction.
func transAddr(t *transaction) uint64 {
	if t.read != nil {
		return t.read.Address
	}
	return t.write.Address
}

func (p *topParser) fillLookupTrans(trans *transaction, req *protocol.LookupReq) {
	trans.read = protocol.ReadReqBuilder{}.
		WithSendTime(req.SendTime).
		WithSrc(req.Meta().Src).
		WithDst(req.Meta().Dst).
		WithAddress(req.Address).
		WithByteSize(req.AccessByteSize).
		WithInfo(req.Info).
		Build()
	trans.read.Meta().ID = req.Meta().ID
	trans.read.IsPtw = req.IsPtw      // carry PTW flag for data/PTE split in tracers
	trans.hopTraceID = req.HopTraceID // carry origin hop-trace task across corner
	trans.isLookup = true
	trans.hop = req.Hop + req.HopBase // true requestor-relative mesh-hop distance
	if req.CornerNodeId == p.cache.ChipletId {
		trans.isCornerNode = true
		trans.toRemote = true
	}
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
