package writeback_dynamic_dist

import (
	// "fmt"

	"fmt"
	"strings"

	"gitlab.com/akita/akita"
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

	if wb.tooManyInflightEvictions() {
		return false
	}

	trans := wb.pendingEvictions.Peek().(*transaction)

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
		fmt.Println("write to evict",
			bottomPort.Name(),
			lowModulePort.Name(),
			write.ID)
	}

	trans.evictionWriteReq = write
	wb.pendingEvictions.Pop()
	wb.inflightEviction.Push(trans)

	tracing.TraceReqInitiate(write, now, wb.cache,
		tracing.MsgIDAtReceiver(trans.req(), wb.cache))

	return true
}

func (wb *writeBufferStage) processNewTransaction(now akita.VTimeInSec) bool {
	item := wb.cache.writeBufferBuffer.Peek()
	if item == nil {
		return false
	}

	trans := item.(*transaction)
	switch trans.action {
	case writeBufferFetch:
		return wb.processWriteBufferFetch(now, trans)
	case writeBufferEvictAndWrite:
		return wb.processWriteBufferEvictAndWrite(now, trans)
	case writeBufferEvictAndFetch:
		return wb.processWriteBufferFetchAndEvict(now, trans)
	case writeBufferFlush:
		return wb.processWriteBufferFlush(now, trans, true)
	default:
		panic("unknown transaction action")
	}
}

func (wb *writeBufferStage) processWriteBufferFetch(
	now akita.VTimeInSec,
	trans *transaction,
) bool {
	// The starge first tries to find the needed data locally from evictions of the same line.
	if wb.findDataLocally(trans) {
		// If data is found locally, the stage prepares a bankWriteFetched for the owning bank,
		// merges pending writes via the MSHR entry, and removes the MSHR entry.
		return wb.sendFetchedDataToBank(trans)
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
			wb.cache.numToRemote--
		}
	} else {
		if wb.cache.isReqToRemote(trans.write.Address) {
			wb.cache.numToRemote--
		}
	}

	mshrEntry := trans.mshrEntry
	block := mshrEntry.Block
	block.IsLocked = false
	block.IsValid = true

	wb.cache.mshr.Remove(trans.mshrEntry.PID, trans.mshrEntry.Address)

	// sendFetchedDataToBank does not receive `now`; pass 0 — MSHRFailTracer
	// only reads task.What/Where (matches the mshr-fail tracing pattern).
	tracing.StartTask(0, 0, 0, wb.cache,
		"mshr-merge", "buffer", len(mshrEntry.Requests))

	wb.cache.mshrStageBuffer.Push(mshrEntry) // wake up the mshr entry
	wb.cache.storage.Write(block.CacheAddress, mshrEntry.Data)

	wb.cache.writeBufferBuffer.Pop()
	return true
}

func (wb *writeBufferStage) fetchFromBottom(
	now akita.VTimeInSec,
	trans *transaction,
) bool {
	// Resolve local vs remote BEFORE the inflight admission test so a local-DRAM
	// fetch (the serve sink — DRAM always responds) keeps reserved inflight slots
	// that a remote-fetch flood cannot take.
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

	if wb.inflightFetchBlocked(trans.toRemote) {
		return false
	}

	if !bottomSender.CanSend(1) {
		return false
	}

	readReqBuilder := protocol.ReadReqBuilder{}.
		WithSrc(bottomPort).
		WithDst(lowModulePort). // this could be DRAM Top or RDMA ToL2
		WithPID(trans.fetchPID).
		WithAddress(trans.fetchAddress).
		WithByteSize(1 << wb.cache.log2BlockSize)

	if trans.read != nil {
		readReqBuilder = readReqBuilder.WithInfo(trans.read.Info) // pc
	}

	read := readReqBuilder.Build()
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

	wb.cache.writeBufferBuffer.Pop()
	return true
}

func (wb *writeBufferStage) processWriteBufferEvictAndWrite(
	now akita.VTimeInSec,
	trans *transaction,
) bool {
	if wb.writeBufferFull() {
		return false
	}

	bankNum := GetBankID(
		trans.block,
		wb.cache.directory.WayAssociativity(),
		len(wb.cache.dirToBankBuffers),
	)
	bankBuf := wb.cache.writeBufferToBankBuffers[bankNum]

	if !bankBuf.CanPushTransReserve(trans.toRemote, smartBufferLocalReserve) {
		return false
	}

	trans.action = bankWriteHit
	bankBuf.Push(trans)

	wb.pendingEvictions.Push(trans)
	wb.cache.writeBufferBuffer.Pop()

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
		// return true
		return false
	}

	wb.pendingEvictions.Push(trans)
	// wb.pendingEvictions = append(wb.pendingEvictions, trans)

	if popAfterDone {
		wb.cache.writeBufferBuffer.Pop()
	}

	return true
}

