package writebackrcr

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

// Tick selects and processes at most one transaction per cycle. Selection is
// driven by transPriorityKey (lookupbuffer.go), a 2-axis key:
//
//  1. toRemote=false first — this chiplet is the home node for the address.
//     Home-served transactions are the cycle-breakers of the NearFetch all-
//     to-all wait, because they generate the DataReady that unblocks a
//     remote requester.
//  2. Within the same destination tier, a non-corner lookup probe wins. It
//     can always answer (handleReadHit / handleReadMSHRHit / handleLookupMiss)
//     without blocking on MSHR allocation or remote budget, so it advances
//     the chain even when general reads are wedged.
//  3. Lock-readiness is NOT a sort key — it lives in readReady/writeReady, so
//     PeekReady automatically skips locked-block transactions and a later
//     unlocked one of the same priority bypasses it.
//  4. Ties break by lowest transaction ID (oldest first) — transOrderKey.
//
// Tick collects every deadline-ready candidate across all three lookup
// buffers, sorts by (priority, id), and tries them in order until one
// succeeds. The fall-through is the safety net for readReady's blind spot:
// readReady does not consult dirToBank capacity, so a picked transaction can
// still fail in doRead. Without fall-through that failure would let the
// cache return false with a higher-priority lookup sitting unattempted, the
// engine quiesces, and the all-to-all wait keeps the component asleep.
// Returns true on the first successful commit (one transaction per cycle
// guarantee preserved).
func (ds *directoryStage) Tick(now akita.VTimeInSec) bool {
	buf := ds.cache.lookupBuf.(*LookupBuffer)
	ready := ds.readyWithDeadline(now)

	// buf is (priority, id)-sorted by transPriorityKey (local-home first, then
	// remote, then non-corner lookup, then corner-serve), so iterating in order
	// serves the highest-priority ready transaction first. Fall through on a
	// failed commit (downstream buffer full) to the next ready one. Return the
	// REAL progress: a ready-but-all-blocked tick must return false so the
	// engine can quiesce and the deadlock detector fires, instead of spinning
	// forever on a downstream that never frees.
	for i := 0; i < len(buf.elements); i++ {
		trans := buf.elements[i].trans
		if !ready(trans) {
			continue
		}
		var ok bool
		if trans.read != nil {
			ok = ds.doRead(now, trans)
		} else {
			ok = ds.doWrite(now, trans)
		}
		if ok {
			buf.RemoveAt(i)
			return true
		}
	}
	return false
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

// readyWithDeadline gates each transaction by its stamped
// pipelineReadyCycle, mirroring the deadline drain in writeback/.
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
// transaction so the metric counts distinct blocked requests rather
// than per-cycle PeekReady re-probes. For mshr-fail it also stamps
// mshrBlockedAt so the directory can measure how long the request waited
// for a slot. Mirrors writeback's helper.
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
	cacheLineID, _ := cache.GetCacheLineID(t.read.Address, ds.cache.log2BlockSize)

	if e := ds.cache.mshr.Query(t.read.PID, cacheLineID); e != nil {
		if ds.cache.mshr.CanMerge(e) {
			return true
		}
		// A non-corner lookup probe does not coalesce: an un-mergeable in-flight
		// entry is answered immediately as a forwarding miss (handleLookupMiss),
		// so it is ready without needing an MSHR slot — same as the directory-miss
		// probe path below. doRead routes it to handleLookupMiss.
		if t.isLookup && !t.isCornerNode {
			return true
		}
		// Entry is at its max-merge cap — cannot coalesce another request.
		// Stall and retry, mirroring the IsFull gate below.
		ds.traceFail(t, now, "mshr-merge-fail", "read")
		return false
	}

	block := ds.cache.directory.Lookup(t.read.PID, cacheLineID)
	if block != nil {
		// A transit probe (non-corner lookup) only checks presence and forwards;
		// it never reads the block. A locked block here is held by THIS hop's own
		// coalesce (virtual MSHR) or an eviction — the data is not landing here, so
		// waiting is pointless AND is the chain-stall deadlock: the probe can't
		// reach the home while the lock holder's coalesce waits on that very chain.
		// Forward past it. (A real in-flight fetch is already handled by the
		// mshr.Query merge above, so it never reaches this locked-block case.)
		if t.isLookup && !t.isCornerNode {
			return true
		}
		return !block.IsLocked
	}

	// Lookup-traffic that hasn't reached the corner node is a pure probe:
	// a miss here is answered immediately by handleLookupMiss without
	// allocating an MSHR entry, so don't gate it on MSHR capacity.
	if t.isLookup && !t.isCornerNode {
		return true
	}

	// Corner-node-coalesced misses delegate the fetch to the corner, but the
	// requestor still allocates its OWN real MSHR (paper: intra-module reply
	// tracking). So they ARE gated on mshr-full: this is the flow-control bound
	// that caps outstanding delegations at the MSHR capacity and keeps the request
	// network from flooding (the deadlock we hit with the old unbounded virtual
	// MSHR). They do not consume remote budget — the real remote fetch happens at
	// the corner.
	if ds.cache.hasCornerNode(t.read.Address) {
		if ds.cache.mshr.IsFull() {
			ds.traceFail(t, now, "mshr-fail", "read")
			return false
		}
		victim := ds.cache.directory.FindVictim(cacheLineID)
		if victim.IsLocked || victim.ReadCount != 0 {
			return false
		}
		// The coalesce this read will issue must be able to enter dirToBank.
		// If it can't, do NOT report ready — otherwise it is picked, fails in
		// doRead, and (being lower priority) it would also have blocked a real
		// fetch that CAN enter. Mirror the class-aware admission used at push.
		bank := cache.GetBankID(victim,
			ds.cache.directory.WayAssociativity(), len(ds.cache.dirToBankBuffers))
		return ds.cache.dirToBankBuffers[bank].CanPushTransClassR(
			true, true, dirToBankLocalReserve, dirToBankCoalesceReserve)
	}

	if ds.cache.mshr.IsFull() {
		// Record MSHR-full pressure here — this is the actual gate that
		// stops a missing transaction from advancing.
		ds.traceFail(t, now, "mshr-fail", "read")
		return false
	}
	victim := ds.cache.directory.FindVictim(cacheLineID)
	if victim.IsLocked || victim.ReadCount != 0 {
		return false
	}
	// A remote-home miss also needs remote MSHR budget; fetch refuses it when
	// numToRemote is at the reserve cap. Reflect that gate here so a capped
	// remote-miss is not-ready and PeekReady skips it, letting local-home and
	// incoming-remote transactions drain on the reserved slots — otherwise the
	// remote miss sits "ready" and HOL-blocks local drain, defeating the MSHR
	// reservation meant to break the remote circular wait. Mirrors
	// writeback-dynamic-dist's readReady. A corner-node lookup serve draws from
	// the dedicated corner-serve reserve so a generic-remote flood cannot wall
	// it off and stall the NearFetch chain.
	if ds.cache.isReqToRemote(t.read.Address) {
		possible := ds.cache.isRemoteReqPossible()
		if t.isLookup && t.isCornerNode {
			possible = ds.cache.isCornerServePossible()
		}
		if !possible {
			return false
		}
	}
	return true
}

