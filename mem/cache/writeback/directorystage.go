package writeback

import (
	"fmt"
	"strings"

	"gitlab.com/akita/akita"
	"gitlab.com/akita/mem/cache"
	"gitlab.com/akita/protocol"
	"gitlab.com/akita/util/ca"
	"gitlab.com/akita/util/tracing"
)

type directoryStage struct {
	cache *Cache
}

// Tick selects and processes one transaction per cycle. Selection priority
// (applied across both source buffers via pickPreferred):
//
//  1. toRemote=false first — this chiplet is the home node for the address.
//     Encoded as priority tier 0 < tier 1 in the buffer sort key.
//  2. Among tier-equal candidates, a transaction whose target block is NOT
//     locked is served first. This is enforced as a READINESS filter, not a
//     sort key: a locked block makes readReady/writeReady return false, so
//     PeekReady skips it and walks on to the next ready item. Consequently a
//     later unlocked transaction may bypass an earlier locked one of the same
//     tier.
//  3. Ties break by lowest request ID (oldest first) — transOrderKey.
//
// The buffer is sorted only by (toRemote-tier, id); the lock test lives in the
// ready() predicate. IsPTW is not considered in selection at all — PTW reads
// are ordered and served exactly like any other read.
func (ds *directoryStage) Tick(now akita.VTimeInSec) bool {
	buf := ds.cache.lookupBuf.(*LookupBuffer)

	// Deadline gate: a transaction admitted at cycle C is invisible to the
	// directory until C+pipelineLatency, modelling tag-lookup latency.
	nowCycle := ds.cache.Freq.Cycle(now)
	ready := func(t *transaction) bool {
		if nowCycle < t.pipelineReadyCycle {
			return false
		}
		return ds.transactionReady(now, t)
	}
	// One (toRemote, id)-ordered heap: PeekReady walks in priority order, so the
	// first ready item is already the local-first, lowest-id one — no
	// cross-buffer arbitration needed.
	item, idx, ok := buf.PeekReady(ready)
	if !ok {
		// Nothing ready. Return false (no progress) even when buffers are
		// non-empty-but-not-ready, so the engine can quiesce and the driver's
		// deadlock detector fires instead of spinning with zero progress (a
		// livelock that masks a real deadlock).
		return false
	}

	trans := item.trans
	success := false
	if trans.read != nil {
		success = ds.doRead(now, trans)
	} else {
		success = ds.doWrite(now, trans)
	}

	if success {
		buf.RemoveAt(idx)
	}
	// Report actual progress: a failed doRead/doWrite (e.g. bank buffer full)
	// made no progress, so do not keep the cache ticking on a false positive.
	return success
}

func (ds *directoryStage) transactionReady(now akita.VTimeInSec, t *transaction) bool {
	if t.read != nil {
		return ds.readReady(now, t)
	}
	if t.write != nil {
		return ds.writeReady(now, t)
	}
	return false
}

// traceFail records an MSHR reservation/merge fail at most once per
// transaction, so the metric counts distinct blocked requests rather than
// per-cycle PeekReady re-probes. For mshr-fail it also stamps mshrBlockedAt
// so the directory can measure how long the request waited for a slot.
func (ds *directoryStage) traceFail(t *transaction, now akita.VTimeInSec, kind, op string) {
	if t.failTraced {
		return
	}
	t.failTraced = true
	if kind == "mshr-fail" {
		t.mshrBlockedAt = now
	}
	tracing.StartTask(0, 0, 0, ds.cache, kind, op, nil)
}

func (ds *directoryStage) readReady(now akita.VTimeInSec, t *transaction) bool {
	cacheLineID, _ := cache.GetCacheLineID(
		t.read.Address, ds.cache.log2BlockSize)

	if e := ds.cache.mshr.Query(t.read.PID, cacheLineID); e != nil {
		if ds.cache.mshr.CanMerge(e) {
			return true
		}
		// Entry is at its max-merge cap — cannot coalesce another request.
		// Stall and retry, mirroring the IsFull gate below.
		ds.traceFail(t, now, "mshr-merge-fail", "read")
		return false
	}

	block := ds.cache.directory.Lookup(t.read.PID, cacheLineID)
	if block != nil {
		return !block.IsLocked
	}

	if ds.cache.mshr.IsFull() {
		// Record MSHR-full pressure here — this is the actual gate that
		// stops a missing transaction from advancing.
		ds.traceFail(t, now, "mshr-fail", "read")
		return false
	}
	victim := ds.cache.directory.FindVictim(cacheLineID)
	return !victim.IsLocked && victim.ReadCount == 0
}

