package writethrough

import (
	"strings"

	"gitlab.com/akita/akita"
	"gitlab.com/akita/mem/cache"
	"gitlab.com/akita/protocol"
	"gitlab.com/akita/util"
	"gitlab.com/akita/util/tracing"
)

type directory struct {
	cache     *Cache
	lookupBuf util.Buffer
}

// traceFail records an MSHR reservation fail at most once per transaction so
// the metric counts distinct blocked requests rather than per-cycle Tick
// re-probes. For mshr-fail it stamps mshrBlockedAt so the directory can
// measure how long the request waited for a slot. Mirrors writeback.
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
	if d.cache.arbiter {
		d.lookupBuf = d.cache.lookupBufForLocal
	} else {
		d.lookupBuf = d.cache.lookupBufForRemote
	}

	pipelineitem := d.lookupBuf.Peek()
	if pipelineitem == nil {
		if d.cache.lookupBufForLocal.Peek() != nil ||
			d.cache.lookupBufForRemote.Peek() != nil {
			return true
		}
		return false
	}

	pitem := pipelineitem.(cachePipelineItem)
	item := pitem.trans

	trans := item
	if trans.read != nil {
		return d.doRead(now, trans)
	}

	return d.doWrite(now, trans)
}

func (d *directory) doRead(
	now akita.VTimeInSec,
	trans *transaction,
) bool {
	cacheLineID, _ := cache.GetCacheLineID(
		trans.read.Address, d.cache.log2BlockSize)

	mshrEntry := d.cache.mshr.Query(trans.PID(), cacheLineID)
	if mshrEntry != nil {
		return d.handleMSHRHit(now, trans, mshrEntry)
	}

	block := d.cache.directory.Lookup(trans.PID(), cacheLineID)

	if block != nil && block.IsValid {
		return d.handleReadHit(now, trans, block)
	}

	return d.handleReadMiss(now, trans)
}

func (d *directory) handleMSHRHit(
	now akita.VTimeInSec,
	trans *transaction,
	mshrEntry *cache.MSHREntry,
) bool {
	mshrEntry.Requests = append(mshrEntry.Requests, trans)

	d.lookupBuf.Pop()

	if trans.read != nil {
		tracing.AddTaskStep(
			trans.id,
			now,
			d.cache,
			"read-mshr-hit")
	} else {
		tracing.AddTaskStep(
			trans.id,
			now,
			d.cache,
			"write-mshr-hit")
	}

	return true
}

func (d *directory) handleReadHit(
	now akita.VTimeInSec,
	trans *transaction,
	block *cache.Block,
) bool {
	// check if a block is being evicted or written.
	if block.IsLocked {
		return false
	}

	numBanks := len(d.cache.bankBufs)
	bank := cache.GetBankID(block, d.cache.directory.WayAssociativity(), numBanks)
	bankBuf := d.cache.bankBufs[bank]

	if !bankBuf.CanPush() {
		return false
	}

	d.cache.directory.Visit(block)
	block.ReadCount++
	trans.block = block
	trans.bankAction = bankActionReadHit

	d.lookupBuf.Pop()
	bankBuf.Push(trans)

	tracing.AddTaskStep(
		trans.id,
		now,
		d.cache,
		"read-hit")

	return true
}

func (d *directory) handleReadMiss(
	now akita.VTimeInSec,
	trans *transaction,
) bool {
	cacheLineID, _ := cache.GetCacheLineID(trans.Address(), d.cache.log2BlockSize)

	if d.cache.mshr.IsFull() {
		d.traceFail(trans, now, "mshr-fail", "read")
		return false
	}

	victim := d.cache.directory.FindVictim(cacheLineID)
	if victim.IsLocked || victim.ReadCount > 0 {
		return false
	}

	if !d.fetchFromBottom(now, trans, victim) {
		return false
	}

	d.lookupBuf.Pop()

	tracing.AddTaskStep(
		trans.id,
		now,
		d.cache,
		"read-miss")

	return true
}