// canBypassRsp reports whether a bottom-port return msg can be processed out
// of order ahead of a deferred head. Eviction acks (WriteDoneRsp) always
// qualify; fills qualify when the MSHR stage has room. This lets acks / ready
// fills bypass a LocalHomeRsp stalled on write-buffer room or a locked victim
// — the head-of-line deadlock on the shared bottom ports.
func (wb *writeBufferStage) canBypassRsp(m akita.Msg) bool {
	switch m.(type) {
	case *protocol.WriteDoneRsp:
		return true
	case *protocol.DataReadyRsp, *protocol.CommonDistRsp:
		return wb.cache.mshrStageBuffer.CanPush()
	default:
		return false
	}
}

func (wb *writeBufferStage) processReturnRspFromDram(now akita.VTimeInSec) bool {
	port := wb.cache.BottomToDramPort.(*akita.LimitNumMsgPort)

	if m, idx, ok := port.PeekReady(wb.canBypassRsp); ok {
		return wb.dispatchDramRsp(now, m, idx)
	}

	msg := port.Peek()
	if msg == nil {
		return false
	}
	return wb.dispatchDramRsp(now, msg, 0)
}

func (wb *writeBufferStage) dispatchDramRsp(
	now akita.VTimeInSec, msg akita.Msg, idx int,
) bool {
	switch msg := msg.(type) {
	case *protocol.DataReadyRsp:
		return wb.processDataReadyRspFromDram(now, msg, idx)
	case *protocol.WriteDoneRsp:
		return wb.processWriteDoneRspFromDram(now, msg, idx)
	default:
		panic("unknown msg type")
	}
}

// enableBypassByHotness gates all three bypass-by-hotness branches in this
// file. Flip to false to disable bypass everywhere (DRAM rsp, RDMA rsp, and
// LocalHome distribution rsp) and let the normal install path run instead.
const enableBypassByHotness = false
const enableBypassByHotnessForDram = true

func (wb *writeBufferStage) processDataReadyRspFromDram(
	now akita.VTimeInSec,
	dataReady *protocol.DataReadyRsp,
	idx int,
) bool {
	trans := wb.findInflightFetchByFetchReadReqID(dataReady.RespondTo)

	if !wb.cache.mshrStageBuffer.CanPush() {
		// DIAGNOSTIC: was `return true` (kept cache ticking while stuck →
		// masked deadlock as livelock). false lets the engine quiesce so the
		// quiesce-deadlock detector can fire. Revert after diagnosis.
		return false
	}

	if trans.fetchSentTime != 0 {
		wb.cache.LocalWaitSum += now - trans.fetchSentTime
		wb.cache.LocalWaitCount++
	}
	wb.absorbDramData(trans, dataReady.Data)

	mshrEntry := trans.mshrEntry
	block := mshrEntry.Block

	// DRAM responses carry no DistInfo, so the fetched line's hotness
	// is Cold. Bypass when the resident victim ranks strictly higher
	// (Warm+) or when ranks tie and the victim is a PTE we don't want
	// to evict with non-PTE data. The fetched line's PTE-ness comes
	// from the originating L2 request (IsPtw flag).
	incomingIsPte := trans.fetchReadReq != nil && trans.fetchReadReq.IsPtw
	if shouldBypassByHotness(trans.victim, protocol.Cold, incomingIsPte) {
		wb.bypassFetchedLine(now, trans, block, mshrEntry, wb.cache.BottomToDramPort, idx)
		return true
	}
	wb.installFetchedLine(now, trans, block, mshrEntry,
		wb.cache.BottomToDramPort, protocol.Cold, incomingIsPte, idx)
	return true
}

