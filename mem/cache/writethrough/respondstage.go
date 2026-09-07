package writethrough

import (
	"strings"

	"gitlab.com/akita/akita"
	"gitlab.com/akita/protocol"
	"gitlab.com/akita/util/tracing"
)

type respondStage struct {
	cache *Cache
}

func (s *respondStage) Tick(now akita.VTimeInSec) bool {
	if len(s.cache.inFlightTransactions) == 0 {
		return false
	}

	for _, trans := range s.cache.inFlightTransactions {
		if !trans.done {
			continue
		}

		if trans.read != nil {
			return s.respondReadTrans(now, trans)
		}
		return s.respondWriteTrans(now, trans)
	}

	return false
}

func (s *respondStage) respondReadTrans(
	now akita.VTimeInSec,
	trans *transaction,
) bool {
	if !trans.done {
		return false
	}
	read := trans.read

	var srcPort akita.Port
	if strings.Contains(read.Meta().Src.Name(), "RDMA") {
		srcPort = s.cache.RDMAPort
	} else {
		srcPort = s.cache.TopPort
	}

	dr := protocol.DataReadyRspBuilder{}.
		WithSendTime(now).
		WithSrc(srcPort).
		WithDst(read.Src).
		WithRspTo(read.ID).
		WithData(trans.data).
		Build()

	err := srcPort.Send(dr)
	if err != nil {
		return false
	}

	s.removeTransaction(trans)

	tracing.TraceReqComplete(read, now, s.cache)

	return true
}

func (s *respondStage) respondWriteTrans(
	now akita.VTimeInSec,
	trans *transaction,
) bool {
	if !trans.done {
		return false
	}
	write := trans.write

	var srcPort akita.Port
	if strings.Contains(write.Meta().Src.Name(), "RDMA") {
		srcPort = s.cache.RDMAPort
	} else {
		srcPort = s.cache.TopPort
	}

	done := protocol.WriteDoneRspBuilder{}.
		WithSendTime(now).
		WithSrc(srcPort).
		WithDst(write.Src).
		WithRspTo(write.ID).
		Build()

	err := srcPort.Send(done)
	if err != nil {
		return false
	}

	s.removeTransaction(trans)

	tracing.TraceReqComplete(write, now, s.cache)

	return true
}

func (s *respondStage) removeTransaction(trans *transaction) {
	for i, t := range s.cache.inFlightTransactions {
		if t == trans {
			s.cache.inFlightTransactions = append(s.cache.inFlightTransactions[:i],
				s.cache.inFlightTransactions[i+1:]...)
			return
		}
	}

	panic("not found")
}