func (ds *directoryStage) writeReady(now akita.VTimeInSec, t *transaction) bool {
	cacheLineID, _ := cache.GetCacheLineID(
		t.write.Address, ds.cache.log2BlockSize)

	if e := ds.cache.mshr.Query(t.write.PID, cacheLineID); e != nil {
		if ds.cache.mshr.CanMerge(e) {
			return true
		}
		// Entry is at its max-merge cap — cannot coalesce another request.
		ds.traceFail(t, now, "mshr-merge-fail", "write")
		return false
	}

	block := ds.cache.directory.Lookup(t.write.PID, cacheLineID)
	if block != nil {
		return !block.IsLocked && block.ReadCount == 0
	}

	if ds.isWritingFullLine(t.write) {
		victim := ds.cache.directory.FindVictim(cacheLineID)
		return !victim.IsLocked && victim.ReadCount == 0
	}

	if ds.cache.mshr.IsFull() {
		// Record MSHR-full pressure here — see readReady for rationale.
		ds.traceFail(t, now, "mshr-fail", "write")
		return false
	}
	victim := ds.cache.directory.FindVictim(cacheLineID)
	return !victim.IsLocked && victim.ReadCount == 0
}

func (ds *directoryStage) Reset(now akita.VTimeInSec) {
	ds.cache.lookupBuf.Clear()
}

func (ds *directoryStage) doRead(
	now akita.VTimeInSec,
	trans *transaction,
) bool {
	cachelineID, _ := cache.GetCacheLineID(
		trans.read.Address, ds.cache.log2BlockSize)

	// Count this transaction once per setID — distinct from getSet's
	// internal use which fires per PeekReady probe (inflated by stalls).
	if di, ok := ds.cache.directory.(*cache.DirectoryImpl); ok {
		di.RecordSetAccess(cachelineID)
	}

	mshrEntry := ds.cache.mshr.Query(trans.read.PID, cachelineID)
	if mshrEntry != nil {
		return ds.handleReadMSHRHit(now, trans, mshrEntry)
	}

	block := ds.cache.directory.Lookup(
		trans.read.PID, cachelineID)

	if block != nil {
		return ds.handleReadHit(now, trans, block)
	}

	return ds.handleReadMiss(now, trans)
}

func (ds *directoryStage) handleReadMSHRHit(
	now akita.VTimeInSec,
	trans *transaction,
	mshrEntry *cache.MSHREntry,
) bool {
	trans.mshrEntry = mshrEntry
	mshrEntry.Requests = append(mshrEntry.Requests, trans)
	// ds.prioritizeLocalReqs(mshrEntry, trans)

	src := trans.read.Meta().Src.Name()
	if strings.Contains(src, "MMU") {
		// tracing.StartTask(
		// "", "", now,
		// ds.cache,
		// "pte-caching-read-mshr-hit",
		// "", nil)
	} else {
		tracing.AddTaskStep(
			tracing.MsgIDAtReceiver(trans.read, ds.cache),
			now, ds.cache,
			"data-read-mshr-hit",
		)
	}

	if trans.fromLocal {
		isVec := uint64(0)
		if strings.Contains(src, "L1V") {
			isVec = 1
		}

		tracing.StartTask(
			0,
			isVec,
			now,
			ds.cache,
			"memory-trace",
			ds.cache.chipletIDStrings[ds.cache.HomeNodeFinder.GetChipletId(trans.read.Address)],
			trans.read,
		)
	}

	return true
}

func (ds *directoryStage) handleReadHit(
	now akita.VTimeInSec,
	trans *transaction,
	block *cache.Block,
) bool {
	// check if a block is being evicted or written.
	if block.IsLocked {
		return false
	}

	return ds.readFromBank(trans, block, now)
}

