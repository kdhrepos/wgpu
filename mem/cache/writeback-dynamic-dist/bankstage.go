package writeback_dynamic_dist

import (
	"strings"

	"gitlab.com/akita/akita"
	"gitlab.com/akita/protocol"
	"gitlab.com/akita/util/akitaext"
	"gitlab.com/akita/util/tracing"
)

type bankStage struct {
	cache   *Cache
	bankID  int
	latency int

	readyCycle   uint64
	currentTrans *transaction
}

func (s *bankStage) Reset(
	now akita.VTimeInSec,
) {
	s.cache.dirToBankBuffers[s.bankID].Clear()
	s.currentTrans = nil
}

// Tick replaces the old cycleLeft-- busy-wait with a deadline gate plus a
// single TickLater per access. Mirror of writeback/bankstage.go.
func (s *bankStage) Tick(
	now akita.VTimeInSec,
) bool {
	if s.currentTrans != nil {
		if s.cache.Freq.Cycle(now) < s.readyCycle {
			return false
		}
		return s.finalizeTrans(now)
	}
	return s.pullFromBuf(now)
}

func (s *bankStage) startAccess(now akita.VTimeInSec, t *transaction) {
	s.currentTrans = t
	s.readyCycle = s.cache.Freq.Cycle(now) + uint64(s.latency)
	s.cache.TickLater(s.cache.Freq.NCyclesLater(s.latency, now))
}

func (s *bankStage) pullFromBuf(now akita.VTimeInSec) bool {
	inBuf := s.cache.writeBufferToBankBuffers[s.bankID]
	trans := inBuf.Pop()
	if trans != nil {
		s.startAccess(now, trans.(*transaction))
		return true
	}

	// Peek the highest-priority dirToBank transaction (SmartBuffer is a
	// local-first heap). Only a writeBufferFetch needs the writeBufferBuffer
	// outbound queue, so gate ONLY that action on it — class-aware, so a remote
	// fetch (incl. a lookup-driven remote miss) can never starve a local one.
	// Read/write hits and evictions go straight to the bank and are NOT gated:
	// a local-home sink always drains even when writeBufferBuffer is full of
	// remote-bound fetches.
	dirBuf := s.cache.dirToBankBuffers[s.bankID]
	head := dirBuf.Peek()
	if head == nil {
		return false
	}
	t := head.(*transaction)
	if t.action == writeBufferFetch {
		if !s.cache.writeBufferBuffer.CanPushTransReserve(
			t.toRemote, smartBufferLocalReserve) {
			return false
		}
		dirBuf.Pop()
		s.cache.writeBufferBuffer.Push(t)
		return true
	}
	dirBuf.Pop()
	s.startAccess(now, t)
	return true
}

func (s *bankStage) finalizeTrans(
	now akita.VTimeInSec,
) bool {
	switch s.currentTrans.action {
	case bankReadHit:
		return s.finalizeReadHit(now)
	case bankWriteHit:
		return s.finalizeWriteHit(now)
	case bankWriteFetched:
		return s.finalizeBankWriteFetched(now)
	case bankEvictAndFetch, bankEvictAndWrite, bankEvict:
		return s.finalizeBankEviction(now)
	default:
		panic("bank action not supported")
	}
}

func (s *bankStage) finalizeReadHit(
	now akita.VTimeInSec,
) bool {
	read := s.currentTrans.read

	var srcPort akita.Port
	var sender akitaext.BufferedSender

	if strings.Contains(read.Meta().Src.Name(), "RDMA") {
		srcPort = s.cache.RDMAPort
		sender = s.cache.rdmaSender
	} else {
		srcPort = s.cache.TopPort
		sender = s.cache.topSender
	}

	if !sender.CanSend(1) {
		return false
	}

	addr := read.Address
	_, offset := GetCacheLineID(addr, s.cache.log2BlockSize)
	block := s.currentTrans.block

	data, err := s.cache.storage.Read(block.CacheAddress+offset, read.AccessByteSize)

	if err != nil {
		panic(err)
	}

	s.removeTransaction(s.currentTrans)

	s.currentTrans = nil
	block.ReadCount--

	var rspToOutside akita.Msg
	dataReadyRspBuilder := protocol.DataReadyRspBuilder{}.
		WithSendTime(now).
		WithSrc(srcPort).
		WithDst(read.Src).
		WithRspTo(read.ID).
		WithInfo(read.Info).
		WithData(data)

	rspToOutside = dataReadyRspBuilder.Build()

	sender.Send(rspToOutside)

	tracing.TraceReqComplete(read, now, s.cache)

	return true
}

