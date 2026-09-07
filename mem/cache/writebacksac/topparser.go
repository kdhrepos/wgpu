package writebacksac

import (
	"gitlab.com/akita/akita"
	"gitlab.com/akita/mem/cache"
	"gitlab.com/akita/mem/idealmemcontroller"
	"gitlab.com/akita/protocol"
	"gitlab.com/akita/util/global"
	"gitlab.com/akita/util/tracing"
)

type topParser struct {
	cache *Cache
}

// Tick admits one request per call. Instead of feeding a pipelining shift
// register, it stamps trans.pipelineReadyCycle = now + pipelineLatency and
// pushes directly into the lookup buffer; the directory stage gates by that
// deadline. A single TickLater(readyTime) wake replaces the per-cycle tick
// the old pipeline forced. See multiflitswitchconnection.go.Send for the
// mirror pattern on the inter-switch link.
//
// SAC: during PreFlushing only inbound (RDMAPort) requests are admitted.
// Draining a flush may require sibling caches to keep serving inbound
// requests (a draining cache's pass-through depends on its home cache),
// while local admission must stop so the drain terminates. Inbound lookups
// depend only on this chiplet's DRAM, so no cross-cache cycle remains.
func (p *topParser) Tick(now akita.VTimeInSec) bool {
	if p.cache.state != cacheStateRunning &&
		p.cache.state != cacheStatePreFlushing {
		return false
	}

	req, fromLocal := p.peekIncoming()
	if req == nil {
		return false
	}
	if p.cache.state == cacheStatePreFlushing && fromLocal {
		return false
	}

	buf := p.cache.lookupBufForRemote.(*LookupBuffer)
	if fromLocal {
		buf = p.cache.lookupBufForLocal.(*LookupBuffer)
	}
	if !buf.CanPush() {
		return false
	}

	trans := p.buildTransaction(req, fromLocal)
	if trans == nil {
		return false
	}
	p.stampAndAdmit(now, trans, buf, fromLocal)
	p.retrieveAndTrace(now, req, trans, fromLocal)
	return true
}

func (p *topParser) peekIncoming() (akita.Msg, bool) {
	if req := p.cache.RDMAPort.Peek(); req != nil {
		return req, false
	}
	if req := p.cache.TopPort.Peek(); req != nil {
		return req, true
	}
	return nil, false
}

func (p *topParser) buildTransaction(
	req akita.Msg,
	fromLocal bool,
) *transaction {
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

	s := global.GetInstance().SAC()
	if s.Enabled && p.cache.HomeNodeFinder != nil {
		p.applySAC(trans, addr, fromLocal, s)
	}

	return trans
}

// applySAC marks pass-through transactions according to the current LLC
// mode and feeds the profiling counters / CRD during the profiling window.
func (p *topParser) applySAC(
	trans *transaction,
	addr uint64,
	fromLocal bool,
	s *global.SACState,
) {
	c := p.cache
	home := int(c.HomeNodeFinder.GetChipletId(addr))

	if fromLocal {
		if s.Profiling {
			s.TotalReqs++
			if home == c.ChipletId {
				s.LocalReqs++
			}
			if n := s.NumBanksPerChiplet; n > 0 && c.BankId < n {
				s.SliceReqsMem[home*n+c.BankId]++
				s.SliceReqsSM[c.ChipletId*n+c.BankId]++
			}
		}

		if s.Mode == global.SACMemorySide && home != c.ChipletId {
			trans.isPassThrough = true
			return
		}

		if s.Profiling && home == c.ChipletId && c.crd != nil {
			lineID, _ := cache.GetCacheLineID(addr, c.log2BlockSize)
			c.crd.Access(lineID, c.ChipletId, c.log2BlockSize, s)
		}
		return
	}

	// Inbound request from a remote chiplet (via RDMA). Under SM-side the
	// home L2 is bypassed: the request passes through to local DRAM.
	if s.Mode == global.SACSMSide {
		trans.isPassThrough = true
		return
	}

	if s.Profiling && c.crd != nil {
		requester := -1
		if trans.read != nil {
			requester = trans.read.RequesterChiplet
		} else if trans.write != nil {
			requester = trans.write.RequesterChiplet
		}
		lineID, _ := cache.GetCacheLineID(addr, c.log2BlockSize)
		c.crd.Access(lineID, requester, c.log2BlockSize, s)
	}
}

func (p *topParser) stampAndAdmit(
	now akita.VTimeInSec,
	trans *transaction,
	buf *LookupBuffer,
	fromLocal bool,
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
	_ = fromLocal
}

func (p *topParser) retrieveAndTrace(
	now akita.VTimeInSec,
	req akita.Msg,
	trans *transaction,
	fromLocal bool,
) {
	tracing.TraceReqReceive(req, now, p.cache)
	if fromLocal {
		p.cache.TopPort.Retrieve(now)
		trans.fromLocal = true
		return
	}
	p.cache.RDMAPort.Retrieve(now)
}
