package writeback

import (
	"gitlab.com/akita/akita"
	"gitlab.com/akita/protocol"
)

type pageWalkCacheTopParser struct {
	cache *PageWalkCache
}

func (p *pageWalkCacheTopParser) Tick(now akita.VTimeInSec) bool {
	if p.cache.state != cacheStateRunning {
		return false
	}
	req := p.cache.TopPort.Peek()
	if req == nil {
		return false
	}

	if !p.cache.lookupBuffer.CanPush() {
		return false
	}

	trans := &transaction{}
	switch req := req.(type) {
	case *protocol.ReadReq:
		trans.read = req
	case *protocol.WriteReq:
		trans.write = req
	}
	pipelineItem := pwcPipelineItem{
		taskID: akita.GetIDGenerator().Generate(),
		trans:  trans,
	}
	p.cache.lookupBuffer.Push(pipelineItem)

	p.cache.inFlightTransactions = append(p.cache.inFlightTransactions, trans)
	// tracing.TraceReqReceive(req, now, p.cache)

	p.cache.TopPort.Retrieve(now)

	return true
}