func (ds *directoryStage) writeReady(now akita.VTimeInSec, t *transaction) bool {
	cacheLineID, _ := cache.GetCacheLineID(t.write.Address, ds.cache.log2BlockSize)

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
		victim := ds.cache.directory.FindVictim(cacheLineID)
		return !victim.IsLocked && victim.ReadCount == 0
	}

	if ds.cache.mshr.IsFull() {
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

	mshrEntry := ds.cache.mshr.Query(trans.read.PID, cachelineID)
	if mshrEntry != nil {
		// Defensive recheck: readReady gated on CanMerge but PeekReady runs
		// before dirStage commits, so another transaction in the same tick
		// may have filled the entry to its cap. Stall here as well.
		if !ds.cache.mshr.CanMerge(mshrEntry) {
			// A non-corner lookup is a pure probe: an un-mergeable in-flight
			// entry is not a forwarding hit (data not resident, slot full), so
			// answer it as a forwarding miss and push the probe to the next hop
			// instead of stalling. Corner-node (and non-lookup) requests still
			// stall to wait for merge capacity.
			if trans.isLookup && !trans.isCornerNode {
				return ds.handleLookupMiss(now, trans)
			}
			ds.traceFail(trans, now, "mshr-merge-fail", "read")
			return false
		}
		return ds.handleReadMSHRHit(now, trans, mshrEntry)
	}

	block := ds.cache.directory.Lookup(
		trans.read.PID, cachelineID)

	if block != nil {
		// Transit probe past a locked block: forward instead of trying to read it
		// (handleReadHit would fail on the lock and re-stall every cycle). Mirrors
		// readReady — the lock is a coalesce/eviction, data is not landing here.
		if trans.isLookup && !trans.isCornerNode && block.IsLocked {
			return ds.handleLookupMiss(now, trans)
		}
		return ds.handleReadHit(now, trans, block)
	}

	if trans.isLookup && !trans.isCornerNode {
		return ds.handleLookupMiss(now, trans)
	}

	return ds.handleReadMiss(now, trans)
}