// absorbDramData is the DRAM counterpart of absorbRdmaData. It copies the
// fetched payload into the MSHR entry, runs the inter-entry combine pass,
// and retires the MSHR slot. Unlike the RDMA path, it does NOT decrement
// numToRemote (that counter only tracks remote fetches).
func (wb *writeBufferStage) absorbDramData(
	trans *transaction,
	data []byte,
) {
	trans.fetchedData = data
	trans.action = bankWriteFetched
	trans.mshrEntry.Data = data
	wb.combineData(trans.mshrEntry)

	wb.cache.mshr.Remove(trans.mshrEntry.PID, trans.mshrEntry.Address)
}

func (wb *writeBufferStage) processWriteDoneRspFromDram(
	now akita.VTimeInSec,
	writeDone *protocol.WriteDoneRsp,
	idx int,
) bool {
	for i, e := range wb.inflightEviction.elements {
		trans := e.(*transaction)
		if trans.evictionWriteReq.ID == writeDone.RespondTo {

			wb.inflightEviction.elements = append(
				wb.inflightEviction.elements[:i],
				wb.inflightEviction.elements[i+1:]...,
			)
			wb.cache.BottomToDramPort.(*akita.LimitNumMsgPort).RetrieveAt(now, idx)
			tracing.TraceReqFinalize(trans.evictionWriteReq, now, wb.cache)

			return true
		}
	}

	panic("write request not found")
}

func (wb *writeBufferStage) processReturnRspFromRdma(now akita.VTimeInSec) bool {
	port := wb.cache.BottomToRdmaPort.(*akita.LimitNumMsgPort)

	if m, idx, ok := port.PeekReady(wb.canBypassRsp); ok {
		return wb.dispatchRdmaRsp(now, m, idx)
	}

	msg := port.Peek()
	if msg == nil {
		return false
	}
	return wb.dispatchRdmaRsp(now, msg, 0)
}

func (wb *writeBufferStage) dispatchRdmaRsp(
	now akita.VTimeInSec, msg akita.Msg, idx int,
) bool {
	switch msg := msg.(type) {
	case *protocol.LocalHomeRsp:
		return wb.processLocalHomeRspFromRdma(now, msg, idx)
	case *protocol.DataReadyRsp:
		return wb.processDataReadyRspFromRdma(now, msg, idx)
	case *protocol.CommonDistRsp:
		return wb.processCommonDistRspFromRdma(now, msg, idx)
	case *protocol.WriteDoneRsp:
		return wb.processWriteDoneRspFromRdma(now, msg, idx)
	default:
		panic("unknown msg type")
	}
}

func (wb *writeBufferStage) processCommonDistRspFromRdma(
	now akita.VTimeInSec,
	rsp *protocol.CommonDistRsp,
	idx int,
) bool {
	trans := wb.findInflightFetchByFetchReadReqID(rsp.RespondTo)

	if !wb.cache.mshrStageBuffer.CanPush() {
		// DIAGNOSTIC: was `return true` (kept cache ticking while stuck →
		// masked deadlock as livelock). false lets the engine quiesce so the
		// quiesce-deadlock detector can fire. Revert after diagnosis.
		return false
	}

	distInfo, ok := rsp.Info.(*protocol.DistInfo)
	if !ok {
		panic("expect DistInfo in CommonDistRsp")
	}

	wb.absorbRdmaData(trans, rsp.Data)

	mshrEntry := trans.mshrEntry
	block := mshrEntry.Block

	// CommonDistRsp arrives when this cache is BOTH the requester and
	// the line's local home. Apply the same bypass / install / non-
	// cacheable handling as the regular RDMA DataReadyRsp path.
	incomingIsPte := distInfo.IsPtw
	if shouldBypassByHotness(trans.victim, distInfo.Hotness, incomingIsPte) {
		wb.bypassFetchedLine(now, trans, block, mshrEntry, wb.cache.BottomToRdmaPort, idx)
		return true
	}
	wb.installFetchedLine(now, trans, block, mshrEntry,
		wb.cache.BottomToRdmaPort, storableRdmaHotness(distInfo.Hotness), incomingIsPte, idx)

	block.virtualBitForLocalHome = true

	if isNonCacheableRsp(distInfo.Hotness) {
		block.IsValid = false
		traceName := "bypassed-by-streaming"
		if distInfo.Hotness == protocol.Bypass {
			traceName = "bypassed-by-lookup"
		}
		tracing.StartTask(0, 0, now, wb.cache, traceName, "", nil)
	}
	return true
}