func (d *directory) doWrite(
	now akita.VTimeInSec,
	trans *transaction,
) bool {
	write := trans.write
	cacheLineID, _ := cache.GetCacheLineID(trans.Address(), d.cache.log2BlockSize)

	mshrEntry := d.cache.mshr.Query(trans.PID(), cacheLineID)
	if mshrEntry != nil {
		ok := d.writeBottom(now, trans)
		if ok {
			return d.handleMSHRHit(now, trans, mshrEntry)
		}
		return false
	}

	block := d.cache.directory.Lookup(trans.PID(), cacheLineID)
	if block != nil && block.IsValid {
		ok := d.handleWriteHit(now, trans, block)
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
	cacheLineID, _ := cache.GetCacheLineID(trans.Address(), d.cache.log2BlockSize)
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

	d.lookupBuf.Pop()
	tracing.AddTaskStep(trans.id, now, d.cache, "write-miss")

	return true
}

func (d *directory) fullLineWriteMiss(
	now akita.VTimeInSec,
	trans *transaction,
) bool {
	cacheLineID, _ := cache.GetCacheLineID(trans.Address(), d.cache.log2BlockSize)
	block := d.cache.directory.FindVictim(cacheLineID)
	return d.handleWriteHit(now, trans, block)
}

func (d *directory) writeBottom(now akita.VTimeInSec, trans *transaction) bool {
	write := trans.write
	addr := write.Address

	dstPort := d.cache.lowModuleFinder.Find(addr)

	var srcPort akita.Port
	if strings.Contains(dstPort.Name(), "RDMA") {
		srcPort = d.cache.BottomToRdmaPort
	} else {
		srcPort = d.cache.BottomToDramPort
	}

	writeToBottom := protocol.WriteReqBuilder{}.
		WithSendTime(now).
		WithSrc(srcPort).
		WithDst(dstPort).
		WithAddress(addr).
		WithPID(write.PID).
		WithData(write.Data).
		WithDirtyMask(write.DirtyMask).
		Build()

	err := srcPort.Send(writeToBottom)
	if err != nil {
		return false
	}

	trans.writeToBottom = writeToBottom
	tracing.TraceReqInitiate(writeToBottom, now, d.cache, trans.id)

	return true
}

func (d *directory) handleWriteHit(
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

	cacheLineID, _ := cache.GetCacheLineID(trans.Address(), d.cache.log2BlockSize)

	block.IsLocked = true
	block.IsValid = true
	block.Tag = cacheLineID
	d.cache.directory.Visit(block)

	trans.bankAction = bankActionWrite
	trans.block = block
	bankBuf.Push(trans)

	d.lookupBuf.Pop()

	return true
}

func (d *directory) fetchFromBottom(
	now akita.VTimeInSec,
	trans *transaction,
	victim *cache.Block,
) bool {
	if d.cache.isReqToRemote(trans.Address()) {
		if !d.cache.isRemoteReqPossible() {
			return false
		}
		d.cache.numToRemote++
	}

	cacheLineID, _ := cache.GetCacheLineID(trans.Address(), d.cache.log2BlockSize)

	dstPort := d.cache.lowModuleFinder.Find(cacheLineID)

	var srcPort akita.Port
	if strings.Contains(dstPort.Name(), "RDMA") {
		srcPort = d.cache.BottomToRdmaPort
	} else {
		srcPort = d.cache.BottomToDramPort
	}

	readToBottom := protocol.ReadReqBuilder{}.
		WithSendTime(now).
		WithSrc(srcPort).
		WithDst(dstPort).
		WithAddress(cacheLineID).
		WithPID(trans.PID()).
		WithByteSize(1 << d.cache.log2BlockSize).
		Build()
	err := srcPort.Send(readToBottom)
	if err != nil {
		return false
	}

	tracing.TraceReqInitiate(readToBottom, now, d.cache, trans.id)
	trans.readToBottom = readToBottom
	trans.block = victim

	mshrEntry := d.cache.mshr.Add(trans.PID(), cacheLineID)
	mshrEntry.Requests = append(mshrEntry.Requests, trans)
	mshrEntry.ReadReq = readToBottom
	mshrEntry.Block = victim
	d.emitWaitTime(now, trans)

	victim.Tag = cacheLineID
	victim.PID = trans.PID()
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