func (ds *directoryStage) handleLookupMiss(
	now akita.VTimeInSec,
	trans *transaction,
) bool {
	if !ds.cache.lookupSender.CanSend(1) {
		return false
	}

	// delete the rcr transaction from inflightTransaction
	for i, t := range ds.cache.inFlightTransactions {
		if trans == t {
			ds.cache.inFlightTransactions = append(
				(ds.cache.inFlightTransactions)[:i],
				(ds.cache.inFlightTransactions)[i+1:]...)
		}
	}

	lookupMissRsp := protocol.LookupRspBuilder{}.
		WithSendTime(now).
		WithSrc(ds.cache.RDMAPort).
		WithDst(trans.read.Meta().Src).
		WithRspTo(trans.read.Meta().ID).
		WithData(nil).
		Build()

	// send response to rdma engine. (use rcr sender)
	ds.cache.lookupSender.Send(lookupMissRsp)

	tracing.StartTask(
		0,
		0,
		0,
		ds.cache,
		"data-forwarding-miss",
		"",
		&tracing.DataForwardingDetail{IsPtw: trans.read.IsPtw, PC: tracing.PCFromInfo(trans.read.Info)})

	return true
}

func (ds *directoryStage) handleReadMSHRHit(
	now akita.VTimeInSec,
	trans *transaction,
	mshrEntry *cache.MSHREntry,
) bool {
	trans.mshrEntry = mshrEntry
	mshrEntry.Requests = append(mshrEntry.Requests, trans)
	// ds.prioritizeLocalReqs(mshrEntry, trans)

	tracing.AddTaskStep(
		tracing.MsgIDAtReceiver(trans.read, ds.cache),
		now, ds.cache,
		"read-mshr-hit",
	)

	if trans.read != nil &&
		strings.Contains(trans.read.Meta().Src.Name(), "MMU") {
		// tracing.StartTask(
		// "", "", now,
		// ds.cache,
		// "pte-caching-read-mshr-hit",
		// "", nil)
	}

	if trans.isLookup {
		tracing.StartTask(
			0,
			0,
			0,
			ds.cache,
			"data-forwarding-mshr-hit",
			"",
			&tracing.DataForwardingDetail{Hop: trans.hop, IsPtw: trans.read.IsPtw, PC: tracing.PCFromInfo(trans.read.Info)})
	}

	ds.emitReadMemoryTrace(now, trans, 0)
	return true
}

