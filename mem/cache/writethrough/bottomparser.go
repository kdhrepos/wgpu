package writethrough

import (
	"gitlab.com/akita/akita"
	"gitlab.com/akita/mem/cache"
	"gitlab.com/akita/protocol"
	"gitlab.com/akita/util"
	"gitlab.com/akita/util/tracing"
)

type bottomParser struct {
	cache *Cache
}

func (p *bottomParser) Tick(now akita.VTimeInSec) bool {
	madeProgress := false

	madeProgress = p.processRspFromDram(now) || madeProgress
	madeProgress = p.processRspFromRdma(now) || madeProgress

	return madeProgress
}

func (p *bottomParser) processRspFromDram(now akita.VTimeInSec) bool {
	item := p.cache.BottomToDramPort.Peek()
	if item == nil {
		return false
	}

	switch rsp := item.(type) {
	case *protocol.WriteDoneRsp:
		return p.processWriteDoneRspFromDram(now, rsp)
	case *protocol.DataReadyRsp:
		return p.processDataReadyFromDram(now, rsp)
	default:
		panic("cannot process response")
	}
}

func (p *bottomParser) processWriteDoneRspFromDram(
	now akita.VTimeInSec,
	done *protocol.WriteDoneRsp,
) bool {
	trans := p.findTransactionByWriteToBottomID(done.GetRespondTo())
	// if the transaction is nil, it's partial line miss
	if trans == nil || trans.fetchAndWrite {
		p.cache.BottomToDramPort.Retrieve(now)
		return true
	}

	trans.done = true

	p.cache.BottomToDramPort.Retrieve(now)

	tracing.TraceReqFinalize(trans.writeToBottom, now, p.cache)
	tracing.EndTask(trans.id, now, p.cache)

	return true
}

func (p *bottomParser) processDataReadyFromDram(
	now akita.VTimeInSec,
	rsp *protocol.DataReadyRsp,
) bool {
	trans := p.findTransactionByReadToBottomID(rsp.GetRespondTo())
	if trans == nil {
		p.cache.BottomToDramPort.Retrieve(now)
		return true
	}
	pid := trans.readToBottom.PID
	bankBuf := p.getBankBuf(trans.block)
	if !bankBuf.CanPush() {
		return false
	}

	cachelineID, _ := cache.GetCacheLineID(trans.Address(), p.cache.log2BlockSize)
	data := rsp.Data
	dirtyMask := make([]bool, 1<<p.cache.log2BlockSize)
	mshrEntry := p.cache.mshr.Query(pid, cachelineID)
	p.mergeMSHRData(mshrEntry, data, dirtyMask)
	p.finalizeMSHRTrans(mshrEntry, data, now)
	tracing.StartTask(0, 0, now, p.cache,
		"mshr-merge", "dram", len(mshrEntry.Requests))
	p.cache.mshr.Remove(pid, cachelineID)

	trans.bankAction = bankActionWriteFetched
	trans.data = data
	trans.done = true
	trans.writeFetchedDirtyMask = dirtyMask
	bankBuf.Push(trans)

	p.cache.BottomToDramPort.Retrieve(now)

	tracing.TraceReqFinalize(trans.readToBottom, now, p.cache)

	return true
}

func (p *bottomParser) processRspFromRdma(now akita.VTimeInSec) bool {
	item := p.cache.BottomToRdmaPort.Peek()
	if item == nil {
		return false
	}

	switch rsp := item.(type) {
	case *protocol.WriteDoneRsp:
		return p.processWriteDoneRspFromRdma(now, rsp)
	case *protocol.DataReadyRsp:
		return p.processDataReadyFromRdma(now, rsp)
	default:
		panic("cannot process response")
	}
}