// processLocalHomeRspFromRdma handles a distribution arriving at this cache
// as the cluster-local home (no MSHR involvement). Behavior is identical to
// the commented-out original; only structure is split.
//
// Branches (in priority order):
//
//	(A) The line is already cached  → set owner bit, drop the distribution.
//	(B) Victim is busy              → defer (return false; head retries
//	                                  next tick — strict FIFO, no rotation).
//	(C) Hotness-rank bypass         → drop the distribution, unlock victim.
//	(D) Victim needs eviction       → snapshot + enqueue write-back.
//	(E) Install                     → storage.Write, finalize metadata,
//	                                  always mark owner bit.
func (wb *writeBufferStage) processLocalHomeRspFromRdma(
	now akita.VTimeInSec,
	rsp *protocol.LocalHomeRsp,
	idx int,
) bool {
	distInfo, ok := rsp.Info.(*protocol.DistInfo)
	if !ok {
		panic("expect physical address in DistInfo")
	}

	address := distInfo.PAddr
	cacheLineID, _ := GetCacheLineID(address, wb.cache.log2BlockSize)

	// if the victim has tag as same as distribute cacheline,
	// the remote data is already in the cache.
	block := wb.cache.directory.Lookup(
		1, cacheLineID)

	if block != nil {
		// Normalize before stamping onto a resident line: the raw
		// DistInfo.Hotness may carry _NonShared / Streaming / Bypass
		// sentinels that the rest of the cache (PromoteOnHit, FindVictim
		// arithmetic) assumes never appear on a resident block. Route it
		// through the same mapping every other install path uses so the
		// stored value stays within {Cold, Warm, Hot, Global}.
		newHotness := storableRdmaHotness(distInfo.Hotness)
		block.Hotness = newHotness
		block.virtualBitForLocalHome = true
		tracing.StartTask(
			0,
			0,
			now,
			wb.cache,
			"already-exists",
			"",
			nil)
		tracing.StartTask(0, 0, now, wb.cache,
			"distribution-trace", "",
			&tracing.DistInfo{
				Addr:     block.Tag,
				PC:       block.InstallPC,
				Home:     wb.cache.HomeNodeFinder.GetChipletId(block.Tag),
				Hotness:  newHotness,
				DistType: tracing.TYPE_HOTNESS_REASSIGN_DIST,
			})
		wb.cache.BottomToRdmaPort.(*akita.LimitNumMsgPort).RetrieveAt(now, idx)
		return true
	}

	victim := wb.cache.directory.FindVictimWithoutDemotion(cacheLineID)
	if victim.IsLocked || victim.ReadCount > 0 {
		return false
	}

	// Bypass-by-hotness: if the incoming distributed line ranks lower
	// than the resident victim — or ranks tie and the victim is a PTE
	// we don't want to evict with non-PTE data — drop the distribution
	// and leave the resident in place. No restore is needed here —
	// victim has not been mutated yet on this path. The deferred LRU/
	// hotness side-effects from FindVictim are NOT committed.
	if shouldBypassByHotness(victim, distInfo.Hotness, distInfo.IsPtw) {
		tracing.StartTask(0, 0, now, wb.cache,
			"bypassed-by-hotness-local-home", "", nil)
		wb.cache.BottomToRdmaPort.(*akita.LimitNumMsgPort).RetrieveAt(now, idx)
		return true
	}

	trans := &transaction{}
	trans.block = victim
	trans.fetchedData = rsp.Data

	// evict the block before distribute
	if wb.cache.dirStage.needEviction(victim) {
		if wb.writeBufferFull() {
			// return true
			return false
		}

		trans.evictingPID = victim.PID
		trans.evictingAddr = victim.Tag
		trans.evictingData, _ = wb.cache.storage.Read(
			victim.CacheAddress, 1<<wb.cache.log2BlockSize)
		trans.evictingDirtyMask = victim.DirtyMask
		victimInstallPC := victim.InstallPC

		wb.pendingEvictions.Push(trans)

		victimHome := wb.cache.chipletIDStrings[wb.cache.HomeNodeFinder.GetChipletId(trans.evictingAddr)]

		tracing.StartTask(
			0,
			0,
			now,
			wb.cache,
			"evict-by-distribution",
			"",
			nil)

		// Distribution-driven evict has no demand-request evictor PC, so
		// only the victim row (type 8) is emitted; the evictor row (type
		// 6) would have no usable PC. Joins by (time, chiplet) on a
		// memory-trace pulled from a dist-platform run should expect
		// orphan type 8 rows on this path.
		tracing.StartTask(
			8,
			1,
			now,
			wb.cache,
			"memory-trace",
			victimHome,
			&tracing.VictimInfo{
				VictimAddr: trans.evictingAddr,
				VictimPC:   victimInstallPC,
			},
		)

		if victim.virtualBitForLocalHome {
			tracing.StartTask(
				0,
				0,
				now,
				wb.cache,
				"evict-local-home-cacheline",
				"",
				&tracing.DistInfo{
					Addr:       trans.evictingAddr,
					PC:         victimInstallPC,
					Home:       wb.cache.HomeNodeFinder.GetChipletId(trans.evictingAddr),
					ToResponse: wb.cache.ChipletId,
					Hotness:    victim.Hotness,
					DistType:   tracing.TYPE_EVICT_LOCAL_HOME_CACHELINE,
				})
		}
	} else {
		victimAddr := victim.Tag
		victimInstallPC := victim.InstallPC
		victimHome := wb.cache.chipletIDStrings[wb.cache.HomeNodeFinder.GetChipletId(victimAddr)]

		tracing.StartTask(
			0,
			0,
			now,
			wb.cache,
			"overwrite-by-distribution",
			"",
			nil)

		// Same caveat as the evict branch above: no evictor PC available
		// on the distribution path, so only type 9 (victim) is emitted.
		tracing.StartTask(
			9,
			1,
			now,
			wb.cache,
			"memory-trace",
			victimHome,
			&tracing.VictimInfo{
				VictimAddr: victimAddr,
				VictimPC:   victimInstallPC,
			},
		)

		if victim.virtualBitForLocalHome {
			tracing.StartTask(
				0,
				0,
				now,
				wb.cache,
				"overwrite-local-home-cacheline",
				"",
				&tracing.DistInfo{
					Addr:       victimAddr,
					PC:         victimInstallPC,
					Home:       wb.cache.HomeNodeFinder.GetChipletId(victimAddr),
					ToResponse: wb.cache.ChipletId,
					Hotness:    victim.Hotness,
					DistType:   tracing.TYPE_OVERWRITE_LOCAL_HOME_CACHELINE,
				})
		}
	}

	// directly write the distributed data.
	// do not go to bankstage to simplify the logic
	err := wb.cache.storage.Write(trans.block.CacheAddress, trans.fetchedData)
	if err != nil {
		panic(err)
	}

	// LRU/hotness side-effects of the victim selection already happened
	// during FindVictim's scan (hot blocks were demoted + rotated).
	// Here we only need to mark the slot MRU for the new tag.
	wb.cache.directory.Visit(trans.block)
	trans.block.Tag = cacheLineID
	trans.block.IsLocked = false
	trans.block.IsValid = true
	trans.block.IsDirty = false
	trans.block.ReadCount = 0
	// Distribution-pushed line: no demand-request PC associated with the
	// install (the line is brought in by RDMA distribution, not by a load/
	// store). Leave InstallPC = 0 (= "no installer PC") so a later evict
	// reports VictimPC = 0 for lines that arrived this way.
	trans.block.InstallPC = 0

	trans.block.Hotness = distInfo.Hotness
	trans.block.IsPte = distInfo.IsPtw

	wb.cache.BottomToRdmaPort.(*akita.LimitNumMsgPort).RetrieveAt(now, idx)

	switch distInfo.Hotness {
	case protocol.Warm_NonShared:
		trans.block.Hotness = protocol.Warm
	case protocol.Hot_NonShared:
		trans.block.Hotness = protocol.Hot
	default:
		trans.block.virtualBitForLocalHome = true
		trans.block.Hotness = distInfo.Hotness
	}

	return true
}

