package writebacksac

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

	// SAC pass-through requests forwarded to the bottom; matched against
	// bottom responses by ptForwardedReq ID. Capped by maxInflightFetch.
	inflightPassThrough *SeqBuffer
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
		bottomPort.(*akita.LimitNumMsgPort).HeadToTail()
		// return true
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
	case writeBufferPassThrough:
		return wb.forwardPassThrough(now, trans)
	default:
		panic("unknown transaction action")
	}
}

// forwardPassThrough forwards a SAC pass-through request 1:1 to the bottom
// (RDMA toward the home L2 for memory-side local requests to remote
// addresses, DRAM for SM-side inbound requests). No directory or MSHR
// state is touched; the response is matched in the bottom response
// handlers and forwarded back to the original requester.
func (wb *writeBufferStage) forwardPassThrough(
	now akita.VTimeInSec,
	trans *transaction,
) bool {
	if wb.inflightPassThrough.Size() >= wb.maxInflightFetch {
		return false
	}

	req := trans.accessReq()
	addr := req.GetAddress()
	lowModulePort := wb.cache.lowModuleFinder.Find(addr)

	var bottomPort akita.Port
	var bottomSender akitaext.BufferedSender
	toRdma := strings.Contains(lowModulePort.Name(), "RDMA")
	if toRdma {
		bottomPort = wb.cache.BottomToRdmaPort
		bottomSender = wb.cache.bottomToRdmaSender
	} else {
		bottomPort = wb.cache.BottomToDramPort
		bottomSender = wb.cache.bottomToDramSender
	}

	if !bottomSender.CanSend(1) {
		return false
	}

	var fwd akita.Msg
	if trans.read != nil {
		read := protocol.ReadReqBuilder{}.
			WithSrc(bottomPort).
			WithDst(lowModulePort).
			WithPID(trans.read.PID).
			WithAddress(trans.read.Address).
			WithVAddr(trans.read.VAddr).
			WithByteSize(trans.read.AccessByteSize).
			WithInfo(trans.read.Info).
			Build()
		read.RequesterChiplet = wb.cache.ChipletId
		if trans.read.IsPtw ||
			strings.Contains(trans.read.Meta().Src.Name(), "MMU") {
			read.IsPtw = true
		}
		fwd = read
	} else {
		write := protocol.WriteReqBuilder{}.
			WithSrc(bottomPort).
			WithDst(lowModulePort).
			WithPID(trans.write.PID).
			WithAddress(trans.write.Address).
			WithData(trans.write.Data).
			WithDirtyMask(trans.write.DirtyMask).
			WithInfo(trans.write.Info).
			Build()
		write.RequesterChiplet = wb.cache.ChipletId
		write.IsDemandWrite = true
		fwd = write
	}
	trans.ptForwardedReq = fwd

	if toRdma {
		bottomSender.Send(fwd)
	} else {
		bottomSender.Send(&idealmemcontroller.ToDramMsg{
			Msg:       fwd,
			FromLocal: true,
		})
	}

	wb.inflightPassThrough.Push(trans)

	tracing.TraceReqInitiate(fwd, now, wb.cache,
		tracing.MsgIDAtReceiver(trans.req(), wb.cache))

	wb.cache.writeBufferBuffer.Pop()

	return true
}

// findPassThroughByRspID returns the in-flight pass-through transaction
// whose forwarded request matches the given response ID, or nil.
func (wb *writeBufferStage) findPassThroughByRspID(id uint64) *transaction {
	for _, e := range wb.inflightPassThrough.elements {
		trans := e.(*transaction)
		if trans.ptForwardedReq.Meta().ID == id {
			return trans
		}
	}
	return nil
}

func (wb *writeBufferStage) removePassThrough(trans *transaction) {
	for i, e := range wb.inflightPassThrough.elements {
		if e.(*transaction) == trans {
			wb.inflightPassThrough.elements = append(
				wb.inflightPassThrough.elements[:i],
				wb.inflightPassThrough.elements[i+1:]...,
			)
			return
		}
	}
	panic("pass-through transaction not found")
}