func (ds *directoryStage) readFromBank(
	trans *transaction,
	block *cache.Block,
	now akita.VTimeInSec,
) bool {
	numBanks := len(ds.cache.dirToBankBuffers)
	bank := cache.GetBankID(block, ds.cache.directory.WayAssociativity(), numBanks)
	bankBuf := ds.cache.dirToBankBuffers[bank]

	if !bankBuf.CanPushTransReserve(trans.toRemote, smartBufferLocalReserve) {
		// return true
		return false
	}

	ds.cache.directory.Visit(block) // update LRU list
	block.ReadCount++
	trans.block = block
	trans.action = bankReadHit
	bankBuf.Push(trans)

	src := trans.read.Meta().Src.Name()
	if strings.Contains(src, "MMU") {
		// tracing.StartTask(
		// "", "", now,
		// ds.cache,
		// "pte-caching-read-hit",
		// "", nil)
	} else {
		tracing.AddTaskStep(
			tracing.MsgIDAtReceiver(trans.read, ds.cache),
			now,
			ds.cache,
			"data-read-hit",
		)
	}

	if trans.fromLocal {
		isVec := uint64(0)
		if strings.Contains(src, "L1V") {
			isVec = 1
		}

		tracing.StartTask(
			1,
			isVec,
			now,
			ds.cache,
			"memory-trace",
			ds.cache.chipletIDStrings[ds.cache.HomeNodeFinder.GetChipletId(trans.read.Address)],
			trans.read,
		)
	}

	return true
}

func (ds *directoryStage) handleReadMiss(
	now akita.VTimeInSec,
	trans *transaction,
) bool {
	req := trans.read
	cacheLineID, _ := cache.GetCacheLineID(req.Address, ds.cache.log2BlockSize)

	if ds.cache.mshr.IsFull() {
		ds.traceFail(trans, now, "mshr-fail", "read")
		// return true
		return false
	}

	victim := ds.cache.directory.FindVictim(cacheLineID)
	if victim.IsLocked || victim.ReadCount > 0 {
		return false
	}

	if ds.needEviction(victim) {
		ok := ds.evict(now, trans, victim)
		if ok {
			// if ds.cache.isReqToRemote(req.Address) {
			// 	tracing.StartTask(
			// 		"",
			// 		"",
			// 		now,
			// 		ds.cache,
			// 		"l2-miss-trace",
			// 		ds.cache.selfChipletIDString,
			// 		ds.cache.chipletIDStrings[ds.cache.HomeNodeFinder.GetChipletId(req.Address)])
			// }

			src := trans.read.Meta().Src.Name()
			if strings.Contains(src, "MMU") {
				// tracing.StartTask(
				// "", "", now,
				// ds.cache,
				// "pte-caching-read-miss",
				// "", nil)
			} else {
				tracing.AddTaskStep(
					tracing.MsgIDAtReceiver(trans.read, ds.cache),
					now,
					ds.cache,
					"data-read-miss",
				)
			}

			if trans.fromLocal {
				isVec := uint64(0)
				if strings.Contains(src, "L1V") {
					isVec = 1
				}

				tracing.StartTask(
					2,
					isVec,
					now,
					ds.cache,
					"memory-trace",
					ds.cache.chipletIDStrings[ds.cache.HomeNodeFinder.GetChipletId(trans.read.Address)],
					trans.read,
				)
			}
		}
		return ok
	}

	ok := ds.fetch(now, trans, victim)
	if ok {
		// if ds.cache.isReqToRemote(req.Address) {
		// 	tracing.StartTask(
		// 		"",
		// 		"",
		// 		now,
		// 		ds.cache,
		// 		"l2-miss-trace",
		// 		ds.cache.selfChipletIDString,
		// 		ds.cache.chipletIDStrings[ds.cache.HomeNodeFinder.GetChipletId(req.Address)])
		// }

		src := trans.read.Meta().Src.Name()
		if strings.Contains(src, "MMU") {
			// tracing.StartTask(
			// "", "", now,
			// ds.cache,
			// "pte-caching-read-miss",
			// "", nil)
		} else {
			tracing.AddTaskStep(
				tracing.MsgIDAtReceiver(trans.read, ds.cache),
				now,
				ds.cache,
				"data-read-miss",
			)
		}

		if trans.fromLocal {
			isVec := uint64(0)
			if strings.Contains(src, "L1V") {
				isVec = 1
			}

			tracing.StartTask(
				2,
				isVec,
				now,
				ds.cache,
				"memory-trace",
				ds.cache.chipletIDStrings[ds.cache.HomeNodeFinder.GetChipletId(trans.read.Address)],
				trans.read,
			)
		}
	}
	return ok
}