// emitReadMemoryTrace emits the per-access memory-trace task consumed by the
// LocalRemoteTracer / memory trace — ONLY for an origin CU read (fromLocal), so
// each access is counted once at the requestor (a remote home-serve or a
// lookup-chain probe is not fromLocal). typeCode is the access kind (0=mshr-hit,
// 1=read hit, 2=read miss). Mirrors writeback/.
func (ds *directoryStage) emitReadMemoryTrace(
	now akita.VTimeInSec, trans *transaction, typeCode uint64,
) {
	if !trans.fromLocal {
		return
	}
	isVec := uint64(0)
	if strings.Contains(trans.read.Meta().Src.Name(), "L1V") {
		isVec = 1
	}
	tracing.StartTask(typeCode, isVec, now, ds.cache, "memory-trace",
		ds.cache.chipletIDStrings[ds.cache.HomeNodeFinder.GetChipletId(trans.read.Address)],
		trans.read)
}

// emitWriteMemoryTrace is emitReadMemoryTrace for writes (typeCode 3/4/5).
func (ds *directoryStage) emitWriteMemoryTrace(
	now akita.VTimeInSec, trans *transaction, typeCode uint64,
) {
	if !trans.fromLocal {
		return
	}
	isVec := uint64(0)
	if strings.Contains(trans.write.Meta().Src.Name(), "L1V") {
		isVec = 1
	}
	tracing.StartTask(typeCode, isVec, now, ds.cache, "memory-trace",
		ds.cache.chipletIDStrings[ds.cache.HomeNodeFinder.GetChipletId(trans.write.Address)],
		trans.write)
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

	if !bankBuf.CanPushTransClassR(trans.toRemote, false,
		dirToBankLocalReserve, dirToBankCoalesceReserve) {
		return false
	}

	ds.cache.directory.Visit(block) // update LRU list
	block.ReadCount++
	trans.block = block
	trans.action = bankReadHit
	bankBuf.Push(trans)

	tracing.AddTaskStep(
		tracing.MsgIDAtReceiver(trans.read, ds.cache),
		now, ds.cache,
		"read-hit",
	)

	if trans.read != nil &&
		strings.Contains(trans.read.Meta().Src.Name(), "MMU") {
		// tracing.StartTask(
		// "", "", now,
		// ds.cache,
		// "pte-caching-read-hit",
		// "", nil)
	}

	if trans.isLookup {
		tracing.StartTask(
			0,
			0,
			0,
			ds.cache,
			"data-forwarding-hit",
			"",
			&tracing.DataForwardingDetail{Hop: trans.hop, IsPtw: trans.read.IsPtw, PC: tracing.PCFromInfo(trans.read.Info)})

		if !trans.read.IsPtw {
			tracing.StartTask(0, 0, 0, ds.cache, tracing.CachelineHopKind, "",
				&tracing.CachelineHopDetail{Addr: trans.read.Address, Hop: uint64(trans.hop)})
		}
	}

	ds.emitReadMemoryTrace(now, trans, 1)
	return true
}

