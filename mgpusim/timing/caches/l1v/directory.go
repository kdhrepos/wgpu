package l1v

import (
	"gitlab.com/akita/akita"
	"gitlab.com/akita/mem/cache"
	"gitlab.com/akita/protocol"
	"gitlab.com/akita/util"
	"gitlab.com/akita/util/tracing"
)

type directory struct {
	cache *Cache
}

// traceFail records an MSHR reservation/merge fail at most once per
// transaction, mirroring the L2 (writeback) directory stage. Without this
// dedupe, every Tick of stall would emit a fresh task and the metric would
// count cycles rather than distinct blocked requests.
func (d *directory) traceFail(
	t *transaction,
	now akita.VTimeInSec,
	kind, op string,
) {
	if t.failTraced {
		return
	}
	t.failTraced = true
	if kind == "mshr-fail" {
		t.mshrBlockedAt = now
	}
	tracing.StartTask(0, 0, 0, d.cache, kind, op, nil)
}

// emitWaitTime records how long trans waited from its first MSHR-full stall
// until this MSHR slot was finally allocated. No-op if it never stalled.
func (d *directory) emitWaitTime(now akita.VTimeInSec, trans *transaction) {
	if trans.mshrBlockedAt <= 0 {
		return
	}
	op := "read"
	if trans.write != nil {
		op = "write"
	}
	tracing.StartTask(0, 0, now, d.cache,
		"mshr-wait-time", op, float64(now-trans.mshrBlockedAt))
}

func (d *directory) Tick(now akita.VTimeInSec) bool {
	item := d.cache.dirBuf.Peek()
	if item == nil {
		return false
	}

	trans := item.(*transaction)
	if trans.read != nil {
		return d.processRead(now, trans)
	}

	return d.processWrite(now, trans)
}

func (d *directory) processRead(now akita.VTimeInSec, trans *transaction) bool {
	read := trans.read
	addr := read.Address
	// pid := read.PID
	blockSize := uint64(1 << d.cache.log2BlockSize)
	cacheLineID := addr / blockSize * blockSize

	mshrEntry := d.cache.mshr.Query(1, cacheLineID)
	if mshrEntry != nil {
		if !d.cache.mshr.CanMerge(mshrEntry) {
			// Entry is at its max-merge cap — secondary miss cannot coalesce.
			d.traceFail(trans, now, "mshr-merge-fail", "read")
			return false
		}
		return d.processMSHRHit(now, trans, mshrEntry)
	}

	block := d.cache.directory.Lookup(1, cacheLineID)
	if block != nil && block.IsValid {
		return d.processReadHit(now, trans, block)
	}

	return d.processReadMiss(now, trans)
}

func (d *directory) processMSHRHit(
	now akita.VTimeInSec,
	trans *transaction,
	mshrEntry *cache.MSHREntry,
) bool {
	mshrEntry.Requests = append(mshrEntry.Requests, trans)

	d.cache.dirBuf.Pop()

	if trans.read != nil {
		tracing.AddTaskStep(trans.id, now, d.cache, "read-mshr-hit")
	} else {
		tracing.AddTaskStep(trans.id, now, d.cache, "write-mshr-hit")
	}

	return true
}

func (d *directory) processReadHit(
	now akita.VTimeInSec,
	trans *transaction,
	block *cache.Block,
) bool {
	if block.IsLocked {
		return false
	}

	bankBuf := d.getBankBuf(block)
	if !bankBuf.CanPush() {
		return false
	}

	trans.block = block
	trans.bankAction = bankActionReadHit
	block.ReadCount++
	d.cache.directory.Visit(block)
	bankBuf.Push(trans)

	d.cache.dirBuf.Pop()
	tracing.AddTaskStep(trans.id, now, d.cache, "read-hit")

	return true
}

func (d *directory) processReadMiss(
	now akita.VTimeInSec,
	trans *transaction,
) bool {
	read := trans.read
	addr := read.Address
	blockSize := uint64(1 << d.cache.log2BlockSize)
	cacheLineID := addr / blockSize * blockSize

	victim := d.cache.directory.FindVictim(cacheLineID)
	if victim.IsLocked || victim.ReadCount > 0 {
		return false
	}

	if d.cache.mshr.IsFull() {
		d.traceFail(trans, now, "mshr-fail", "read")
		return false
	}

	if !d.fetchFromBottom(now, trans, victim) {
		return false
	}

	d.cache.dirBuf.Pop()
	tracing.AddTaskStep(trans.id, now, d.cache, "read-miss")

	return true
}

func (d *directory) processWrite(
	now akita.VTimeInSec,
	trans *transaction,
) bool {
	write := trans.write
	addr := write.Address
	// pid := write.PID
	blockSize := uint64(1 << d.cache.log2BlockSize)
	cacheLineID := addr / blockSize * blockSize

	mshrEntry := d.cache.mshr.Query(1, cacheLineID)
	if mshrEntry != nil {
		if !d.cache.mshr.CanMerge(mshrEntry) {
			// Entry is at its max-merge cap — secondary miss cannot coalesce.
			d.traceFail(trans, now, "mshr-merge-fail", "write")
			return false
		}
		ok := d.writeBottom(now, trans)
		if ok {
			return d.processMSHRHit(now, trans, mshrEntry)
		}
		return false
	}

	block := d.cache.directory.Lookup(1, cacheLineID)
	if block != nil && block.IsValid {
		ok := d.processWriteHit(now, trans, block)
		if ok {
			tracing.AddTaskStep(trans.id, now, d.cache, "write-hit")
		}

		return ok
	}

	if d.isPartialWrite(write) {
		return d.partialWriteMiss(now, trans)
	}

	ok := d.fullLineWriteMiss(now, trans)
	if ok {
		tracing.AddTaskStep(trans.id, now, d.cache, "write-miss")
	}

	return ok
}

