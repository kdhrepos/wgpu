package writebackrcr

import (
	// "fmt"

	"fmt"
	"strings"

	"gitlab.com/akita/akita"
	"gitlab.com/akita/mem/cache"
	"gitlab.com/akita/mem/idealmemcontroller"
	"gitlab.com/akita/protocol"
	"gitlab.com/akita/util/akitaext"
	"gitlab.com/akita/util/global"
	"gitlab.com/akita/util/tracing"
)

type writeBufferStage struct {
	cache *Cache

	writeBufferCapacity int
	maxInflightFetch    int
	maxInflightEviction int

	// pendingEvictions []*transaction
	// inflightFetch    []*transaction
	// inflightEviction []*transaction

	pendingEvictions *SeqBuffer
	inflightFetch    *SeqBuffer
	inflightEviction *SeqBuffer
}

func (wb *writeBufferStage) Tick(now akita.VTimeInSec) bool {
	madeProgress := false

	madeProgress = wb.processReturnRspFromDram(now) || madeProgress
	madeProgress = wb.processReturnRspFromRdma(now) || madeProgress
	madeProgress = wb.processNewTransaction(now) || madeProgress
	madeProgress = wb.write(now) || madeProgress

	return madeProgress
}

// writing evictions to lower memory
func (wb *writeBufferStage) write(now akita.VTimeInSec) bool {
	if wb.pendingEvictions.Size() == 0 {
		return false
	}

	trans := wb.pendingEvictions.Peek().(*transaction)

	if wb.tooManyInflightEvictions() {
		return false
	}
	lowModulePort := wb.cache.lowModuleFinder.Find(trans.evictingAddr)

	var bottomPort akita.Port
	var bottomSender akitaext.BufferedSender
	if strings.Contains(lowModulePort.Name(), "RDMA") {
		bottomPort = wb.cache.BottomToRdmaPort
		bottomSender = wb.cache.bottomToRdmaSender
		trans.toRemote = true
	} else {
		bottomPort = wb.cache.BottomToDramPort
		bottomSender = wb.cache.bottomToDramSender
		trans.toRemote = false
	}

	if !bottomSender.CanSend(1) {
		// bottomPort.(*akita.LimitNumMsgPort).HeadToTail()
		// return true
		return false
	}

	write := protocol.WriteReqBuilder{}.
		WithSrc(bottomPort).
		WithDst(lowModulePort).
		WithPID(trans.evictingPID).
		WithAddress(trans.evictingAddr).
		WithData(trans.evictingData).
		WithDirtyMask(trans.evictingDirtyMask).
		Build()

	if bottomPort == wb.cache.BottomToRdmaPort {
		bottomSender.Send(write)
	} else {
		toDramMsg := &idealmemcontroller.ToDramMsg{
			Msg:       write,
			FromLocal: true,
		}
		bottomSender.Send(toDramMsg)
	}

	if global.GetInstance().GetVerbose() {
		fmt.Println("write to flush",
			bottomPort.Name(),
			lowModulePort.Name(),
			write.ID)
	}

	// wb.pendingEvictions = wb.pendingEvictions[1:]
	// wb.inflightEviction = append(wb.inflightEviction, trans)
	trans.evictionWriteReq = write
	wb.pendingEvictions.Pop()
	wb.inflightEviction.Push(trans)

	tracing.TraceReqInitiate(write, now, wb.cache,
		tracing.MsgIDAtReceiver(trans.req(), wb.cache))

	return true
}