func (p *bottomParser) processWriteDoneRspFromRdma(
	now akita.VTimeInSec,
	done *protocol.WriteDoneRsp,
) bool {
	trans := p.findTransactionByWriteToBottomID(done.GetRespondTo())
	if trans == nil || trans.fetchAndWrite {
		p.cache.BottomToRdmaPort.Retrieve(now)
		return true
	}

	trans.done = true

	p.cache.BottomToRdmaPort.Retrieve(now)

	tracing.TraceReqFinalize(trans.writeToBottom, now, p.cache)
	tracing.EndTask(trans.id, now, p.cache)

	return true
}

func (p *bottomParser) processDataReadyFromRdma(
	now akita.VTimeInSec,
	rsp *protocol.DataReadyRsp,
) bool {
	trans := p.findTransactionByReadToBottomID(rsp.GetRespondTo())
	if trans == nil {
		p.cache.BottomToRdmaPort.Retrieve(now)
		return true
	}
	pid := trans.readToBottom.PID
	bankBuf := p.getBankBuf(trans.block)
	if !bankBuf.CanPush() {
		return false
	}

	cachelineID, _ := cache.GetCacheLineID(trans.Address(), p.cache.log2BlockSize)
	data := rsp.Data
	dirtyMask := make([]bool, 1<<p.cache.log2BlockSize)
	mshrEntry := p.cache.mshr.Query(pid, cachelineID)
	p.mergeMSHRData(mshrEntry, data, dirtyMask)
	p.finalizeMSHRTrans(mshrEntry, data, now)
	tracing.StartTask(0, 0, now, p.cache,
		"mshr-merge", "rdma", len(mshrEntry.Requests))

	p.cache.numToRemote--
	p.cache.mshr.Remove(pid, cachelineID)

	trans.bankAction = bankActionWriteFetched
	trans.data = data
	trans.done = true
	trans.writeFetchedDirtyMask = dirtyMask
	bankBuf.Push(trans)

	p.cache.BottomToRdmaPort.Retrieve(now)

	tracing.TraceReqFinalize(trans.readToBottom, now, p.cache)

	return true
}

func (p *bottomParser) mergeMSHRData(
	mshrEntry *cache.MSHREntry,
	data []byte,
	dirtyMask []bool,
) {
	for _, t := range mshrEntry.Requests {
		trans := t.(*transaction)

		if trans.write == nil {
			continue
		}

		write := trans.write
		offset := write.Address - mshrEntry.Block.Tag
		for i := 0; i < len(write.Data); i++ {
			if write.DirtyMask[i] {
				data[offset+uint64(i)] = write.Data[i]
				dirtyMask[offset+uint64(i)] = true
			}
		}
	}
}

func (p *bottomParser) finalizeMSHRTrans(
	mshrEntry *cache.MSHREntry,
	data []byte,
	now akita.VTimeInSec,
) {
	for _, t := range mshrEntry.Requests {
		trans := t.(*transaction)
		if trans.read != nil {
			read := trans.read
			offset := read.Address - mshrEntry.Block.Tag
			trans.data = data[offset : offset+read.AccessByteSize]
			trans.done = true
		} else {
			trans.done = true
		}

		tracing.EndTask(trans.id, now, p.cache)
	}
}

func (p *bottomParser) findTransactionByWriteToBottomID(
	id uint64,
) *transaction {
	for _, trans := range p.cache.inFlightTransactions {
		if trans.writeToBottom != nil && trans.writeToBottom.ID == id {
			return trans
		}
	}
	return nil
}

func (p *bottomParser) findTransactionByReadToBottomID(
	id uint64,
) *transaction {
	for _, trans := range p.cache.inFlightTransactions {
		if trans.readToBottom != nil && trans.readToBottom.ID == id {
			return trans
		}
	}
	return nil
	// panic("")
}

func (p *bottomParser) getBankBuf(block *cache.Block) util.Buffer {
	numWaysPerSet := p.cache.wayAssociativity
	blockID := block.SetID*numWaysPerSet + block.WayID
	bankID := blockID % len(p.cache.bankBufs)
	return p.cache.bankBufs[bankID]
}