func (ds *directoryStage) doWrite(
	now akita.VTimeInSec,
	trans *transaction,
) bool {
	write := trans.write
	cachelineID, _ := cache.GetCacheLineID(write.Address, ds.cache.log2BlockSize)

	if di, ok := ds.cache.directory.(*cache.DirectoryImpl); ok {
		di.RecordSetAccess(cachelineID)
	}

	mshrEntry := ds.cache.mshr.Query(write.PID, cachelineID)
	if mshrEntry != nil {
		ok := ds.doWriteMSHRHit(now, trans, mshrEntry)
		tracing.AddTaskStep(
			tracing.MsgIDAtReceiver(trans.write, ds.cache),
			now, ds.cache,
			"data-write-mshr-hit",
		)

		src := trans.write.Meta().Src.Name()
		if trans.fromLocal {
			isVec := uint64(0)
			if strings.Contains(src, "L1V") {
				isVec = 1
			}

			tracing.StartTask(
				3,
				isVec,
				now,
				ds.cache,
				"memory-trace",
				ds.cache.chipletIDStrings[ds.cache.HomeNodeFinder.GetChipletId(trans.write.Address)],
				trans.write,
			)
		}

		return ok
	}

	block := ds.cache.directory.Lookup(trans.write.PID, cachelineID)
	if block != nil {
		ok := ds.doWriteHit(trans, block)
		if ok {
			tracing.AddTaskStep(
				tracing.MsgIDAtReceiver(trans.write, ds.cache),
				now, ds.cache,
				"data-write-hit",
			)

			src := trans.write.Meta().Src.Name()
			if trans.fromLocal {
				isVec := uint64(0)
				if strings.Contains(src, "L1V") {
					isVec = 1
				}

				tracing.StartTask(
					4,
					isVec,
					now,
					ds.cache,
					"memory-trace",
					ds.cache.chipletIDStrings[ds.cache.HomeNodeFinder.GetChipletId(trans.write.Address)],
					trans.write,
				)
			}
		}

		return ok
	}

	ok := ds.doWriteMiss(now, trans)
	if ok {
		tracing.AddTaskStep(
			tracing.MsgIDAtReceiver(trans.write, ds.cache),
			now, ds.cache,
			"data-write-miss",
		)
		// if ds.cache.isReqToRemote(write.Address) {
		// 	tracing.StartTask(
		// 		"",
		// 		"",
		// 		now,
		// 		ds.cache,
		// 		"l2-miss-trace",
		// 		ds.cache.selfChipletIDString,
		// 		ds.cache.chipletIDStrings[ds.cache.HomeNodeFinder.GetChipletId(write.Address)])
		// }

		src := trans.write.Meta().Src.Name()
		if trans.fromLocal {
			isVec := uint64(0)
			if strings.Contains(src, "L1V") {
				isVec = 1
			}

			tracing.StartTask(
				5,
				isVec,
				now,
				ds.cache,
				"memory-trace",
				ds.cache.chipletIDStrings[ds.cache.HomeNodeFinder.GetChipletId(trans.write.Address)],
				trans.write,
			)
		}
	}

	return ok
}

func (ds *directoryStage) doWriteMSHRHit(
	now akita.VTimeInSec,
	trans *transaction,
	mshrEntry *cache.MSHREntry,
) bool {
	trans.mshrEntry = mshrEntry
	mshrEntry.Requests = append(mshrEntry.Requests, trans)
	// ds.prioritizeLocalReqs(mshrEntry, trans)

	return true
}

func (ds *directoryStage) doWriteHit(
	trans *transaction,
	block *cache.Block,
) bool {
	if block.IsLocked || block.ReadCount > 0 {
		return false
	}

	return ds.writeToBank(trans, block)
}

func (ds *directoryStage) doWriteMiss(
	now akita.VTimeInSec,
	trans *transaction,
) bool {
	write := trans.write

	if ds.isWritingFullLine(write) {
		return ds.writeFullLineMiss(now, trans)
	}
	return ds.writePartialLineMiss(now, trans)
}

func (ds *directoryStage) writeFullLineMiss(
	now akita.VTimeInSec,
	trans *transaction,
) bool {
	write := trans.write
	cachelineID, _ := cache.GetCacheLineID(write.Address, ds.cache.log2BlockSize)

	victim := ds.cache.directory.FindVictim(cachelineID)
	if victim.IsLocked || victim.ReadCount > 0 {
		return false
	}

	if ds.needEviction(victim) {
		return ds.evict(now, trans, victim)
	}

	return ds.writeToBank(trans, victim)
}