func (wb *writeBufferStage) processNewTransaction(now akita.VTimeInSec) bool {
	// Fall-through over the priority-ordered heap: a coalesce at the head that is
	// blocked on inflightFetch (coalesce-band full) must NOT stall a later real
	// fetch / local-home / evict transaction. Each action handler removes its own
	// transaction by value on success, so committing a non-head entry is safe.
	for _, e := range wb.cache.writeBufferBuffer.elements {
		trans := e.value.(*transaction)
		var ok bool
		switch trans.action {
		// here, mshr coalesced request is processed
		case writeBufferFetch:
			ok = wb.processWriteBufferFetch(now, trans)
		case writeBufferEvictAndWrite:
			ok = wb.processWriteBufferEvictAndWrite(now, trans)
		case writeBufferEvictAndFetch:
			ok = wb.processWriteBufferFetchAndEvict(now, trans)
		case writeBufferFlush:
			ok = wb.processWriteBufferFlush(now, trans, true)
		case writeBufferMshrCoalesce:
			ok = wb.processWriteBufferMshrCoalesce(now, trans)
		case writeBufferEvictAndMshrCoalesce:
			ok = wb.processWriteBufferEvictAndMshrCoalesce(now, trans)
		default:
			panic("unknown transaction action")
		}
		if ok {
			return true
		}
	}
	return false
}

func (wb *writeBufferStage) processWriteBufferMshrCoalesce(
	now akita.VTimeInSec,
	trans *transaction,
) bool {
	return wb.mshrCoalesceFromBottom(now, trans)
}

func (wb *writeBufferStage) processWriteBufferEvictAndMshrCoalesce(
	now akita.VTimeInSec,
	trans *transaction,
) bool {
	ok := wb.processWriteBufferFlush(now, trans, false)
	if ok {
		trans.action = writeBufferMshrCoalesce
		return true
	}

	return false
}

func (wb *writeBufferStage) processWriteBufferFetch(
	now akita.VTimeInSec,
	trans *transaction,
) bool {
	// The stage first tries to find the needed data locally from evictions of the same line.
	if wb.findDataLocally(trans) {
		// If data is found locally, the stage prepares a bankWriteFetched for the owning bank,
		// merges pending writes via the MSHR entry, and removes the MSHR entry.
		return wb.sendFetchedDataToBank(now, trans)
	}

	// Otherwise, it issues a read to lower memory.
	return wb.fetchFromBottom(now, trans)
}

func (wb *writeBufferStage) findDataLocally(trans *transaction) bool {
	for _, e := range wb.inflightEviction.elements {
		elem := e.(*transaction)
		if elem.evictingAddr == trans.fetchAddress {
			trans.fetchedData = elem.evictingData
			return true
		}
	}

	for _, e := range wb.pendingEvictions.elements {
		elem := e.(*transaction)
		if elem.evictingAddr == trans.fetchAddress {
			trans.fetchedData = elem.evictingData
			return true
		}
	}
	return false
}

func (wb *writeBufferStage) sendFetchedDataToBank(
	now akita.VTimeInSec,
	trans *transaction,
) bool {
	if !wb.cache.mshrStageBuffer.CanPush() {
		trans.fetchedData = nil
		return false
	}

	trans.action = bankWriteFetched
	trans.mshrEntry.Data = trans.fetchedData
	trans.action = bankWriteFetched
	wb.combineData(trans.mshrEntry)

	if trans.read != nil {
		if wb.cache.isReqToRemote(trans.read.Address) {
			wb.cache.releaseRemoteBudget(trans)
		}
	} else {
		if wb.cache.isReqToRemote(trans.write.Address) {
			wb.cache.releaseRemoteBudget(trans)
		}
	}

	mshrEntry := trans.mshrEntry
	block := mshrEntry.Block
	block.IsLocked = false
	block.IsValid = true

	wb.cache.mshr.Remove(trans.mshrEntry.PID, trans.mshrEntry.Address)

	// Emit the merge-size sample so MSHRFailTracer.mergeHistogram and
	// per-source totals match the writeback baseline.
	tracing.StartTask(0, 0, now, wb.cache,
		"mshr-merge", "buffer", len(mshrEntry.Requests))

	wb.cache.mshrStageBuffer.Push(mshrEntry) // wake up the mshr entry
	wb.cache.storage.Write(block.CacheAddress, mshrEntry.Data)

	wb.cache.writeBufferBuffer.RemoveByValue(trans)
	return true
}