func (ds *directoryStage) handleReadMiss(
	now akita.VTimeInSec,
	trans *transaction,
) bool {
	req := trans.read
	cacheLineID, _ := cache.GetCacheLineID(req.Address, ds.cache.log2BlockSize)

	// Corner-node misses use a virtual MSHR (fetch/evict handle it), so they
	// must not be blocked when the real MSHR table is full.
	if !ds.cache.hasCornerNode(req.Address) && ds.cache.mshr.IsFull() {
		ds.traceFail(trans, now, "mshr-fail", "read")
		return false
	}

	victim := ds.cache.directory.FindVictim(cacheLineID)
	if victim.IsLocked || victim.ReadCount > 0 {
		return false
	}

	if ds.needEviction(victim) {
		ok := ds.evict(now, trans, victim)
		if ok {
			tracing.AddTaskStep(
				tracing.MsgIDAtReceiver(trans.read, ds.cache),
				now,
				ds.cache,
				"read-miss",
			)

			if trans.read != nil &&
				strings.Contains(trans.read.Meta().Src.Name(), "MMU") {
				// tracing.StartTask(
				// "", "", now,
				// ds.cache,
				// "pte-caching-read-miss",
				// "", nil)
			}

			ds.emitReadMemoryTrace(now, trans, 2)
		}

		return ok
	}

	ok := ds.fetch(now, trans, victim)
	if ok {
		tracing.AddTaskStep(
			tracing.MsgIDAtReceiver(trans.read, ds.cache),
			now,
			ds.cache,
			"read-miss",
		)

		if trans.read != nil &&
			strings.Contains(trans.read.Meta().Src.Name(), "MMU") {
			// tracing.StartTask(
			// "", "", now,
			// ds.cache,
			// "pte-caching-read-miss",
			// "", nil)
		}

		ds.emitReadMemoryTrace(now, trans, 2)
	}
	return ok
}