func (ds *directoryStage) writePartialLineMiss(
	now akita.VTimeInSec,
	trans *transaction,
) bool {
	write := trans.write
	cachelineID, _ := cache.GetCacheLineID(write.Address, ds.cache.log2BlockSize)

	if ds.cache.mshr.IsFull() {
		ds.traceFail(trans, now, "mshr-fail", "write")
		return false
	}

	victim := ds.cache.directory.FindVictim(cachelineID)
	if victim.IsLocked || victim.ReadCount > 0 {
		return false
	}

	if ds.needEviction(victim) {
		return ds.evict(now, trans, victim)
	}

	return ds.fetch(now, trans, victim)
}

func (ds *directoryStage) writeToBank(
	trans *transaction,
	block *cache.Block,
) bool {
	numBanks := len(ds.cache.dirToBankBuffers)
	bank := cache.GetBankID(block, ds.cache.directory.WayAssociativity(), numBanks)
	bankBuf := ds.cache.dirToBankBuffers[bank]

	if !bankBuf.CanPushTransReserve(trans.toRemote, smartBufferLocalReserve) {
		// return true
		return false
	}

	addr := trans.write.Address
	cachelineID, _ := cache.GetCacheLineID(addr, ds.cache.log2BlockSize)

	ds.cache.directory.Visit(block)
	block.IsLocked = true
	block.Tag = cachelineID
	block.IsValid = true
	block.PID = trans.write.PID
	trans.block = block
	trans.action = bankWriteHit
	bankBuf.Push(trans)

	return true
}

func (ds *directoryStage) evict(
	now akita.VTimeInSec,
	trans *transaction,
	victim *cache.Block,
) bool {
	bankNum := cache.GetBankID(victim,
		ds.cache.directory.WayAssociativity(), len(ds.cache.dirToBankBuffers))
	bankBuf := ds.cache.dirToBankBuffers[bankNum]

	if !bankBuf.CanPushTransReserve(trans.toRemote, smartBufferLocalReserve) {
		// return true
		return false
	}

	var addr uint64
	var pid ca.PID
	var req protocol.AccessReq
	if trans.read != nil {
		addr = trans.read.Address
		pid = trans.read.PID
		req = trans.read
	} else {
		addr = trans.write.Address
		pid = trans.write.PID
		req = trans.write
	}

	cacheLineID, _ := cache.GetCacheLineID(addr, ds.cache.log2BlockSize)

	trans.action = bankEvictAndFetch
	trans.victim = &cache.Block{
		PID:          victim.PID,
		Tag:          victim.Tag,
		CacheAddress: victim.CacheAddress,
		DirtyMask:    victim.DirtyMask,
		InstallPC:    victim.InstallPC,
	}

	trans.block = victim
	trans.evictingPID = trans.victim.PID
	trans.evictingAddr = trans.victim.Tag
	trans.evictingDirtyMask = victim.DirtyMask

	if ds.evictionNeedFetch(trans) {
		// if the request goes to remote chiplet, it can make deadlock
		if ds.cache.isReqToRemote(addr) {
			if !ds.cache.isRemoteReqPossible() {
				return false
			}
			ds.cache.numToRemote++
			trans.toRemote = true
		}
		mshrEntry := ds.cache.mshr.Add(pid, cacheLineID)
		mshrEntry.Block = victim
		mshrEntry.Requests = append(mshrEntry.Requests, trans)

		if trans.mshrBlockedAt > 0 {
			op := "read"
			if trans.write != nil {
				op = "write"
			}
			tracing.StartTask(0, 0, now, ds.cache,
				"mshr-wait-time", op, float64(now-trans.mshrBlockedAt))
		}

		trans.mshrEntry = mshrEntry
		trans.fetchPID = pid
		trans.fetchAddress = cacheLineID
		trans.action = bankEvictAndFetch
	} else {
		trans.action = bankEvictAndWrite
	}

	victim.Tag = cacheLineID
	victim.PID = pid
	victim.IsLocked = true
	victim.IsDirty = false
	victim.InstallPC = tracing.PCFromReq(req)

	bankBuf.Push(trans)

	victimHome := ds.cache.chipletIDStrings[ds.cache.HomeNodeFinder.GetChipletId(trans.evictingAddr)]

	tracing.StartTask(
		0, 0, now, ds.cache,
		"evict",
		victimHome,
		req)

	tracing.StartTask(
		6,
		1,
		now,
		ds.cache,
		"memory-trace",
		victimHome,
		req,
	)

	tracing.StartTask(
		8,
		1,
		now,
		ds.cache,
		"memory-trace",
		victimHome,
		&tracing.VictimInfo{
			VictimAddr: trans.evictingAddr,
			VictimPC:   trans.victim.InstallPC,
		},
	)

	return true
}

