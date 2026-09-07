package writeback_dynamic_dist

import (
	"fmt"
	"strings"

	"gitlab.com/akita/akita"
	"gitlab.com/akita/protocol"
	"gitlab.com/akita/util/ca"
	"gitlab.com/akita/util/tracing"
)

type directoryStage struct {
	cache *Cache
}

// Tick selects and processes one transaction per cycle. Selection priority:
//
//  1. Tier (transPriorityKey), lower served first:
//     0 local-home serve (!toRemote && !isLookup) — this chiplet is home.
//     1 lookup probe (isLookup) — immediately forwardable, keeps the
//     distribution chain flowing (no MSHR / numToRemote budget).
//     2 real remote fetch (toRemote && !isLookup) — throttled by numToRemote.
//  2. Among tier-equal candidates, a transaction whose target block is NOT
//     locked is served first. This is enforced as a READINESS filter, not a
//     sort key: a locked block makes readReady/writeReady return false, so
//     PeekReady skips it and walks on to the next ready item. Consequently a
//     later unlocked transaction may bypass an earlier locked one of the same
//     tier.
//  3. Ties break by lowest request ID (oldest first) — transOrderKey.
//
// The buffer is sorted only by (tier, id); the lock test lives in the
// ready() predicate. IsPTW is not considered in selection at all — PTW reads
// are ordered and served exactly like any other read.
func (ds *directoryStage) Tick(now akita.VTimeInSec) bool {
	buf := ds.cache.lookupBuf.(*LookupBuffer)

	// The single buffer is sorted (toRemote-tier, id), so PeekReady returns the
	// highest-priority ready item — local-home (toRemote=false) first, then lower
	// id — exactly what the former two-buffer pickPreferred produced.
	ready := ds.readyWithDeadline(now)
	item, idx, ok := buf.PeekReady(ready)
	if !ok {
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
	return success
}

func (ds *directoryStage) transactionReady(
	now akita.VTimeInSec,
	t *transaction,
) bool {
	if t.read != nil {
		return ds.readReady(now, t)
	}
	if t.write != nil {
		return ds.writeReady(now, t)
	}
	return false
}

// readyWithDeadline gates each transaction by its pipelineReadyCycle so
// the directory sees a transaction only after pipelineLatency has elapsed
// since topparser admission.
func (ds *directoryStage) readyWithDeadline(
	now akita.VTimeInSec,
) func(*transaction) bool {
	nowCycle := ds.cache.Freq.Cycle(now)
	return func(t *transaction) bool {
		if nowCycle < t.pipelineReadyCycle {
			return false
		}
		return ds.transactionReady(now, t)
	}
}

// traceFail records an MSHR reservation/merge fail at most once per
// transaction, so the metric counts distinct blocked requests rather than
// per-cycle PeekReady re-probes. For mshr-fail it also stamps mshrBlockedAt
// so the directory can measure how long the request waited for a slot.
func (ds *directoryStage) traceFail(
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
	tracing.StartTask(0, 0, 0, ds.cache, kind, op, nil)
}

// emitWaitTime records how long trans waited from its first MSHR-full stall
// until this MSHR slot was finally allocated. No-op if it never stalled.
func (ds *directoryStage) emitWaitTime(
	now akita.VTimeInSec,
	trans *transaction,
) {
	if trans.mshrBlockedAt <= 0 {
		return
	}
	op := "read"
	if trans.write != nil {
		op = "write"
	}
	tracing.StartTask(0, 0, now, ds.cache,
		"mshr-wait-time", op, float64(now-trans.mshrBlockedAt))
}

func (ds *directoryStage) readReady(now akita.VTimeInSec, t *transaction) bool {
	cacheLineID, _ := GetCacheLineID(t.read.Address, ds.cache.log2BlockSize)

	if e := ds.cache.mshr.Query(t.read.PID, cacheLineID); e != nil {
		// A peek-only lookup (forced off-path probe) never coalesces: it is
		// answered as a miss so it forms no wait-edge on a pending MSHR.
		// Prevents the circular dependency two mutual off-path lookups would
		// otherwise create.
		if t.isLookup && t.peekOnly {
			return ds.cache.rdmaSender.CanSend(1)
		}
		if ds.cache.mshr.CanMerge(e) {
			return true
		}
		// A lookup does not coalesce into a full entry and does not stall:
		// it is answered immediately as a miss (handleLookupMiss), gated only
		// on rdmaSender capacity. Only real reads honor the per-entry
		// max-merge cap. Mirrors writebackrcr's non-corner lookup path.
		if t.isLookup {
			return ds.cache.rdmaSender.CanSend(1)
		}
		ds.traceFail(t, now, "mshr-merge-fail", "read")
		return false
	}

	block := ds.cache.directory.Lookup(t.read.PID, cacheLineID)
	if block != nil {
		if t.isLookup {
			return true
		}
		return !block.IsLocked
	}

	if t.isLookup {
		// A pure-miss lookup installs/locks nothing — it only forwards a
		// LookupMissRsp to the RDMA engine (handleLookupMiss). Its sole gate
		// is rdmaSender capacity. Reflect that here so a full rdmaSender marks
		// it not-ready and PeekReady skips it, letting sendable transactions
		// (lookup hits, non-locked local-home reads) behind it run instead of
		// being head-of-line blocked by a lookup that cannot forward yet.
		return ds.cache.rdmaSender.CanSend(1)
	}

	if ds.cache.mshr.IsFull() {
		// Record MSHR-full pressure here — this is the actual gate that
		// stops a missing transaction from advancing. traceFail dedups the
		// count and stamps mshrBlockedAt so mshr-wait-time can be measured.
		ds.traceFail(t, now, "mshr-fail", "read")
		return false
	}
	// Readiness probe only — use the non-mutating selector so repeated
	// per-cycle probes do not demote/rotate hot blocks. The actual cooling
	// happens once, on the committing path (handleReadMiss).
	victim := ds.cache.directory.FindVictimWithoutDemotion(cacheLineID)
	if victim.IsLocked || victim.ReadCount != 0 {
		return false
	}
	// A remote-home miss also needs remote MSHR budget; handleReadMiss refuses
	// it when numToRemote is at the reserve cap. Reflect that gate here so a
	// capped remote-miss is not-ready and PeekReady skips it, letting local-
	// home transactions drain on the reserved slots — otherwise the remote
	// miss sits "ready" at the head, HOL-blocking local drain and defeating
	// the MSHR reservation meant to break the remote circular wait.
	if ds.cache.isReqToRemote(t.read.Address) &&
		!ds.cache.isRemoteReqPossible() {
		return false
	}
	return true
}

func (ds *directoryStage) writeReady(now akita.VTimeInSec, t *transaction) bool {
	cacheLineID, _ := GetCacheLineID(t.write.Address, ds.cache.log2BlockSize)

	if e := ds.cache.mshr.Query(t.write.PID, cacheLineID); e != nil {
		if ds.cache.mshr.CanMerge(e) {
			return true
		}
		ds.traceFail(t, now, "mshr-merge-fail", "write")
		return false
	}

	block := ds.cache.directory.Lookup(t.write.PID, cacheLineID)
	if block != nil {
		return !block.IsLocked && block.ReadCount == 0
	}

	if ds.isWritingFullLine(t.write) {
		// Readiness probe only — non-mutating selector (see readReady).
		victim := ds.cache.directory.FindVictimWithoutDemotion(cacheLineID)
		return !victim.IsLocked && victim.ReadCount == 0
	}

	if ds.cache.mshr.IsFull() {
		// Record MSHR-full pressure here — see readReady. traceFail dedups
		// and stamps mshrBlockedAt for the mshr-wait-time metric.
		ds.traceFail(t, now, "mshr-fail", "write")
		return false
	}
	// Readiness probe only — non-mutating selector (see readReady).
	victim := ds.cache.directory.FindVictimWithoutDemotion(cacheLineID)
	return !victim.IsLocked && victim.ReadCount == 0
}

func (ds *directoryStage) Reset(now akita.VTimeInSec) {
	ds.cache.lookupBuf.Clear()
}

func (ds *directoryStage) doRead(
	now akita.VTimeInSec,
	trans *transaction,
) bool {
	cachelineID, _ := GetCacheLineID(
		trans.read.Address, ds.cache.log2BlockSize)

	// if mshrEntry != nil && !trans.isLookup {
	// 	return ds.handleReadMSHRHit(now, trans, mshrEntry)
	// } else if mshrEntry != nil && trans.isLookup {
	// 	return ds.handleLookupMiss(now, trans)
	// }
	mshrEntry := ds.cache.mshr.Query(trans.read.PID, cachelineID)
	if mshrEntry != nil {
		// A lookup answers as a miss (forward to RDMA) instead of stalling when
		// it cannot coalesce: either the entry is un-mergeable, or it is a
		// peek-only forced off-path probe that must never coalesce to avoid a
		// circular MSHR wait. Real reads still coalesce — readReady gated them
		// on CanMerge. Mirrors rcr.
		if trans.isLookup &&
			(trans.peekOnly || !ds.cache.mshr.CanMerge(mshrEntry)) {
			return ds.handleLookupMiss(now, trans)
		}
		return ds.handleReadMSHRHit(now, trans, mshrEntry)
	}

	block := ds.cache.directory.Lookup(
		trans.read.PID, cachelineID)

	if block != nil {
		if trans.isLookup && block.IsLocked {
			return ds.handleLookupMiss(now, trans)
		}
		return ds.handleReadHit(now, trans, block)
	}

	if trans.isLookup {
		return ds.handleLookupMiss(now, trans)
	}

	return ds.handleReadMiss(now, trans)
}

func (ds *directoryStage) handleReadMSHRHit(
	now akita.VTimeInSec,
	trans *transaction,
	mshrEntry *MSHREntry,
) bool {
	trans.mshrEntry = mshrEntry
	mshrEntry.Requests = append(mshrEntry.Requests, trans)
	// ds.prioritizeLocalReqs(mshrEntry, trans)

	tracing.AddTaskStep(
		tracing.MsgIDAtReceiver(trans.read, ds.cache),
		now, ds.cache,
		"read-mshr-hit",
	)

	if trans.isLookup {
		tracing.StartTask(
			0,
			0,
			now,
			ds.cache,
			"lookup-mshr-hit",
			"",
			trans.read)
	}

	src := trans.read.Meta().Src.Name()
	if strings.Contains(src, "MMU") {
		// tracing.StartTask(
		// "", "", now,
		// ds.cache,
		// "pte-caching-read-mshr-hit",
		// "", nil)
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
	block *Block,
) bool {
	// check if a block is being evicted or written.
	if block.IsLocked {
		return false
	}

	return ds.readFromBank(trans, block, now)
}

func (ds *directoryStage) readFromBank(
	trans *transaction,
	block *Block,
	now akita.VTimeInSec,
) bool {
	numBanks := len(ds.cache.dirToBankBuffers)
	bank := GetBankID(block, ds.cache.directory.WayAssociativity(), numBanks)
	bankBuf := ds.cache.dirToBankBuffers[bank]

	if !bankBuf.CanPushTransReserve(trans.toRemote, smartBufferLocalReserve) {
		return false
	}

	ds.cache.directory.Visit(block) // update LRU list
	block.ReadCount++
	// Probabilistic hotness promotion on read hit (n% chance via
	// -hotness-promote-percent). 0 disables = original behavior.
	ds.cache.directory.PromoteOnHit(block)
	trans.block = block
	trans.action = bankReadHit
	bankBuf.Push(trans)

	tracing.AddTaskStep(
		tracing.MsgIDAtReceiver(trans.read, ds.cache),
		now, ds.cache,
		"read-hit",
	)

	if block.virtualBitForLocalHome {
		tracing.StartTask(
			0,
			0,
			now,
			ds.cache,
			"distribution-hit",
			"",
			nil,
		)
	}

	if trans.isLookup {
		if block.virtualBitForLocalHome {
			tracing.StartTask(
				0,
				0,
				now,
				ds.cache,
				"lookup-distribution-hit",
				"",
				trans.read,
			)
		}

		tracing.StartTask(
			0,
			0,
			now,
			ds.cache,
			"lookup-hit",
			"",
			trans.read)
	}

	src := trans.read.Meta().Src.Name()
	if strings.Contains(src, "MMU") {
		// tracing.StartTask(
		// "", "", now,
		// ds.cache,
		// "pte-caching-read-hit",
		// "", nil)
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

func (ds *directoryStage) handleLookupMiss(
	now akita.VTimeInSec,
	trans *transaction,
) bool {
	if !ds.cache.rdmaSender.CanSend(1) {
		return false
	}

	for i, t := range ds.cache.inFlightTransactions {
		if trans == t {
			ds.cache.inFlightTransactions = append(
				(ds.cache.inFlightTransactions)[:i],
				(ds.cache.inFlightTransactions)[i+1:]...)
		}
	}

	lookupMissRsp := protocol.LookupMissRspBuilder{}.
		WithSendTime(now).
		WithSrc(ds.cache.BottomToRdmaPort).
		WithDst(trans.read.Meta().Src).
		WithRspTo(trans.read.Meta().ID).
		WithInfo(trans.read.Info).
		WithData(nil).
		Build()

	ds.cache.rdmaSender.Send(lookupMissRsp)

	tracing.StartTask(
		0,
		0,
		now,
		ds.cache,
		"lookup-miss",
		"",
		trans.read)

	return true
}

func (ds *directoryStage) handleReadMiss(
	now akita.VTimeInSec,
	trans *transaction,
) bool {
	req := trans.read
	cacheLineID, _ := GetCacheLineID(req.Address, ds.cache.log2BlockSize)

	if ds.cache.mshr.IsFull() {
		ds.traceFail(trans, now, "mshr-fail", "read")
		// return true
		return false
	}
	// Non-mutating selection: hotness demotion/rotation of the passed-over
	// hot blocks is deferred to ApplyCooling and applied ONLY when the
	// eviction actually commits below (ok == true).
	victim, coolList := ds.cache.directory.FindVictimAndPassedOver(cacheLineID)
	if victim.IsLocked || victim.ReadCount > 0 {
		return false
	}

	if ds.needEviction(victim) {
		ok := ds.evict(now, trans, victim)
		if ok {
			ds.applyCoolingWithTrace(now, coolList)
			tracing.AddTaskStep(
				tracing.MsgIDAtReceiver(trans.read, ds.cache),
				now,
				ds.cache,
				"read-miss",
			)

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
		ds.applyCoolingWithTrace(now, coolList)
		tracing.AddTaskStep(
			tracing.MsgIDAtReceiver(trans.read, ds.cache),
			now,
			ds.cache,
			"read-miss",
		)
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
	cachelineID, _ := GetCacheLineID(write.Address, ds.cache.log2BlockSize)

	mshrEntry := ds.cache.mshr.Query(write.PID, cachelineID)
	if mshrEntry != nil {
		ok := ds.doWriteMSHRHit(now, trans, mshrEntry)
		tracing.AddTaskStep(
			tracing.MsgIDAtReceiver(trans.write, ds.cache),
			now, ds.cache,
			"write-mshr-hit",
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
				"write-hit",
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
			"write-miss",
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
	mshrEntry *MSHREntry,
) bool {
	trans.mshrEntry = mshrEntry
	mshrEntry.Requests = append(mshrEntry.Requests, trans)
	// ds.prioritizeLocalReqs(mshrEntry, trans)

	return true
}

func (ds *directoryStage) doWriteHit(
	trans *transaction,
	block *Block,
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
	cachelineID, _ := GetCacheLineID(write.Address, ds.cache.log2BlockSize)

	// Non-mutating selection; cooling deferred to ApplyCooling on commit.
	victim, coolList := ds.cache.directory.FindVictimAndPassedOver(cachelineID)
	if victim.IsLocked || victim.ReadCount > 0 {
		return false
	}

	if ds.needEviction(victim) {
		ok := ds.evict(now, trans, victim)
		if ok {
			ds.applyCoolingWithTrace(now, coolList)
		}
		return ok
	}

	ok := ds.writeToBank(trans, victim)
	if ok {
		ds.applyCoolingWithTrace(now, coolList)
	}
	return ok
}

func (ds *directoryStage) writePartialLineMiss(
	now akita.VTimeInSec,
	trans *transaction,
) bool {
	write := trans.write
	cachelineID, _ := GetCacheLineID(write.Address, ds.cache.log2BlockSize)

	if ds.cache.mshr.IsFull() {
		ds.traceFail(trans, now, "mshr-fail", "write")
		return false
	}

	// Non-mutating selection; cooling deferred to ApplyCooling on commit.
	victim, coolList := ds.cache.directory.FindVictimAndPassedOver(cachelineID)
	if victim.IsLocked || victim.ReadCount > 0 {
		return false
	}

	if ds.needEviction(victim) {
		ok := ds.evict(now, trans, victim)
		if ok {
			ds.applyCoolingWithTrace(now, coolList)
		}
		return ok
	}

	ok := ds.fetch(now, trans, victim)
	if ok {
		ds.applyCoolingWithTrace(now, coolList)
	}
	return ok
}

func (ds *directoryStage) writeToBank(
	trans *transaction,
	block *Block,
) bool {
	numBanks := len(ds.cache.dirToBankBuffers)
	bank := GetBankID(block, ds.cache.directory.WayAssociativity(), numBanks)
	bankBuf := ds.cache.dirToBankBuffers[bank]

	if !bankBuf.CanPushTransReserve(trans.toRemote, smartBufferLocalReserve) {
		return false
	}

	addr := trans.write.Address
	cachelineID, _ := GetCacheLineID(addr, ds.cache.log2BlockSize)

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
	victim *Block,
) bool {
	bankNum := GetBankID(victim,
		ds.cache.directory.WayAssociativity(),
		len(ds.cache.dirToBankBuffers))
	bankBuf := ds.cache.dirToBankBuffers[bankNum]

	if !bankBuf.CanPushTransReserve(trans.toRemote, smartBufferLocalReserve) {
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

	cacheLineID, _ := GetCacheLineID(addr, ds.cache.log2BlockSize)

	trans.action = bankEvictAndFetch
	// literally, "victim" which should be evict from current l2 cache.
	// Capture the full restore-state (Hotness/IsValid/IsDirty included)
	// so write-back stage can compare hotness on the fetch return and
	// restore the slot if it bypasses install.
	trans.victim = snapshotVictim(victim)

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

		trans.mshrEntry = mshrEntry
		ds.emitWaitTime(now, trans)
		trans.fetchPID = pid
		trans.fetchAddress = cacheLineID
		trans.action = bankEvictAndFetch
	} else {
		trans.action = bankEvictAndWrite
	}

	// delay the overwrite of tag and etc at writebufferstage.

	victim.Tag = cacheLineID
	victim.PID = pid
	victim.IsLocked = true
	victim.IsDirty = false
	victim.InstallPC = tracing.PCFromReq(req)
	// NOTE: victim.Hotness is intentionally NOT reset here. The
	// resident slot's hotness must survive the in-flight period so
	// the write-back stage can compare it against the incoming line
	// and bypass install when the resident outranks. The new line's
	// starting hotness (Cold for DRAM/RDMA-MSHR fetches) is applied
	// at install-commit time by installFetchedLine.

	// at this point, the victim cacheline is no more distributed one.
	victim.virtualBitForLocalHome = false

	bankBuf.Push(trans)

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
	block *Block,
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
	cacheLineID, _ := GetCacheLineID(addr, ds.cache.log2BlockSize)

	bankNum := GetBankID(block,
		ds.cache.directory.WayAssociativity(), len(ds.cache.dirToBankBuffers))
	bankBuf := ds.cache.dirToBankBuffers[bankNum]

	if !bankBuf.CanPushTransReserve(trans.toRemote, smartBufferLocalReserve) {
		return false
	}

	// if the request goes to remote chiplet, it could make deadlock
	if ds.cache.isReqToRemote(addr) {
		if !ds.cache.isRemoteReqPossible() {
			return false
		}
		ds.cache.numToRemote++
	}

	mshrEntry := ds.cache.mshr.Add(pid, cacheLineID)
	trans.mshrEntry = mshrEntry
	ds.emitWaitTime(now, trans)
	trans.block = block
	// Snapshot the resident slot BEFORE we clobber its Tag/PID. The
	// write-back stage needs the original Tag/PID/Hotness to restore
	// the slot if it decides to bypass install on the fetch return.
	// The eviction path handles its snapshot inside evict(); this is
	// the no-eviction counterpart.
	trans.victim = snapshotVictim(block)
	block.IsLocked = true

	// delay the update of tag and etc at writebufferstage.
	block.Tag = cacheLineID
	block.PID = pid
	block.IsValid = true
	block.InstallPC = tracing.PCFromReq(req)

	// at this point, the resident slot is no more distributed one.
	block.virtualBitForLocalHome = false

	ds.cache.directory.Visit(block)

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
	victim *Block,
) bool {
	return victim.IsValid && victim.IsDirty
}

// applyCoolingWithTrace traces hotness demotion for distribution-installed
// blocks (virtualBitForLocalHome == true) before delegating to ApplyCooling.
func (ds *directoryStage) applyCoolingWithTrace(
	now akita.VTimeInSec,
	coolList []*Block,
) {
	for _, b := range coolList {
		if b.Hotness < protocol.Warm || !b.virtualBitForLocalHome {
			continue
		}
		tracing.StartTask(0, 0, now, ds.cache,
			"distribution-trace", "",
			&tracing.DistInfo{
				Addr:     b.Tag,
				PC:       b.InstallPC,
				Home:     ds.cache.HomeNodeFinder.GetChipletId(b.Tag),
				Hotness:  b.Hotness - 1, // value after demotion
				DistType: tracing.TYPE_HOTNESS_DEMOTE_COOLING,
			})
	}
	ds.cache.directory.ApplyCooling(coolList)
}
