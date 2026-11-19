package writeback

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
		return false
	}

	var info any
	if trans.accessReq() != nil {
		info = trans.accessReq().GetAccessInfo()
	}

	write := protocol.WriteReqBuilder{}.
		WithSrc(bottomPort).
		WithDst(lowModulePort).
		WithPID(trans.evictingPID).
		WithAddress(trans.evictingAddr).
		WithInfo(info).
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

	tracing.StartTask(
		0, 0, now, wb.cache,
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
		// bottomPort.(*akita.LimitNumMsgPort).HeadToTail()
		return false
		// return true
	}

	readReqBuilder := protocol.ReadReqBuilder{}.
		WithSrc(bottomPort).
		WithDst(lowModulePort). // this could be DRAM Top or RDMA ToL2
		WithPID(trans.fetchPID).
		WithAddress(trans.fetchAddress).
		WithByteSize(1 << wb.cache.log2BlockSize)

	// need to include if the request is remote access
	if bottomPort == wb.cache.BottomToRdmaPort &&
		trans.read != nil {
		// need to check this is not a instruction fetch
		readReqBuilder = readReqBuilder.WithInfo(trans.read.Info)
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
	// wb.inflightFetch = append(wb.inflightFetch, trans)
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
		// return true
		return false
	}

	bankNum := cache.GetBankID(
		trans.block,
		wb.cache.directory.WayAssociativity(),
		len(wb.cache.dirToBankBuffers),
	)
	bankBuf := wb.cache.writeBufferToBankBuffers[bankNum]

	if !bankBuf.CanPushTransReserve(trans.toRemote, smartBufferLocalReserve) {
		// return true
		return false
	}

	trans.action = bankWriteHit
	bankBuf.Push(trans)

	// wb.pendingEvictions = append(wb.pendingEvictions, trans)
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
		// return false
		return true
	}

	trans.fetchedData = dataReady.Data
	trans.action = bankWriteFetched
	trans.mshrEntry.Data = dataReady.Data
	wb.combineData(trans.mshrEntry)

	wb.cache.mshr.Remove(trans.mshrEntry.PID, trans.mshrEntry.Address)

	mshrEntry := trans.mshrEntry
	block := mshrEntry.Block
	block.IsLocked = false
	block.IsValid = true

	tracing.StartTask(
		0, 0, now, wb.cache,
		"mshr-merge", "dram", len(mshrEntry.Requests))

	wb.cache.mshrStageBuffer.Push(mshrEntry) // wake up the mshr entry
	wb.cache.storage.Write(block.CacheAddress, mshrEntry.Data)

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
		// return false
		return true
	}

	trans.fetchedData = dataReady.Data
	trans.action = bankWriteFetched
	trans.mshrEntry.Data = dataReady.Data
	wb.combineData(trans.mshrEntry)

	wb.cache.mshr.Remove(trans.mshrEntry.PID, trans.mshrEntry.Address)
	wb.cache.numToRemote--

	mshrEntry := trans.mshrEntry
	block := mshrEntry.Block
	block.IsLocked = false
	block.IsValid = true

	tracing.StartTask(
		0, 0, now, wb.cache,
		"mshr-merge", "rdma", len(mshrEntry.Requests))

	wb.cache.mshrStageBuffer.Push(mshrEntry) // wake up the mshr entry
	wb.cache.storage.Write(block.CacheAddress, mshrEntry.Data)

	wb.removeInflightFetch(trans)
	wb.cache.BottomToRdmaPort.Retrieve(now)

	tracing.TraceReqFinalize(trans.fetchReadReq, now, wb.cache)

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
	// numEntry := len(wb.pendingEvictions) + len(wb.inflightEviction)
	// numEntry := len(wb.pendingEvictions) + wb.inflightEviction.Size()
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
	// return len(wb.inflightEviction) >= wb.maxInflightEviction
	return wb.inflightEviction.Size() >= wb.maxInflightEviction
}

func (wb *writeBufferStage) Reset(now akita.VTimeInSec) {
	wb.cache.writeBufferBuffer.Clear()
}
