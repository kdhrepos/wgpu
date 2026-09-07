package writebackrcr

import (
	"fmt"
	"log"
	"reflect"

	"gitlab.com/akita/akita"
	"gitlab.com/akita/mem/cache"
	"gitlab.com/akita/util/global"
	"gitlab.com/akita/util/tracing"
)

type flusher struct {
	cache *Cache

	blockToEvict           []*cache.Block
	blockToEvictFromRemote []*cache.Block
	processingFlush        *cache.FlushReq
}

func (f *flusher) Tick(now akita.VTimeInSec) bool {
	if f.processingFlush != nil && f.cache.state == cacheStatePreFlushing {
		return f.processPreFlushing(now)
	}

	madeProgress := false
	if f.processingFlush != nil && f.cache.state == cacheStateFlushing {
		madeProgress = f.finalizeFlushing(now) || madeProgress
		// madeProgress = f.processPostFlush(now) || madeProgress // only used for writeback cache
		madeProgress = f.processFlush(now) || madeProgress
		return madeProgress
	}

	return f.extractFromPort(now)
}

func (f *flusher) processPreFlushing(now akita.VTimeInSec) bool {
	// Guarantees inflight transactions not corrupted.
	if f.existInflightTransaction() {
		return false
	}

	// Get all the cachelines being evicted from the cache directory.
	f.prepareBlockToFlushList()
	// Now, cache could be safely flushed.
	f.cache.state = cacheStateFlushing

	return true
}

func (f *flusher) existInflightTransaction() bool {
	return len(f.cache.inFlightTransactions) > 0
}

func (f *flusher) prepareBlockToFlushList() {
	sets := f.cache.directory.GetSets()
	for _, set := range sets {
		for _, block := range set.Blocks {
			if block.ReadCount > 0 || block.IsLocked {
				panic("all the blocks should be unlocked before flushing")
			}

			if block.IsValid && block.IsDirty {
				f.blockToEvict = append(f.blockToEvict, block)
			}
		}
	}
}

func (f *flusher) finalizeFlushing(now akita.VTimeInSec) bool {
	if len(f.blockToEvict) > 0 {
		return false
	}

	if !f.flushCompleted() {
		return false
	}

	// wait for evictions from remote chiplets
	// if f.cache.TopPort.Peek() != nil {
	// 	return false
	// }

	if !f.cache.controlPortSender.CanSend(1) {
		return false
	}

	if global.GetInstance().GetVerbose() {
		fmt.Println("finalizeFlushing", f.cache.Name())
	}

	rsp := cache.FlushRspBuilder{}.
		WithSendTime(now).
		WithSrc(f.cache.ControlPort).
		WithDst(f.processingFlush.Src).
		WithRspTo(f.processingFlush.ID).
		Build()
	f.cache.controlPortSender.Send(rsp)

	f.cache.mshr.Reset()
	f.cache.directory.Reset()

	if f.processingFlush.PauseAfterFlushing {
		f.cache.state = cacheStatePaused
	} else {
		f.cache.state = cacheStateRunning
	}

	tracing.TraceReqComplete(f.processingFlush, now, f.cache)
	f.processingFlush = nil

	return true
}

func (f *flusher) flushCompleted() bool {
	for _, b := range f.cache.dirToBankBuffers {
		if b.Size() > 0 {
			return false
		}
	}

	for _, b := range f.cache.bankStages {
		if b.currentTrans != nil {
			return false
		}
	}

	if f.cache.writeBufferBuffer.Size() > 0 {
		return false
	}

	if f.cache.writeBuffer.inflightFetch.Size() > 0 ||
		f.cache.writeBuffer.inflightEviction.Size() > 0 ||
		f.cache.writeBuffer.pendingEvictions.Size() > 0 {
		return false
	}

	return true
}

// func (f *flusher) processPostFlush(now akita.VTimeInSec) bool {
// 	if len(f.blockToEvict) != 0 {
// 		return false
// 	}

// 	// check if the processFlush is done.
// 	if f.cache.RDMAPort.Peek() == nil ||
// 		f.cache.state != cacheStateFlushing {
// 		return false
// 	}
// 	if f.cache.writeBuffer.tooManyInflightEvictions() {
// 		return false
// 	}

// 	// need to send write done rsp to remote chiplet
// 	// and write request (eviction) to dram
// 	if !f.cache.bottomSender.CanSend(1) || !f.cache.rdmaSender.CanSend(1) {
// 		return true // return false
// 	}
// 	req := f.cache.RDMAPort.Peek()

// 	writeReq, ok := req.(*mem.WriteReq)
// 	if !ok {
// 		panic("Something's wrong with PostFlush")
// 	}