// installLocalHomeDistribution writes the distributed data into storage and
// finalizes the live block's metadata. The slot is always marked as the
// cluster's owner copy. `*_NonShared` hotness values are mapped to their
// plain counterparts so the stored block hotness is one of
// {Cold, Warm, Hot, Global}.
func (wb *writeBufferStage) installLocalHomeDistribution(
	now akita.VTimeInSec,
	trans *transaction,
	victim *Block,
	cacheLineID uint64,
	distInfo *protocol.DistInfo,
) {
	err := wb.cache.storage.Write(trans.block.CacheAddress, trans.fetchedData)
	if err != nil {
		panic(err)
	}

	// set the owner bit
	wb.cache.directory.Visit(trans.block)
	trans.block.Tag = cacheLineID
	trans.block.IsLocked = false
	trans.block.IsValid = true
	trans.block.IsDirty = false
	trans.block.ReadCount = 0
	trans.block.PID = victim.PID
	trans.block.IsPte = distInfo.IsPtw
	trans.block.virtualBitForLocalHome = true
	trans.block.InstallPC = 0 // distributed install, no demand-request PC

	switch distInfo.Hotness {
	case protocol.Warm_NonShared:
		trans.block.Hotness = protocol.Warm
	case protocol.Hot_NonShared:
		trans.block.Hotness = protocol.Hot
	default:
		trans.block.Hotness = distInfo.Hotness
	}
}