func (ds *directoryStage) doWrite(
	now akita.VTimeInSec,
	trans *transaction,
) bool {
	write := trans.write
	cachelineID, _ := cache.GetCacheLineID(write.Address, ds.cache.log2BlockSize)

	mshrEntry := ds.cache.mshr.Query(write.PID, cachelineID)
	if mshrEntry != nil {
		if !ds.cache.mshr.CanMerge(mshrEntry) {
			ds.traceFail(trans, now, "mshr-merge-fail", "write")
			return false
		}
		ok := ds.doWriteMSHRHit(now, trans, mshrEntry)
		tracing.AddTaskStep(
			tracing.MsgIDAtReceiver(trans.write, ds.cache),
			now, ds.cache,
			"write-mshr-hit",
		)

		ds.emitWriteMemoryTrace(now, trans, 3)
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

			ds.emitWriteMemoryTrace(now, trans, 4)
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

		ds.emitWriteMemoryTrace(now, trans, 5)
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

	if !bankBuf.CanPushTransClassR(trans.toRemote, false,
		dirToBankLocalReserve, dirToBankCoalesceReserve) {
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

	// coalesce iff this eviction will issue a corner-node-coalesced fetch.
	if !bankBuf.CanPushTransClassR(trans.toRemote,
		ds.evictionNeedFetch(trans) && ds.cache.hasCornerNode(addr),
		dirToBankLocalReserve, dirToBankCoalesceReserve) {
		return false
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

	// If the trans is read miss or partial line write miss, fetch the data
	// If the trans is full-line write miss, no fetch.
	// Additionally, if the corner node is available, do not make MSHR entry.
	// MSHR entry will be made at the corner node.
	if ds.evictionNeedFetch(trans) {
		// if there is no corner node, mshr coalescing is impossible
		if !ds.cache.hasCornerNode(addr) {
			// if the request goes to remote chiplet, it can make deadlock.
			// A corner-node lookup serve draws from the corner-serve reserve.
			if ds.cache.isReqToRemote(addr) {
				possible := ds.cache.isRemoteReqPossible()
				if trans.isLookup && trans.isCornerNode {
					possible = ds.cache.isCornerServePossible()
				}
				if !possible {
					return false
				}
				ds.cache.acquireRemoteBudget(trans)
			}
			mshrEntry := ds.cache.mshr.Add(pid, cacheLineID)
			mshrEntry.Block = victim
			mshrEntry.Requests = append(mshrEntry.Requests, trans)
			// ds.prioritizeLocalReqs(mshrEntry, trans)
			trans.mshrEntry = mshrEntry
			ds.emitWaitTime(now, trans)

			trans.fetchPID = pid
			trans.fetchAddress = cacheLineID
			trans.action = bankEvictAndFetch
		} else {
			// Requestor delegates the fetch to the corner but allocates its OWN
			// real MSHR (intra-module reply tracking, flow-control bound). gated by
			// mshr-full in readReady.
			mshrEntry := ds.cache.mshr.Add(pid, cacheLineID)
			mshrEntry.Block = victim
			mshrEntry.Requests = append(mshrEntry.Requests, trans)
			trans.mshrEntry = mshrEntry
			ds.emitWaitTime(now, trans)

			// same process
			trans.fetchPID = pid
			trans.fetchAddress = cacheLineID
			trans.action = bankEvictAndMshrCoalesce
		}
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

	tracing.StartTask(0, 0, now, ds.cache,
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

	if !bankBuf.CanPushTransClassR(trans.toRemote,
		ds.cache.hasCornerNode(addr), dirToBankLocalReserve, dirToBankCoalesceReserve) {
		return false
	}

	trans.fetchPID = pid
	trans.fetchAddress = cacheLineID

	// if there is no corner node, mshr coalescing is impossible
	if !ds.cache.hasCornerNode(addr) {
		// if the request goes to remote chiplet, it could make deadlock.
		// A corner-node lookup serve draws from the corner-serve reserve.
		if ds.cache.isReqToRemote(addr) {
			possible := ds.cache.isRemoteReqPossible()
			if trans.isLookup && trans.isCornerNode {
				possible = ds.cache.isCornerServePossible()
			}
			if !possible {
				return false
			}
			ds.cache.acquireRemoteBudget(trans)
		}

		mshrEntry := ds.cache.mshr.Add(pid, cacheLineID)
		trans.mshrEntry = mshrEntry
		ds.emitWaitTime(now, trans)

		tracing.AddTaskStep(
			tracing.MsgIDAtReceiver(req, ds.cache),
			now,
			ds.cache,
			fmt.Sprintf("add-mshr-entry-0x%x-0x%x", mshrEntry.Address, block.Tag),
		)

		trans.action = writeBufferFetch

		mshrEntry.Block = block
		mshrEntry.Requests = append(mshrEntry.Requests, trans)
		// ds.prioritizeLocalReqs(mshrEntry, trans)
	} else {
		// Requestor delegates the fetch to the corner but allocates its OWN real
		// MSHR (intra-module reply tracking). gated by mshr-full in readReady, so
		// outstanding delegations are bounded by MSHR capacity = flow control.
		mshrEntry := ds.cache.mshr.Add(pid, cacheLineID)
		trans.mshrEntry = mshrEntry
		ds.emitWaitTime(now, trans)

		trans.action = writeBufferMshrCoalesce

		mshrEntry.Block = block
		mshrEntry.Requests = append(mshrEntry.Requests, trans)
	}

	if block.IsValid && !block.IsDirty {
		victimAddr := block.Tag
		victimPC := block.InstallPC
		victimHome := ds.cache.chipletIDStrings[ds.cache.HomeNodeFinder.GetChipletId(victimAddr)]

		tracing.StartTask(0, 0, now, ds.cache,
			"overwrite",
			victimHome,
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

	block.IsLocked = true
	block.IsValid = true
	trans.block = block
	block.Tag = cacheLineID
	block.PID = pid
	block.InstallPC = tracing.PCFromReq(req)
	ds.cache.directory.Visit(block)

	bankBuf.Push(trans)

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

// Used to prevent deadlock.
func (ds *directoryStage) prioritizeLocalReqs(
	mshrEntry *cache.MSHREntry,
	trans *transaction,
) {
	if !trans.toRemote {
		for i, t := range mshrEntry.Requests {
			if !t.(*transaction).toRemote {
				mshrEntry.Requests = append(
					append(mshrEntry.Requests[:i], trans),
					mshrEntry.Requests[i:]...)
				return
			}
		}
	}
	mshrEntry.Requests = append(mshrEntry.Requests, trans)
}