func (d *directory) isPartialWrite(write *protocol.WriteReq) bool {
	if len(write.Data) < (1 << d.cache.log2BlockSize) {
		return true
	}

	if write.DirtyMask != nil {
		for _, byteDirty := range write.DirtyMask {
			if !byteDirty {
				return true
			}
		}
	}

	return false
}

func (d *directory) partialWriteMiss(
	now akita.VTimeInSec,
	trans *transaction,
) bool {
	write := trans.write
	addr := write.Address
	blockSize := uint64(1 << d.cache.log2BlockSize)
	cacheLineID := addr / blockSize * blockSize
	trans.fetchAndWrite = true

	if d.cache.mshr.IsFull() {
		d.traceFail(trans, now, "mshr-fail", "write")
		return false
	}

	victim := d.cache.directory.FindVictim(cacheLineID)
	if victim.ReadCount > 0 || victim.IsLocked {
		return false
	}

	sentThisCycle := false
	if trans.writeToBottom == nil {
		ok := d.writeBottom(now, trans)
		if !ok {
			return false
		}
		sentThisCycle = true
	}

	ok := d.fetchFromBottom(now, trans, victim)
	if !ok {
		if sentThisCycle {
			return true
		}
		return false
	}

	d.cache.dirBuf.Pop()
	tracing.AddTaskStep(trans.id, now, d.cache, "write-miss")

	return true
}

func (d *directory) fullLineWriteMiss(
	now akita.VTimeInSec,
	trans *transaction,
) bool {
	write := trans.write
	addr := write.Address
	blockSize := uint64(1 << d.cache.log2BlockSize)
	cacheLineID := addr / blockSize * blockSize
	block := d.cache.directory.FindVictim(cacheLineID)
	return d.processWriteHit(now, trans, block)
}

func (d *directory) writeBottom(now akita.VTimeInSec, trans *transaction) bool {
	write := trans.write
	addr := write.Address

	writeToBottom := protocol.WriteReqBuilder{}.
		WithSendTime(now).
		WithSrc(d.cache.BottomPort).
		WithDst(d.cache.lowModuleFinder.Find(addr)).
		WithAddress(addr).
		WithPID(1).
		WithData(write.Data).
		WithDirtyMask(write.DirtyMask).
		WithInfo(write.Info).
		Build()

	err := d.cache.BottomPort.Send(writeToBottom)
	if err != nil {
		return false
	}

	trans.writeToBottom = writeToBottom

	tracing.TraceReqInitiate(writeToBottom, now, d.cache, trans.id)

	return true
}

func (d *directory) processWriteHit(
	now akita.VTimeInSec,
	trans *transaction,
	block *cache.Block,
) bool {
	if block.IsLocked || block.ReadCount > 0 {
		return false
	}

	bankBuf := d.getBankBuf(block)
	if !bankBuf.CanPush() {
		return false
	}

	if trans.writeToBottom == nil {
		ok := d.writeBottom(now, trans)
		if !ok {
			return false
		}
	}

	write := trans.write
	addr := write.Address
	blockSize := uint64(1 << d.cache.log2BlockSize)
	cacheLineID := addr / blockSize * blockSize
	block.IsLocked = true
	block.IsValid = true
	block.Tag = cacheLineID
	d.cache.directory.Visit(block)

	trans.bankAction = bankActionWrite
	trans.block = block
	bankBuf.Push(trans)

	d.cache.dirBuf.Pop()

	return true
}

func (d *directory) fetchFromBottom(
	now akita.VTimeInSec,
	trans *transaction,
	victim *cache.Block,
) bool {
	addr := trans.Address()
	// pid := trans.PID()
	blockSize := uint64(1 << d.cache.log2BlockSize)
	cacheLineID := addr / blockSize * blockSize

	var info interface{}
	if trans.read != nil {
		info = trans.preCoalesceTransactions[0].read.Info
	} else if trans.write != nil {
		info = trans.preCoalesceTransactions[0].write.Info
	}

	bottomModule := d.cache.lowModuleFinder.Find(cacheLineID)
	readToBottom := protocol.ReadReqBuilder{}.
		WithSendTime(now).
		WithSrc(d.cache.BottomPort).
		WithDst(bottomModule).
		WithAddress(cacheLineID).
		WithPID(1).
		WithByteSize(blockSize).
		WithInfo(info).
		Build()
	err := d.cache.BottomPort.Send(readToBottom)
	if err != nil {
		return false
	}

	tracing.TraceReqInitiate(readToBottom, now, d.cache, trans.id)
	trans.readToBottom = readToBottom
	trans.block = victim

	mshrEntry := d.cache.mshr.Add(1, cacheLineID)
	mshrEntry.Requests = append(mshrEntry.Requests, trans)
	mshrEntry.ReadReq = readToBottom
	mshrEntry.Block = victim
	d.emitWaitTime(now, trans)

	victim.Tag = cacheLineID
	victim.PID = 1
	victim.IsValid = true
	victim.IsLocked = true
	d.cache.directory.Visit(victim)

	return true
}

func (d *directory) getBankBuf(block *cache.Block) util.Buffer {
	numWaysPerSet := d.cache.directory.WayAssociativity()
	blockID := block.SetID*numWaysPerSet + block.WayID
	bankID := blockID % len(d.cache.bankBufs)
	return d.cache.bankBufs[bankID]
}