func (wb *writeBufferStage) mshrCoalesceFromBottom(
	now akita.VTimeInSec,
	trans *transaction,
) bool {
	// coalesce is always a remote delegation (defers the fetch to the
	// corner/home and waits on a remote response). Keep BOTH the local and the
	// remote non-coalesce reserves free.
	if wb.inflightFetchBlocked(true, true) {
		return false
	}

	if !wb.cache.bottomToRdmaSender.CanSend(1) {
		return false
	}

	lowModulePort := wb.cache.lowModuleFinder.Find(trans.fetchAddress)

	trans.toRemote = true
	trans.mshrEntry.FetchedRemote = true

	var mshrOrigInfo interface{}
	if trans.read != nil {
		mshrOrigInfo = trans.read.Info
	}
	mshrCoalesce := protocol.ReadReqBuilder{}.
		WithSrc(wb.cache.BottomToRdmaPort).
		WithDst(lowModulePort). // this could be DRAM Top or RDMA ToL2
		WithPID(trans.fetchPID).
		WithAddress(trans.fetchAddress).
		WithByteSize(1 << wb.cache.log2BlockSize).
		WithInfo(mshrOrigInfo).
		Build()
	mshrCoalesce.HopTraceID = trans.hopTraceID // continue origin hop-trace task
	mshrCoalesce.HopBase = trans.hop           // cumulative requestor->corner hops

	wb.cache.bottomToRdmaSender.Send(mshrCoalesce)

	trans.mshrCoalesceReq = mshrCoalesce
	wb.inflightFetch.Push(trans)

	tracing.AddRemoteLLCMiss()

	wb.cache.writeBufferBuffer.RemoveByValue(trans)

	tracing.StartTask(
		0,
		0,
		0,
		wb.cache,
		"data-forwarding-mshr-coalesced",
		"",
		nil)

	return true
}

func (wb *writeBufferStage) fetchFromBottom(
	now akita.VTimeInSec,
	trans *transaction,
) bool {
	// At this point, lmf tells whether this address
	// goes to the remote chiplet or local DRAM. Resolve it BEFORE the inflight
	// admission test so local-DRAM fetches (the serve-read sink that produces
	// the DataReady unblocking a remote requester) get full capacity while a
	// remote-bound non-coalesce fetch must leave the local reserve free.
	lowModulePort := wb.cache.lowModuleFinder.Find(trans.fetchAddress)

	var bottomPort akita.Port
	var bottomSender akitaext.BufferedSender
	if strings.Contains(lowModulePort.Name(), "RDMA") {
		bottomPort = wb.cache.BottomToRdmaPort
		bottomSender = wb.cache.bottomToRdmaSender
		trans.toRemote = true
	} else {
		bottomPort = wb.cache.BottomToDramPort
		bottomSender = wb.cache.bottomToDramSender
		trans.toRemote = false
	}
	trans.mshrEntry.FetchedRemote = trans.toRemote

	if wb.inflightFetchBlocked(trans.toRemote, false) {
		return false
	}

	if !bottomSender.CanSend(1) {
		// return true
		// bottomPort.(*akita.LimitNumMsgPort).HeadToTail()
		return false
	}

	var origInfo interface{}
	if trans.read != nil {
		origInfo = trans.read.Info
	}
	read := protocol.ReadReqBuilder{}.
		WithSrc(bottomPort).
		WithDst(lowModulePort). // this could be DRAM Top or RDMA ToL2
		WithPID(trans.fetchPID).
		WithAddress(trans.fetchAddress).
		WithByteSize(1 << wb.cache.log2BlockSize).
		WithInfo(origInfo).
		Build()
	read.HopTraceID = trans.hopTraceID // continue origin hop-trace task (corner)
	read.HopBase = trans.hop           // cumulative requestor->corner hops (corner)

	if bottomPort == wb.cache.BottomToRdmaPort {
		if trans.read != nil &&
			strings.Contains(trans.read.Meta().Src.Name(), "MMU") {
			// tracing.StartTask(
			// read.Meta().ID,
			// "", 0,
			// wb.cache,
			// "pte-caching-remote-ptw",
			// "", nil)
			read.IsPtw = true
		}

		bottomSender.Send(read)
	} else {
		if trans.read != nil &&
			strings.Contains(trans.read.Meta().Src.Name(), "MMU") {
			// tracing.StartTask(
			// "", "", 0,
			// wb.cache,
			// "pte-caching-local-ptw",
			// "", nil)
			read.IsPtw = true
		}

		toDramMsg := &idealmemcontroller.ToDramMsg{
			Msg:       read,
			FromLocal: true,
		}
		bottomSender.Send(toDramMsg)
	}

	trans.fetchReadReq = read
	trans.fetchSentTime = now
	wb.inflightFetch.Push(trans)

	if trans.toRemote {
		tracing.AddRemoteLLCMiss()
	} else {
		tracing.AddLocalLLCMiss()
	}

	tracing.TraceReqInitiate(read, now, wb.cache,
		tracing.MsgIDAtReceiver(trans.req(), wb.cache))

	wb.cache.writeBufferBuffer.RemoveByValue(trans)
	return true
}

