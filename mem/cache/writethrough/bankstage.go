package writethrough

import (
	"gitlab.com/akita/akita"
	"gitlab.com/akita/util/tracing"
)

// type bankTransaction struct {
// 	*transaction
// }

// func (t *bankTransaction) TaskID() string {
// 	return t.transaction.id
// }

type bankStage struct {
	cache   *Cache
	bankID  int
	latency int

	cycleLeft    int
	currentTrans *transaction
}

func (b *bankStage) Reset() {
	b.cache.bankBufs[b.bankID].Clear()
	b.currentTrans = nil
}

func (s *bankStage) Tick(now akita.VTimeInSec) bool {
	if s.currentTrans != nil {
		s.cycleLeft--
		if s.cycleLeft < 0 {
			return s.finalizeTrans(now)
		}
		return true
	}
	return s.pullFromBuf(now)
}

func (s *bankStage) pullFromBuf(now akita.VTimeInSec) bool {
	trans := s.cache.bankBufs[s.bankID].Peek()
	if trans == nil {
		return false
	}

	s.cycleLeft = s.latency
	s.currentTrans = trans.(*transaction)

	s.cache.bankBufs[s.bankID].Pop()
	return true
}

func (s *bankStage) finalizeTrans(now akita.VTimeInSec) bool {
	switch s.currentTrans.bankAction {
	case bankActionReadHit:
		return s.finalizeReadHit(now)
	case bankActionWrite:
		return s.finalizeWrite(now)
	case bankActionWriteFetched:
		return s.finalizeWriteFetchedTrans(now)
	default:
		panic("cannot handle trans bank action")
	}
}

func (s *bankStage) finalizeReadHit(
	now akita.VTimeInSec,
) bool {
	read := s.currentTrans.read
	block := s.currentTrans.block

	data, err := s.cache.storage.Read(
		block.CacheAddress, read.AccessByteSize)
	if err != nil {
		panic(err)
	}
	block.ReadCount--

	offset := read.Address - block.Tag
	s.currentTrans.data = data[offset : offset+read.AccessByteSize]
	s.currentTrans.done = true

	s.currentTrans = nil

	tracing.EndTask(read.ID, now, s.cache)
	return true
}

func (s *bankStage) finalizeWrite(
	now akita.VTimeInSec,
) bool {
	write := s.currentTrans.write
	block := s.currentTrans.block
	blockSize := 1 << s.cache.log2BlockSize

	data, err := s.cache.storage.Read(block.CacheAddress, uint64(blockSize))

	offset := write.Address - block.Tag
	for i := 0; i < len(write.Data); i++ {
		if write.DirtyMask[i] {
			data[offset+uint64(i)] = write.Data[i]
		}
	}

	err = s.cache.storage.Write(block.CacheAddress, data)
	if err != nil {
		panic(err)
	}
	block.DirtyMask = write.DirtyMask
	block.IsLocked = false

	tracing.EndTask(s.currentTrans.id, now, s.cache)

	s.currentTrans = nil

	return true
}

func (s *bankStage) finalizeWriteFetchedTrans(
	now akita.VTimeInSec,
) bool {
	block := s.currentTrans.block

	err := s.cache.storage.Write(block.CacheAddress, s.currentTrans.data)
	if err != nil {
		panic(err)
	}

	block.DirtyMask = s.currentTrans.writeFetchedDirtyMask
	block.IsLocked = false

	s.currentTrans = nil

	return true
}