func (ds *directoryStage) evictionNeedFetch(
	t *transaction,
) bool {
	if t.write == nil {
		return true
	}

	if ds.isWritingFullLine(t.write) {
		return false
	}

	return true
}

func (ds *directoryStage) fetch(
	now akita.VTimeInSec,
	trans *transaction,
	block *cache.Block,
) bool {

	var addr uint64
	var pid ca.PID
	var req protocol.AccessReq
	if trans.read != nil {
		req = trans.read
		addr = trans.read.Address
		pid = trans.read.PID
	} else {
		req = trans.write
		addr = trans.write.Address
		pid = trans.write.PID
	}
	cacheLineID, _ := cache.GetCacheLineID(addr, ds.cache.log2BlockSize)

	bankNum := cache.GetBankID(block,
		ds.cache.directory.WayAssociativity(), len(ds.cache.dirToBankBuffers))
	bankBuf := ds.cache.dirToBankBuffers[bankNum]

	if !bankBuf.CanPushTransReserve(trans.toRemote, smartBufferLocalReserve) {
		// return true
		return false
	}

	// if the request goes to remote chiplet, it could make deadlock
	if ds.cache.isReqToRemote(addr) {
		if !ds.cache.isRemoteReqPossible() {
			return false
		}
		ds.cache.numToRemote++
	}

	// need to do if block is not dirty but previous cacheline is ovewritten.
	if block.IsValid && !block.IsDirty {
		victimAddr := block.Tag
		victimPC := block.InstallPC
		victimHome := ds.cache.chipletIDStrings[ds.cache.HomeNodeFinder.GetChipletId(victimAddr)]

		tracing.StartTask(
			0, 0, now, ds.cache,
			"overwrite",
			victimHome, // address of overwritten cacheline
			req)

		tracing.StartTask(
			7,
			1,
			now,
			ds.cache,
			"memory-trace",
			victimHome,
			req,
		)

		tracing.StartTask(
			9,
			1,
			now,
			ds.cache,
			"memory-trace",
			victimHome,
			&tracing.VictimInfo{
				VictimAddr: victimAddr,
				VictimPC:   victimPC,
			},
		)
	}

	mshrEntry := ds.cache.mshr.Add(pid, cacheLineID)
	trans.mshrEntry = mshrEntry
	trans.block = block
	block.IsLocked = true
	block.Tag = cacheLineID
	block.PID = pid
	block.IsValid = true
	block.InstallPC = tracing.PCFromReq(req)
	ds.cache.directory.Visit(block)

	if trans.mshrBlockedAt > 0 {
		op := "read"
		if trans.write != nil {
			op = "write"
		}
		tracing.StartTask(0, 0, now, ds.cache,
			"mshr-wait-time", op, float64(now-trans.mshrBlockedAt))
	}

	tracing.AddTaskStep(
		tracing.MsgIDAtReceiver(req, ds.cache),
		now,
		ds.cache,
		fmt.Sprintf("add-mshr-entry-0x%x-0x%x", mshrEntry.Address, block.Tag),
	)

	trans.action = writeBufferFetch
	trans.fetchPID = pid
	trans.fetchAddress = cacheLineID
	bankBuf.Push(trans)

	mshrEntry.Block = block
	mshrEntry.Requests = append(mshrEntry.Requests, trans)

	return true
}

func (ds *directoryStage) isWritingFullLine(
	write *protocol.WriteReq,
) bool {
	if len(write.Data) != (1 << ds.cache.log2BlockSize) {
		return false
	}

	if write.DirtyMask != nil {
		for _, dirty := range write.DirtyMask {
			if !dirty {
				return false
			}
		}
	}

	return true
}

func (ds *directoryStage) needEviction(
	victim *cache.Block,
) bool {
	return victim.IsValid && victim.IsDirty
}
