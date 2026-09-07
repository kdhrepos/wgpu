package writebacksac

import (
	"strings"

	"gitlab.com/akita/akita"
	"gitlab.com/akita/mem/cache"
	"gitlab.com/akita/protocol"
	"gitlab.com/akita/util/akitaext"
	"gitlab.com/akita/util/tracing"
)

type mshrStage struct {
	cache *Cache

	processingMSHREntry *cache.MSHREntry
}

func (s *mshrStage) Tick(now akita.VTimeInSec) bool {
	if s.processingMSHREntry != nil {
		return s.processOneReq(now)
	}

	item := s.cache.mshrStageBuffer.Pop()
	if item == nil {
		return false
	}

	s.processingMSHREntry = item.(*cache.MSHREntry)
	return s.processOneReq(now)
}

func (s *mshrStage) Reset(now akita.VTimeInSec) {
	s.processingMSHREntry = nil
	s.cache.mshrStageBuffer.Clear()
}

// processOneReq scans mshrEntry.Requests for the first request whose
// destination sender currently has room and processes that one. Strict
// head-only processing would let a backpressured topSender (or rdmaSender)
// block requests behind it bound for the *other* sender — which is fatal
// when an mshrEntry merges local (TopPort) and remote (RDMAPort) sources.
// PeekReady-style scanning preserves per-sender FIFO order while letting
// the other sender's responses progress, breaking the cross-sender HOL
// dependency cycle.
func (s *mshrStage) processOneReq(now akita.VTimeInSec) bool {
	mshrEntry := s.processingMSHREntry

	idx, ok := s.findReadyRequestIdx()
	if !ok {
		return false
	}

	trans := mshrEntry.Requests[idx].(*transaction)

	if s.findTransaction(trans) {
		s.removeTransaction(trans)
		if trans.read != nil {
			s.respondRead(now, trans.read, mshrEntry.Data)
		} else {
			s.respondWrite(now, trans.write)
		}
	}

	mshrEntry.Requests = append(
		mshrEntry.Requests[:idx], mshrEntry.Requests[idx+1:]...)
	if len(mshrEntry.Requests) == 0 {
		s.processingMSHREntry = nil
	}
	return true
}

// findReadyRequestIdx returns the position of the first request whose
// destination sender has room. The sender choice mirrors respondRead /
// respondWrite so the request that is selected here is guaranteed
// deliverable when it actually sends.
func (s *mshrStage) findReadyRequestIdx() (int, bool) {
	for i, req := range s.processingMSHREntry.Requests {
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