// 	cachelineID, _ := cache.GetCacheLineID(
// 		writeReq.GetAddress(), f.cache.log2BlockSize)

// 	block := f.cache.directory.FindVictim(cachelineID)
// 	block.Tag = writeReq.GetAddress()
// 	block.PID = writeReq.GetPID()

// 	lowModulePort := f.cache.lowModuleFinder.Find(writeReq.GetAddress())

// 	if strings.Contains(lowModulePort.Name(), "RDMA") {
// 		panic(fmt.Sprint("PostFlush write to RDMA", writeReq.GetAddress()))
// 	}

// 	if global.GetInstance().GetVerbose() {
// 		fmt.Println("processPostFlush", f.cache.Name())
// 	}

// 	write := mem.WriteReqBuilder{}.
// 		WithSrc(f.cache.BottomPort).
// 		WithDst(lowModulePort).
// 		WithPID(writeReq.GetPID()).
// 		WithAddress(writeReq.GetAddress()).
// 		WithData(writeReq.Data).
// 		WithDirtyMask(writeReq.DirtyMask).
// 		Build()
// 	f.cache.bottomSender.Send(write)

// 	writeRsp := mem.WriteDoneRspBuilder{}.
// 		WithSendTime(now).
// 		WithSrc(f.cache.RDMAPort).
// 		WithDst(writeReq.Src).
// 		WithRspTo(writeReq.ID).
// 		Build()
// 	f.cache.rdmaSender.Send(writeRsp)

// 	trans := &transaction{
// 		flush:             f.processingFlush,
// 		victim:            block,
// 		action:            bankEvict,
// 		evictingPID:       writeReq.GetPID(),
// 		evictingAddr:      writeReq.GetAddress(),
// 		evictingData:      writeReq.Data,
// 		evictingDirtyMask: write.DirtyMask,
// 		evictionWriteReq:  write,
// 	}

// 	f.cache.writeBuffer.inflightEviction = append(f.cache.writeBuffer.inflightEviction, trans)

// 	tracing.TraceReqInitiate(write, now, f.cache.writeBuffer.cache,
// 		tracing.MsgIDAtReceiver(trans.req(), f.cache.writeBuffer.cache))

// 	f.cache.RDMAPort.Retrieve(now)

// 	return true
// }

func (f *flusher) processFlush(now akita.VTimeInSec) bool {
	if len(f.blockToEvict) == 0 {
		return false
	}

	block := f.blockToEvict[0]
	bankNum := cache.GetBankID(
		block,
		f.cache.directory.WayAssociativity(),
		len(f.cache.dirToBankBuffers))
	bankBuf := f.cache.dirToBankBuffers[bankNum]

	if !bankBuf.CanPush() {
		return false
	}

	trans := &transaction{
		flush:             f.processingFlush,
		victim:            block,
		action:            bankEvict,
		evictingAddr:      block.Tag,
		evictingDirtyMask: block.DirtyMask,
	}
	bankBuf.Push(trans)

	f.blockToEvict = f.blockToEvict[1:]

	return true
}

func (f *flusher) extractFromPort(now akita.VTimeInSec) bool {
	item := f.cache.ControlPort.Peek()
	if item == nil {
		return false
	}

	switch req := item.(type) {
	case *cache.FlushReq:
		return f.startProcessingFlush(now, req)
	case *cache.RestartReq:
		return f.handleCacheRestart(now, req)
	default:
		log.Panicf("Cannot process request of %s", reflect.TypeOf(req))
	}

	return true
}

func (f *flusher) startProcessingFlush(
	now akita.VTimeInSec,
	req *cache.FlushReq,
) bool {
	f.processingFlush = req
	if req.DiscardInflight {
		f.cache.discardInflightTransactions(now)
	}

	f.cache.state = cacheStatePreFlushing
	f.cache.ControlPort.Retrieve(now)

	tracing.TraceReqReceive(req, now, f.cache)

	return true
}

func (f *flusher) handleCacheRestart(
	now akita.VTimeInSec,
	req *cache.RestartReq,
) bool {
	if !f.cache.controlPortSender.CanSend(1) {
		return false
	}

	cache.ClearPort(f.cache.TopPort, now)
	cache.ClearPort(f.cache.RDMAPort, now)
	cache.ClearPort(f.cache.BottomToDramPort, now)
	cache.ClearPort(f.cache.BottomToRdmaPort, now)

	f.cache.state = cacheStateRunning

	rsp := cache.RestartRspBuilder{}.
		WithSendTime(now).
		WithSrc(f.cache.ControlPort).
		WithDst(req.Src).
		WithRspTo(req.ID).
		Build()
	f.cache.controlPortSender.Send(rsp)

	f.cache.ControlPort.Retrieve(now)

	return true
}