// processDataReadyRspFromRdma handles a DataReadyRsp coming back from the
// remote RDMA path. Behavior is intentionally identical to the commented-out
// original above; only the structure has been split into helpers.
//
// Flow:
//
//	(1) Drain MSHR with the fetched data.
//	(2) Decide install vs. bypass.
//	(3) Pre-install trace on the victim slot.
//	(4) Common cleanup + MSHR wake-up (runs on every path).
//	(5) Install the line as Cold (only if install).
//	(6) Hotness-specific post-install state + tracing.
func (wb *writeBufferStage) processDataReadyRspFromRdma(
	now akita.VTimeInSec,
	rsp *protocol.DataReadyRsp,
	idx int,
) bool {
	trans := wb.findInflightFetchByFetchReadReqID(rsp.RespondTo)

	if !wb.cache.mshrStageBuffer.CanPush() {
		// DIAGNOSTIC: was `return true` (kept cache ticking while stuck →
		// masked deadlock as livelock). false lets the engine quiesce so the
		// quiesce-deadlock detector can fire. Revert after diagnosis.
		return false
	}

	distInfo, ok := rsp.Info.(*protocol.DistInfo)
	if !ok {
		panic("expect DistInfo in DataReadyRsp on RDMA path")
	}

	if trans.fetchSentTime != 0 {
		wb.cache.RemoteWaitSum += now - trans.fetchSentTime
		wb.cache.RemoteWaitCount++
	}
	wb.absorbRdmaData(trans, rsp.Data)

	mshrEntry := trans.mshrEntry
	block := mshrEntry.Block

	// Bypass when the incoming hotness from the remote DistInfo ranks
	// strictly lower than the resident victim — or when ranks tie and
	// the victim is a PTE we don't want to evict with non-PTE data.
	// Otherwise install with the fetched line's hotness (normalized
	// for storage: _NonShared flavors map to their plain counterparts;
	// Streaming/Bypass map to Cold). PTE-ness comes from distInfo (the
	// remote propagated the originating L2 IsPtw); fall back to the
	// local fetchReadReq if DistInfo's flag is absent.
	incomingIsPte := distInfo.IsPtw
	if !incomingIsPte && trans.fetchReadReq != nil {
		incomingIsPte = trans.fetchReadReq.IsPtw
	}
	if shouldBypassByHotness(trans.victim, distInfo.Hotness, incomingIsPte) {
		wb.bypassFetchedLine(now, trans, block, mshrEntry, wb.cache.BottomToRdmaPort, idx)
		return true
	}
	wb.installFetchedLine(now, trans, block, mshrEntry,
		wb.cache.BottomToRdmaPort, storableRdmaHotness(distInfo.Hotness), incomingIsPte, idx)

	// Streaming / Bypass responses signal that the remote chose not
	// to make this line cacheable. The MSHR has been satisfied with
	// the fetched data, but the slot must be invalidated so future
	// lookups miss and the slot is immediately reclaimable.
	if isNonCacheableRsp(distInfo.Hotness) {
		block.IsValid = false
		traceName := "bypassed-by-streaming"
		if distInfo.Hotness == protocol.Bypass {
			traceName = "bypassed-by-lookup"
		}
		tracing.StartTask(0, 0, now, wb.cache, traceName, "", nil)
	}
	return true
}