func (wb *writeBufferStage) processWriteBufferEvictAndWrite(
	now akita.VTimeInSec,
	trans *transaction,
) bool {
	if wb.writeBufferFull() {
		return false
	}

	bankNum := cache.GetBankID(
		trans.block,
		wb.cache.directory.WayAssociativity(),
		len(wb.cache.dirToBankBuffers),
	)
	bankBuf := wb.cache.writeBufferToBankBuffers[bankNum]

	if !bankBuf.CanPush() {
		return false
	}

	trans.action = bankWriteHit
	bankBuf.Push(trans)

	wb.pendingEvictions.Push(trans)
	wb.cache.writeBufferBuffer.RemoveByValue(trans)

	if global.GetInstance().GetVerbose() {
		fmt.Println("writebuffer evict and write", wb.cache.Name(), now)
	}

	return true
}

func (wb *writeBufferStage) processWriteBufferFetchAndEvict(
	now akita.VTimeInSec,
	trans *transaction,
) bool {
	ok := wb.processWriteBufferFlush(now, trans, false)
	if ok {
		trans.action = writeBufferFetch
		return true
	}

	return false
}

func (wb *writeBufferStage) processWriteBufferFlush(
	now akita.VTimeInSec,
	trans *transaction,
	popAfterDone bool,
) bool {
	if wb.writeBufferFull() {
		return false
	}

	wb.pendingEvictions.Push(trans)

	if popAfterDone {
		wb.cache.writeBufferBuffer.RemoveByValue(trans)
	}

	return true
}

func (wb *writeBufferStage) processReturnRspFromDram(now akita.VTimeInSec) bool {
	msg := wb.cache.BottomToDramPort.Peek()
	if msg == nil {
		return false
	}

	switch msg := msg.(type) {
	case *protocol.DataReadyRsp:
		return wb.processDataReadyRspFromDram(now, msg)
	case *protocol.WriteDoneRsp:
		return wb.processWriteDoneRspFromDram(now, msg)
	default:
		panic("unknown msg type")
	}
}

func (wb *writeBufferStage) processDataReadyRspFromDram(
	now akita.VTimeInSec,
	dataReady *protocol.DataReadyRsp,
) bool {
	trans := wb.findInflightFetchByFetchReadReqID(dataReady.RespondTo)

	if !wb.cache.mshrStageBuffer.CanPush() {
		// Transient full: mshrStage drains mshrStageBuffer earlier in this same
		// cache tick, so a slot frees next cycle. Report progress (true) and
		// leave the response on the port (no Retrieve) so the cache keeps
		// ticking and re-processes it, instead of quiescing on a momentary full
		// and tripping the deadlock detector on a false positive.
		return true
	}

	trans.fetchedData = dataReady.Data
	trans.action = bankWriteFetched
	trans.mshrEntry.Data = dataReady.Data
	wb.combineData(trans.mshrEntry)

	wb.cache.mshr.Remove(trans.mshrEntry.PID, trans.mshrEntry.Address)

	// Safety: acquire keys on isReqToRemote(addr); if a remote-charged fetch
	// ever completes via this local-DRAM path, release here too so
	// numToRemote/numCornerServe can't leak. No-op for genuine local fetches
	// (isReqToRemote=false → never charged). Use trans.fetchAddress (set for every
	// fetch, read OR write) — read/write are mutually exclusive, so reading either
	// directly would nil-deref the other class.
	if wb.cache.isReqToRemote(trans.fetchAddress) {
		wb.cache.releaseRemoteBudget(trans)
	}

	mshrEntry := trans.mshrEntry
	block := mshrEntry.Block
	block.IsLocked = false
	block.IsValid = true

	tracing.StartTask(0, 0, now, wb.cache,
		"mshr-merge", "dram", len(mshrEntry.Requests))

	wb.cache.mshrStageBuffer.Push(mshrEntry) // wake up the mshr entry
	wb.cache.storage.Write(block.CacheAddress, mshrEntry.Data)

	if trans.fetchSentTime != 0 {
		wb.cache.LocalWaitSum += now - trans.fetchSentTime
		wb.cache.LocalWaitCount++
	}

	wb.removeInflightFetch(trans)
	wb.cache.BottomToDramPort.Retrieve(now)

	tracing.TraceReqFinalize(trans.fetchReadReq, now, wb.cache)

	return true
}

