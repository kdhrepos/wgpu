package writethrough

import (
	"gitlab.com/akita/akita"
	"gitlab.com/akita/protocol"
	"gitlab.com/akita/util/pipelining"
	"gitlab.com/akita/util/tracing"
)

type topParser struct {
	cache *Cache
}

func (p *topParser) Tick(now akita.VTimeInSec) bool {
	arbiter := p.cache.arbiter

	var req akita.Msg
	var pipeline pipelining.Pipeline
	if arbiter {
		req = p.cache.TopPort.Peek()
		pipeline = p.cache.pipelineForLocal
	} else {
		req = p.cache.RDMAPort.Peek()
		pipeline = p.cache.pipelineForRemote
	}

	if req == nil {
		if p.cache.RDMAPort.Peek() != nil ||
			p.cache.TopPort.Peek() != nil {
			return true
		}
		return false
	}

	// instead wrap in pipeline object and push into pipeline
	if !pipeline.CanAccept() {
		return false
	}

	trans := &transaction{}
	switch req := req.(type) {
	case *protocol.ReadReq:
		trans.read = req
	case *protocol.WriteReq:
		trans.write = req
	}

	// pipeline
	pipelineItem := cachePipelineItem{
		taskID: akita.GetIDGenerator().Generate(),
		trans:  trans,
	}
	pipeline.Accept(now, pipelineItem)

	p.cache.inFlightTransactions = append(p.cache.inFlightTransactions, trans)

	tracing.TraceReqReceive(req, now, p.cache)

	if arbiter {
		p.cache.TopPort.Retrieve(now)
	} else {
		p.cache.RDMAPort.Retrieve(now)
	}

	return true
}