func (s *bankStage) finalizeWriteHit(
	now akita.VTimeInSec,
) bool {
	write := s.currentTrans.write

	var srcPort akita.Port
	var sender akitaext.BufferedSender

	if strings.Contains(write.Meta().Src.Name(), "RDMA") {
		srcPort = s.cache.RDMAPort
		sender = s.cache.rdmaSender
	} else {
		srcPort = s.cache.TopPort
		sender = s.cache.topSender
	}

	if !sender.CanSend(1) {
		return false
	}

	addr := write.Address
	_, offset := GetCacheLineID(addr, s.cache.log2BlockSize)
	block := s.currentTrans.block

	data, err := s.cache.storage.Read(
		block.CacheAddress, 1<<s.cache.log2BlockSize)
	if err != nil {
		panic(err)
	}
	dirtyMask := block.DirtyMask
	if dirtyMask == nil {
		dirtyMask = make([]bool, 1<<s.cache.log2BlockSize)
	}
	for i := 0; i < len(write.Data); i++ {
		if write.DirtyMask == nil || write.DirtyMask[i] {
			index := offset + uint64(i)
			data[index] = write.Data[i]
			dirtyMask[index] = true
		}
	}
	err = s.cache.storage.Write(block.CacheAddress, data)
	if err != nil {
		panic(err)
	}

	block.IsValid = true
	block.IsLocked = false
	block.IsDirty = true
	block.DirtyMask = dirtyMask

	s.removeTransaction(s.currentTrans)

	s.currentTrans = nil

	done := protocol.WriteDoneRspBuilder{}.
		WithSendTime(now).
		WithSrc(srcPort).
		WithDst(write.Src).
		WithRspTo(write.ID).
		Build()

	sender.Send(done)

	tracing.TraceReqComplete(write, now, s.cache)

	return true
}

func (s *bankStage) finalizeBankWriteFetched(
	now akita.VTimeInSec,
) bool {
	if !s.cache.mshrStageBuffer.CanPush() {
		return false
	}

	mshrEntry := s.currentTrans.mshrEntry
	block := mshrEntry.Block
	s.cache.mshrStageBuffer.Push(mshrEntry) // wake up the mshr entry
	s.cache.storage.Write(block.CacheAddress, mshrEntry.Data)
	block.IsLocked = false
	block.IsValid = true

	s.currentTrans = nil

	return true
}

/*
- Eviction paths forward to write buffer.
- For eviction actions, the bank reads out the victim line and
forwards a new command to the write buffer, transforming the action as needed.
- This is the only finalize path that increases traffic to the write buffer.
*/
func (s *bankStage) finalizeBankEviction(
	now akita.VTimeInSec,
) bool {
	if !s.cache.writeBufferBuffer.CanPushTransReserve(
		s.currentTrans.toRemote, smartBufferLocalReserve) {
		return false
	}

	trans := s.currentTrans
	victim := trans.victim

	// if s.cache.isReqToRemote(trans.evictingAddr) {
	// 	if !s.cache.isRemoteEvictPossible() {
	// 		return true
	// 	}
	// 	s.cache.numRemoteEvictions++
	// }

	data, err := s.cache.storage.Read(
		victim.CacheAddress, 1<<s.cache.log2BlockSize)
	if err != nil {
		panic(err)
	}
	trans.evictingData = data

	switch trans.action {
	case bankEvict:
		trans.action = writeBufferFlush
	case bankEvictAndFetch:
		trans.action = writeBufferEvictAndFetch
	case bankEvictAndWrite:
		trans.action = writeBufferEvictAndWrite
	default:
		panic("unsupported action")
	}

	s.cache.writeBufferBuffer.Push(trans)

	s.currentTrans = nil

	return true
}

func (s *bankStage) removeTransaction(
	trans *transaction,
) {
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