// completePassThroughRead forwards the bottom DataReadyRsp of a
// pass-through read back to the original requester.
func (wb *writeBufferStage) completePassThroughRead(
	now akita.VTimeInSec,
	trans *transaction,
	dataReady *protocol.DataReadyRsp,
	bottomPort akita.Port,
) bool {
	read := trans.read

	var srcPort akita.Port
	var sender akitaext.BufferedSender
	if strings.Contains(read.Meta().Src.Name(), "RDMA") {
		srcPort = wb.cache.RDMAPort
		sender = wb.cache.rdmaSender
	} else {
		srcPort = wb.cache.TopPort
		sender = wb.cache.topSender
	}

	if !sender.CanSend(1) {
		return false
	}

	if trans.toRemote {
		wb.cache.numToRemote--
	}
	wb.removePassThrough(trans)
	bottomPort.Retrieve(now)

	// The original request may have been wiped by a DiscardInflight flush;
	// in that case the response is simply dropped.
	if !wb.cache.removeInFlightTransaction(trans) {
		return true
	}

	rsp := protocol.DataReadyRspBuilder{}.
		WithSendTime(now).
		WithSrc(srcPort).
		WithDst(read.Src).
		WithRspTo(read.ID).
		WithData(dataReady.Data).
		Build()
	sender.Send(rsp)

	tracing.TraceReqFinalize(trans.ptForwardedReq, now, wb.cache)
	tracing.TraceReqComplete(read, now, wb.cache)

	return true
}

// completePassThroughWrite forwards the bottom WriteDoneRsp of a
// pass-through write back to the original requester.
func (wb *writeBufferStage) completePassThroughWrite(
	now akita.VTimeInSec,
	trans *transaction,
	bottomPort akita.Port,
) bool {
	write := trans.write

	var srcPort akita.Port
	var sender akitaext.BufferedSender
	if strings.Contains(write.Meta().Src.Name(), "RDMA") {
		srcPort = wb.cache.RDMAPort
		sender = wb.cache.rdmaSender
	} else {
		srcPort = wb.cache.TopPort
		sender = wb.cache.topSender
	}

	if !sender.CanSend(1) {
		return false
	}

	if trans.toRemote {
		wb.cache.numToRemote--
	}
	wb.removePassThrough(trans)
	bottomPort.Retrieve(now)

	// The original request may have been wiped by a DiscardInflight flush;
	// in that case the response is simply dropped.
	if !wb.cache.removeInFlightTransaction(trans) {
		return true
	}

	rsp := protocol.WriteDoneRspBuilder{}.
		WithSendTime(now).
		WithSrc(srcPort).
		WithDst(write.Src).
		WithRspTo(write.ID).
		Build()
	sender.Send(rsp)

	tracing.TraceReqFinalize(trans.ptForwardedReq, now, wb.cache)
	tracing.TraceReqComplete(write, now, wb.cache)

	return true
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
	if wb.tooManyInflightFetches() {
		return false
	}

	// At this point, lmf tells whether this address
	// goes to the remote chiplet or local DRAM.
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
			tracing.StartTask(
				read.Meta().ID,
				0, 0,
				wb.cache,
				"pte-caching-remote-ptw",
				"", nil)
			read.IsPtw = true
		}

		bottomSender.Send(read)
	} else {
		if trans.read != nil &&
			strings.Contains(trans.read.Meta().Src.Name(), "MMU") {
			tracing.StartTask(
				0, 0, 0,
				wb.cache,
				"pte-caching-local-ptw",
				"", nil)
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

	if !bankBuf.CanPush() {
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
	if pt := wb.findPassThroughByRspID(dataReady.RespondTo); pt != nil {
		return wb.completePassThroughRead(
			now, pt, dataReady, wb.cache.BottomToDramPort)
	}

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
	if pt := wb.findPassThroughByRspID(writeDone.RespondTo); pt != nil {
		return wb.completePassThroughWrite(
			now, pt, wb.cache.BottomToDramPort)
	}

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
	if pt := wb.findPassThroughByRspID(dataReady.RespondTo); pt != nil {
		return wb.completePassThroughRead(
			now, pt, dataReady, wb.cache.BottomToRdmaPort)
	}

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
	if pt := wb.findPassThroughByRspID(writeDone.RespondTo); pt != nil {
		return wb.completePassThroughWrite(
			now, pt, wb.cache.BottomToRdmaPort)
	}

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

func (wb *writeBufferStage) tooManyInflightFetches() bool {
	// return len(wb.inflightFetch) >= wb.maxInflightFetch
	return wb.inflightFetch.Size() >= wb.maxInflightFetch
}

func (wb *writeBufferStage) tooManyInflightEvictions() bool {
	// return len(wb.inflightEviction) >= wb.maxInflightEviction
	return wb.inflightEviction.Size() >= wb.maxInflightEviction
}

func (wb *writeBufferStage) Reset(now akita.VTimeInSec) {
	wb.cache.writeBufferBuffer.Clear()
}