func (wb *writeBufferStage) processWriteDoneRspFromDram(
	now akita.VTimeInSec,
	writeDone *protocol.WriteDoneRsp,
) bool {
	for i, e := range wb.inflightEviction.elements {
		trans := e.(*transaction)
		if trans.evictionWriteReq.ID == writeDone.RespondTo {
			wb.inflightEviction.elements = append(
				wb.inflightEviction.elements[:i],
				wb.inflightEviction.elements[i+1:]...,
			)

			wb.cache.BottomToDramPort.Retrieve(now)
			tracing.TraceReqFinalize(trans.evictionWriteReq, now, wb.cache)

			return true
		}
	}

	panic("write request not found")
}

func (wb *writeBufferStage) processReturnRspFromRdma(now akita.VTimeInSec) bool {
	msg := wb.cache.BottomToRdmaPort.Peek()
	if msg == nil {
		return false
	}

	switch msg := msg.(type) {
	case *protocol.DataReadyRsp:
		return wb.processDataReadyRspFromRdma(now, msg)
	case *protocol.WriteDoneRsp:
		return wb.processWriteDoneRspFromRdma(now, msg)
	default:
		panic("unknown msg type")
	}
}

func (wb *writeBufferStage) processDataReadyRspFromRdma(
	now akita.VTimeInSec,
	dataReady *protocol.DataReadyRsp,
) bool {
	trans := wb.findInflightFetchByFetchReadReqID(dataReady.RespondTo)

	if !wb.cache.mshrStageBuffer.CanPush() {
		// Transient full: mshrStage drains mshrStageBuffer earlier in this same
		// cache tick, so a slot frees next cycle. Report progress (true) and
		// leave the response on the port (no Retrieve) so the cache keeps
		// ticking and re-processes it, instead of quiescing on a momentary full
		// and tripping the deadlock detector on a false positive.
		return true
	}

	trans.fetchedData = dataReady.Data

	if trans.action == writeBufferMshrCoalesce ||
		trans.action == writeBufferEvictAndMshrCoalesce {
		// Coalesce delegation completed: the corner returned the data. The
		// requestor's OWN real MSHR (allocated at delegation) receives it — fill,
		// install the block, release the real MSHR slot. Same as a normal fetch
		// completion, except the finalized request is the delegation and no remote
		// budget was acquired (the corner did the real fetch), so none is released.
		trans.mshrEntry.Data = dataReady.Data
		trans.action = bankMshrCoalesceFetched
		wb.combineData(trans.mshrEntry)

		wb.cache.mshr.Remove(trans.mshrEntry.PID, trans.mshrEntry.Address)

		mshrEntry := trans.mshrEntry
		block := mshrEntry.Block
		block.IsLocked = false
		block.IsValid = true
		wb.cache.mshrStageBuffer.Push(mshrEntry) // wake up the mshr entry
		wb.cache.storage.Write(block.CacheAddress, mshrEntry.Data)

		tracing.TraceReqFinalize(trans.mshrCoalesceReq, now, wb.cache)
	} else {
		// normal requests
		trans.mshrEntry.Data = dataReady.Data
		trans.action = bankWriteFetched
		wb.combineData(trans.mshrEntry)

		if trans.read != nil {
			if wb.cache.isReqToRemote(trans.read.Address) {
				wb.cache.releaseRemoteBudget(trans)
			}
		} else {
			if wb.cache.isReqToRemote(trans.write.Address) {
				wb.cache.releaseRemoteBudget(trans)
			}
		}

		wb.cache.mshr.Remove(trans.mshrEntry.PID, trans.mshrEntry.Address)

		mshrEntry := trans.mshrEntry
		block := mshrEntry.Block
		block.IsLocked = false
		block.IsValid = true

		tracing.StartTask(0, 0, now, wb.cache,
			"mshr-merge", "rdma", len(mshrEntry.Requests))

		wb.cache.mshrStageBuffer.Push(mshrEntry) // wake up the mshr entry
		wb.cache.storage.Write(block.CacheAddress, mshrEntry.Data)

		tracing.TraceReqFinalize(trans.fetchReadReq, now, wb.cache)
	}

	if trans.fetchSentTime != 0 {
		wb.cache.RemoteWaitSum += now - trans.fetchSentTime
		wb.cache.RemoteWaitCount++
	}

	wb.removeInflightFetch(trans)
	wb.cache.BottomToRdmaPort.Retrieve(now)

	return true
}