// absorbRdmaData copies the fetched payload into the MSHR entry, runs the
// inter-entry combine pass, and retires the MSHR slot. Bookkeeping only --
// no block state is touched here.
func (wb *writeBufferStage) absorbRdmaData(
	trans *transaction,
	data []byte,
) {
	trans.fetchedData = data
	trans.action = bankWriteFetched
	trans.mshrEntry.Data = data
	wb.combineData(trans.mshrEntry)

	wb.cache.mshr.Remove(trans.mshrEntry.PID, trans.mshrEntry.Address)
	wb.cache.numToRemote--
}

func (wb *writeBufferStage) processWriteDoneRspFromRdma(
	now akita.VTimeInSec,
	writeDone *protocol.WriteDoneRsp,
	idx int,
) bool {
	for i, e := range wb.inflightEviction.elements {
		trans := e.(*transaction)
		if trans.evictionWriteReq.ID == writeDone.RespondTo {
			wb.inflightEviction.elements = append(
				wb.inflightEviction.elements[:i],
				wb.inflightEviction.elements[i+1:]...,
			)
			wb.cache.BottomToRdmaPort.(*akita.LimitNumMsgPort).RetrieveAt(now, idx)
			tracing.TraceReqFinalize(trans.evictionWriteReq, now, wb.cache)

			// wb.cache.numRemoteEvictions--

			return true
		}
	}

	panic("write request not found")
}

