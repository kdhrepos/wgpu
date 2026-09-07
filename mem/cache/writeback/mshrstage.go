package writeback

import (
	"strings"

	"gitlab.com/akita/akita"
	"gitlab.com/akita/mem/cache"
	"gitlab.com/akita/protocol"
	"gitlab.com/akita/util"
	"gitlab.com/akita/util/akitaext"
	"gitlab.com/akita/util/tracing"
)

type mshrStage struct {
	cache *Cache
}

// Tick serves one merged request per cycle. It scans mshrStageBuffer in FIFO
// order and picks the FIRST entry that has any request whose destination sender
// has room, then serves that one request. Skipping entries whose requests are
// ALL blocked is the entry-level analogue of the per-request ready scan: a
// coalesced entry bound entirely to a backpressured sender (e.g. all-RDMAPort)
// must not head-of-line-block a later entry bound to a sender that DOES have
// room (e.g. all-TopPort). Without this, mshrStageBuffer fills, the bottom
// stage's processDataReadyRsp stops draining (CanPush=false), and numToRemote
// never decrements — a deadlock. An entry is removed once its last request is
// served; a half-served entry simply stays and is re-scanned next tick.
func (s *mshrStage) Tick(now akita.VTimeInSec) bool {
	buf, ok := s.cache.mshrStageBuffer.(*util.BufferImpl)
	if !ok {
		return false
	}

	entries := buf.GetBuffer()
	for ei := range entries {
		entry := entries[ei].(*cache.MSHREntry)
		idx, found := s.findReadyRequestIdx(entry)
		if !found {
			continue // every request in this entry is sender-blocked; bypass it
		}

		s.serveOne(now, entry, idx)

		if len(entry.Requests) == 0 {
			buf.SetBuffer(append(entries[:ei:ei], entries[ei+1:]...))
		}
		return true
	}

	return false
}

func (s *mshrStage) Reset(now akita.VTimeInSec) {
	s.cache.mshrStageBuffer.Clear()
}

// serveOne fans out a single merged request and removes it from the entry.
func (s *mshrStage) serveOne(
	now akita.VTimeInSec,
	entry *cache.MSHREntry,
	idx int,
) {
	trans := entry.Requests[idx].(*transaction)

	if s.findTransaction(trans) {
		s.removeTransaction(trans)
		if trans.read != nil {
			s.respondRead(now, trans.read, entry.Data)
		} else {
			s.respondWrite(now, trans.write)
		}
	}

	entry.Requests = append(
		entry.Requests[:idx], entry.Requests[idx+1:]...)
}

// findReadyRequestIdx returns the position of the first request in entry whose
// destination sender has room. The sender choice mirrors respondRead /
// respondWrite so the selected request is guaranteed deliverable when sent.
func (s *mshrStage) findReadyRequestIdx(entry *cache.MSHREntry) (int, bool) {
	for i, req := range entry.Requests {
		trans := req.(*transaction)
		if s.senderFor(trans).CanSend(1) {
			return i, true
		}
	}
	return -1, false
}

func (s *mshrStage) senderFor(trans *transaction) akitaext.BufferedSender {
	var src akita.Port
	if trans.read != nil {
		src = trans.read.Meta().Src
	} else {
		src = trans.write.Meta().Src
	}
	if strings.Contains(src.Name(), "RDMA") {
		return s.cache.rdmaSender
	}
	return s.cache.topSender
}

func (s *mshrStage) respondRead(
	now akita.VTimeInSec,
	read *protocol.ReadReq,
	data []byte,
) {
	var srcPort akita.Port
	var sender akitaext.BufferedSender

	if strings.Contains(read.Meta().Src.Name(), "RDMA") {
		srcPort = s.cache.RDMAPort
		sender = s.cache.rdmaSender
	} else {
		srcPort = s.cache.TopPort
		sender = s.cache.topSender
	}

	_, offset := cache.GetCacheLineID(read.Address, s.cache.log2BlockSize)
	dataReadyRspBuilder := protocol.DataReadyRspBuilder{}.
		WithSendTime(now).
		WithSrc(srcPort).
		WithDst(read.Src).
		WithRspTo(read.ID).
		WithData(data[offset : offset+read.AccessByteSize])

	dataReady := dataReadyRspBuilder.Build()
	sender.Send(dataReady)

	tracing.TraceReqComplete(read, now, s.cache)
}

func (s *mshrStage) respondWrite(
	now akita.VTimeInSec,
	write *protocol.WriteReq,
) {
	var srcPort akita.Port
	var sender akitaext.BufferedSender

	if strings.Contains(write.Meta().Src.Name(), "RDMA") {
		srcPort = s.cache.RDMAPort
		sender = s.cache.rdmaSender
	} else {
		srcPort = s.cache.TopPort
		sender = s.cache.topSender
	}

	writeDoneRsp := protocol.WriteDoneRspBuilder{}.
		WithSendTime(now).
		WithSrc(srcPort).
		WithDst(write.Src).
		WithRspTo(write.ID).
		Build()

	sender.Send(writeDoneRsp)

	tracing.TraceReqComplete(write, now, s.cache)
}

func (s *mshrStage) removeTransaction(trans *transaction) {
	for i, t := range s.cache.inFlightTransactions {
		if trans == t {
			s.cache.inFlightTransactions = append(
				(s.cache.inFlightTransactions)[:i],
				(s.cache.inFlightTransactions)[i+1:]...)
			return
		}
	}
	panic("transaction not found")
}

func (s *mshrStage) findTransaction(trans *transaction) bool {
	for _, t := range s.cache.inFlightTransactions {
		if trans == t {
			return true
		}
	}
	return false
}