func (wb *writeBufferStage) processWriteDoneRspFromRdma(
	now akita.VTimeInSec,
	writeDone *protocol.WriteDoneRsp,
) bool {
	for i, e := range wb.inflightEviction.elements {
		trans := e.(*transaction)
		if trans.evictionWriteReq.ID == writeDone.RespondTo {
			wb.inflightEviction.elements = append(
				wb.inflightEviction.elements[:i],
				wb.inflightEviction.elements[i+1:]...,
			)
			wb.cache.BottomToRdmaPort.Retrieve(now)
			tracing.TraceReqFinalize(trans.evictionWriteReq, now, wb.cache)

			return true
		}
	}

	panic("write request not found")
}

func (wb *writeBufferStage) combineData(mshrEntry *cache.MSHREntry) {
	mshrEntry.Block.DirtyMask = make([]bool, 1<<wb.cache.log2BlockSize)
	for _, t := range mshrEntry.Requests {
		trans := t.(*transaction)
		if trans.read != nil {
			continue
		}

		mshrEntry.Block.IsDirty = true
		write := trans.write
		_, offset := cache.GetCacheLineID(write.Address, wb.cache.log2BlockSize)
		for i := 0; i < len(write.Data); i++ {
			if write.DirtyMask == nil || write.DirtyMask[i] {
				index := offset + uint64(i)
				mshrEntry.Data[index] = write.Data[i]
				mshrEntry.Block.DirtyMask[index] = true
			}
		}
	}
}

func (wb *writeBufferStage) findInflightFetchByFetchReadReqID(
	id uint64,
) *transaction {
	for _, t := range wb.inflightFetch.elements {
		trans := t.(*transaction)
		if trans.fetchReadReq != nil && trans.fetchReadReq.ID == id {
			return trans
		}
		if trans.mshrCoalesceReq != nil && trans.mshrCoalesceReq.ID == id {
			return trans
		}
	}

	panic("inflight read not found")
}

func (wb *writeBufferStage) removeInflightFetch(f *transaction) {
	for i, e := range wb.inflightFetch.elements {
		trans := e.(*transaction)
		if trans == f {
			wb.inflightFetch.elements = append(
				wb.inflightFetch.elements[:i],
				wb.inflightFetch.elements[i+1:]...,
			)
			return
		}
	}

	panic("not found")
}

func (wb *writeBufferStage) writeBufferFull() bool {
	numEntry := wb.pendingEvictions.Size() + wb.inflightEviction.Size()
	return numEntry >= wb.writeBufferCapacity
}