func (wb *writeBufferStage) combineData(mshrEntry *MSHREntry) {
	mshrEntry.Block.DirtyMask = make([]bool, 1<<wb.cache.log2BlockSize)
	for _, t := range mshrEntry.Requests {
		trans := t.(*transaction)
		if trans.read != nil {
			continue
		}

		mshrEntry.Block.IsDirty = true
		write := trans.write
		_, offset := GetCacheLineID(write.Address, wb.cache.log2BlockSize)
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
	for _, e := range wb.inflightFetch.elements {
		trans := e.(*transaction)
		if trans.fetchReadReq.ID == id {
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
	// numEntry := len(wb.pendingEvictions) + len(wb.inflightEviction.elements)
	numEntry := wb.pendingEvictions.Size() + wb.inflightEviction.Size()
	return numEntry >= wb.writeBufferCapacity
}

// inflightFetchLocalReserve keeps inflight-fetch slots free for local-DRAM
// fetches (the serve sink) so a remote-fetch flood cannot starve them.
const inflightFetchLocalReserve = 4

// inflightFetchBlocked is the reserve-aware inflight admission test: a local
// fetch may use the full budget; a remote fetch must leave the local reserve
// free.
func (wb *writeBufferStage) inflightFetchBlocked(toRemote bool) bool {
	if !toRemote {
		return wb.inflightFetch.Size() >= wb.maxInflightFetch
	}
	return wb.inflightFetch.Size() >= wb.maxInflightFetch-inflightFetchLocalReserve
}

func (wb *writeBufferStage) tooManyInflightEvictions() bool {
	return wb.inflightEviction.Size() >= wb.maxInflightEviction
}

func (wb *writeBufferStage) Reset(now akita.VTimeInSec) {
	wb.cache.writeBufferBuffer.Clear()
}

// installFetchedLine commits a fetched line into the resident slot.
// It writes data to storage, marks the block valid+unlocked, stamps
// the incoming line's hotness onto the slot, satisfies the MSHR entry
// so upstream readers get the data, and re-evaluates the local-home
// bit for the new tag.
//
// The resident's old hotness was preserved on the live block
// throughout the in-flight period (the directory stage no longer
// resets it to Cold up-front), so this is the single point where the
// transition to the new line's hotness becomes visible.
//
// incomingHotness is what the just-fetched line itself carries:
//   - DRAM responses carry no DistInfo, so callers pass Cold.
//   - RDMA DataReadyRsp callers pull from rsp.Info.DistInfo and
//     normalize via storableRdmaHotness.
//
// Shared by both DRAM and RDMA DataReadyRsp handlers; bottomPort
// selects which port to drain.
func (wb *writeBufferStage) installFetchedLine(
	now akita.VTimeInSec,
	trans *transaction,
	block *Block,
	mshrEntry *MSHREntry,
	bottomPort akita.Port,
	incomingHotness protocol.Hotness,
	incomingIsPte bool,
	idx int,
) {
	// Trace the OLD slot's disposition before storage.Write destroys
	// its data. Bypass goes through bypassFetchedLine and skips this
	// function entirely, so no trace fires on bypass — by construction
	// only actual installs reach here.
	//
	// trans.victim is the directorystage snapshot of the resident line
	// taken before any in-flight mutations. Defensive nil-check guards
	// future install paths that might forget to populate the snapshot.
	var evictorReq protocol.AccessReq
	if trans.read != nil {
		evictorReq = trans.read
	} else if trans.write != nil {
		evictorReq = trans.write
	}

	if trans.victim != nil && trans.victim.IsValid {
		name := "overwrite"
		memTraceID := uint64(7)
		victimMemTraceID := uint64(9)
		distType := tracing.TYPE_OVERWRITE_LOCAL_HOME_CACHELINE
		if trans.victim.IsDirty {
			name = "evict"
			memTraceID = 6
			victimMemTraceID = 8
			distType = tracing.TYPE_EVICT_LOCAL_HOME_CACHELINE
		}
		victimHome := wb.cache.chipletIDStrings[wb.cache.HomeNodeFinder.GetChipletId(trans.victim.Tag)]

		tracing.StartTask(0, 0, now, wb.cache, name, victimHome, nil)

		if evictorReq != nil {
			tracing.StartTask(
				memTraceID,
				1,
				now,
				wb.cache,
				"memory-trace",
				victimHome,
				evictorReq,
			)
		}

		tracing.StartTask(
			victimMemTraceID,
			1,
			now,
			wb.cache,
			"memory-trace",
			victimHome,
			&tracing.VictimInfo{
				VictimAddr: trans.victim.Tag,
				VictimPC:   trans.victim.InstallPC,
			},
		)

		if trans.victim.virtualBitForLocalHome {
			tracing.StartTask(
				0,
				0,
				now,
				wb.cache,
				fmt.Sprintf("%s-local-home-cacheline", name),
				"",
				&tracing.DistInfo{
					Addr:       trans.victim.Tag,
					PC:         trans.victim.InstallPC,
					Home:       wb.cache.HomeNodeFinder.GetChipletId(trans.victim.Tag),
					ToResponse: wb.cache.ChipletId,
					Hotness:    trans.victim.Hotness,
					DistType:   distType,
				})
		}
	}

	block.IsLocked = false
	block.IsValid = true
	block.InstallPC = tracing.PCFromReq(evictorReq)

	block.Hotness = incomingHotness
	block.IsPte = incomingIsPte

	// Tag the install source so MSHRFailTracer can bucket merge
	// histograms by where the fetch landed (DRAM vs remote RDMA).
	mergeSrc := "dram"
	if bottomPort == wb.cache.BottomToRdmaPort {
		mergeSrc = "rdma"
	}
	tracing.StartTask(0, 0, now, wb.cache,
		"mshr-merge", mergeSrc, len(mshrEntry.Requests))

	wb.cache.mshrStageBuffer.Push(mshrEntry)
	wb.cache.storage.Write(block.CacheAddress, mshrEntry.Data)

	wb.removeInflightFetch(trans)
	bottomPort.(*akita.LimitNumMsgPort).RetrieveAt(now, idx)

	tracing.TraceReqFinalize(trans.fetchReadReq, now, wb.cache)
}

// bypassFetchedLine satisfies the requester with the fetched data but
// leaves the resident line alone. The slot is restored from
// trans.victim (Tag/PID/Hotness/owner-bit/dirty-mask), storage is NOT
// written, and the FindVictim side-effects are NOT committed — so the
// resident line keeps its hotness and the passed-over hot blocks keep
// theirs as well.
//
// IsDirty is unconditionally cleared by restoreVictim: any concurrent
// writeback of the original dirty data is allowed to complete, after
// which cache and memory hold the same bytes.
func (wb *writeBufferStage) bypassFetchedLine(
	now akita.VTimeInSec,
	trans *transaction,
	block *Block,
	mshrEntry *MSHREntry,
	bottomPort akita.Port,
	idx int,
) {
	restoreVictim(block, trans.victim)

	wb.cache.mshrStageBuffer.Push(mshrEntry)
	wb.removeInflightFetch(trans)
	bottomPort.(*akita.LimitNumMsgPort).RetrieveAt(now, idx)

	tracing.TraceReqFinalize(trans.fetchReadReq, now, wb.cache)
	tracing.StartTask(0, 0, now, wb.cache,
		"bypassed-by-hotness", "", nil)
}