// inflightFetch admission is split into three reserve tiers by fetch class.
// The single shared maxInflightFetch budget let a remote-fetch flood (whose
// responses depend on OTHER chiplets serving) starve the local-DRAM fetch that
// a serve-read needs to produce its own DataReady — the exact cross-chiplet
// fetch-response circular wait of the NearFetch deadlock. Splitting the budget
// guarantees the serve sink always has room:
//
//	local  non-coalesce : full budget  (DRAM always responds; never throttle)
//	remote non-coalesce : budget - localReserve            (keep local free)
//	coalesce (remote)   : budget - localReserve - remoteReserve (keep both free)
//
// "remote non-coalesce" is EVERY real remote fetch — a node only ever fetches
// from home when it is straight-line to home (hasCornerNode=false), i.e. it is
// the corner for that fetch. Whether the fetch serves an own CU read or an
// incoming corner-lookup is irrelevant to admission: both go to home, both get a
// home-local DataReady, both always complete. So they share one band; the only
// class that must be bounded is the coalesce delegation (the L-shaped path that
// waits on a corner), keyed on the TRUE predicate isCoalesce = hasCornerNode.
const (
	// inflightFetchLocalReserve keeps slots free for local-DRAM fetches (the
	// serve-read sink). This is the cross-chiplet cycle-breaker.
	inflightFetchLocalReserve = 16
	// inflightFetchRemoteReserve keeps slots free for direct remote
	// (non-coalesce) fetches so a coalesce flood cannot starve them.
	inflightFetchRemoteReserve = 8
	// maxCoalesceInflight hard-caps the number of outstanding coalesce
	// delegations (plain reads delegated to a corner) PER CHIPLET. This is the
	// real flow-control bound: a coalesce uses a virtual MSHR, so without this it
	// is throttled only by the large inflightFetch band (~104) and the requestor
	// can flood the request network faster than corners/homes can drain it —
	// the cross-chiplet deadlock. Bounding outstanding delegations to the real
	// MSHR count keeps total in-flight (32 x numChiplets) below network capacity,
	// so the lookup/serve chain always advances. Hitting the cap stalls the
	// requestor: normal MSHR-style back-pressure.
	maxCoalesceInflight = 32
)

// coalesceInflightCount returns the number of coalesce (mshrCoalesceReq != nil)
// fetches currently in flight. Used to bound coalesce BY COUNT, not by total
// inflightFetch size — otherwise "104 coalesce" and "104 of any class" gate
// identically and the coalesce reserve doesn't actually bound coalesce. Mirrors
// the type-counted SmartBuffer reserve (coalesceCount).
func (wb *writeBufferStage) coalesceInflightCount() int {
	n := 0
	for _, e := range wb.inflightFetch.elements {
		if e.(*transaction).mshrCoalesceReq != nil {
			n++
		}
	}
	return n
}

// inflightFetchBlocked is the reserve-aware admission test for inflightFetch.
// Local and remote-non-coalesce are gated on total size (so the lower-priority
// classes leave headroom for the higher ones); coalesce is gated on its own
// COUNT so a coalesce flood is bounded regardless of how full the buffer is
// with real fetches.
func (wb *writeBufferStage) inflightFetchBlocked(toRemote, isCoalesce bool) bool {
	sz := wb.inflightFetch.Size()
	switch {
	case !toRemote: // local-DRAM fetch: full budget
		return sz >= wb.maxInflightFetch
	case !isCoalesce: // remote non-coalesce: keep local reserve free
		return sz >= wb.maxInflightFetch-inflightFetchLocalReserve
	default: // coalesce delegation: hard-capped per chiplet (flow control), plus
		// the inflightFetch overflow guard.
		return wb.coalesceInflightCount() >= maxCoalesceInflight ||
			sz >= wb.maxInflightFetch
	}
}

func (wb *writeBufferStage) tooManyInflightEvictions() bool {
	return wb.inflightEviction.Size() >= wb.maxInflightEviction
}

func (wb *writeBufferStage) Reset(now akita.VTimeInSec) {
	wb.cache.writeBufferBuffer.Clear()
}
