package cu

import (
	"fmt"
	"log"
	"reflect"

	"gitlab.com/akita/akita"
	"gitlab.com/akita/lmf"
	"gitlab.com/akita/mgpusim/coreprotocol"
	"gitlab.com/akita/mgpusim/emu"
	"gitlab.com/akita/mgpusim/insts"
	"gitlab.com/akita/mgpusim/kernels"
	"gitlab.com/akita/mgpusim/timing/wavefront"
	"gitlab.com/akita/protocol"
	"gitlab.com/akita/util"
	"gitlab.com/akita/util/akitaext"
	"gitlab.com/akita/util/global"
	"gitlab.com/akita/util/tracing"
)

// DeadlockSummary reports every external port, in-flight memory/inst-fetch
// list, sub-component IsIdle status (vmem unit, scalar unit, branch, LDS,
// decoders, all SIMDs), shadow-buffer counts, scheduler pause/flush state,
// and the wavefront state histogram. anyPending is true iff any port has a
// queued msg, any in-flight bucket is non-empty, or any sub-unit is busy.
func (cu *ComputeUnit) DeadlockSummary() (line string, anyPending bool) {
	toACEQ := cu.ToACE != nil && cu.ToACE.Peek() != nil
	toCPQ := cu.ToCP != nil && cu.ToCP.Peek() != nil
	toInstQ := cu.ToInstMem != nil && cu.ToInstMem.Peek() != nil
	toScalarQ := cu.ToScalarMem != nil && cu.ToScalarMem.Peek() != nil
	toVecQ := cu.ToVectorMem != nil && cu.ToVectorMem.Peek() != nil
	instN := len(cu.InFlightInstFetch)
	scalN := len(cu.InFlightScalarMemAccess)
	vecN := len(cu.InFlightVectorMemAccess)
	shadowInstN := len(cu.shadowInFlightInstFetch)
	shadowScalN := len(cu.shadowInFlightScalarMemAccess)
	shadowVecN := len(cu.shadowInFlightVectorMemAccess)

	notIdle := func(s SubComponent) bool {
		return s != nil && !s.IsIdle()
	}
	busyBU := notIdle(cu.BranchUnit)
	busyVMU := notIdle(cu.VectorMemUnit)
	busySU := notIdle(cu.ScalarUnit)
	busyLDS := notIdle(cu.LDSUnit)
	busyVMD := notIdle(cu.VectorMemDecoder)
	busySD := notIdle(cu.ScalarDecoder)
	busyVD := notIdle(cu.VectorDecoder)
	busyLD := notIdle(cu.LDSDecoder)
	busySIMDs := 0
	for _, s := range cu.SIMDUnit {
		if notIdle(s) {
			busySIMDs++
		}
	}

	wfTotal := 0
	wfRunning := 0
	wfReady := 0
	wfBarrier := 0
	wfDispatch := 0
	wfCompleted := 0
	for _, p := range cu.WfPools {
		if p == nil {
			continue
		}
		for _, wf := range p.wfs {
			wfTotal++
			switch wf.State {
			case wavefront.WfRunning:
				wfRunning++
			case wavefront.WfReady:
				wfReady++
			case wavefront.WfAtBarrier:
				wfBarrier++
			case wavefront.WfDispatching:
				wfDispatch++
			case wavefront.WfCompleted:
				wfCompleted++
			}
		}
	}

	anyPending = toACEQ || toCPQ || toInstQ || toScalarQ || toVecQ ||
		instN > 0 || scalN > 0 || vecN > 0 ||
		shadowInstN > 0 || shadowScalN > 0 || shadowVecN > 0 ||
		busyBU || busyVMU || busySU || busyLDS ||
		busyVMD || busySD || busyVD || busyLD || busySIMDs > 0 ||
		cu.isPaused || cu.currentFlushReq != nil ||
		wfTotal > 0
	line = fmt.Sprintf(
		"toACE=%v toCP=%v toInst=%v toScalar=%v toVec=%v "+
			"instFetch=%d scalarMem=%d vecMem=%d shadow(inst=%d scal=%d vec=%d) "+
			"busy(BU=%v VMU=%v SU=%v LDS=%v VMD=%v SD=%v VD=%v LD=%v SIMD=%d/%d) "+
			"paused=%v flush=%v wf=%d(run=%d ready=%d barrier=%d disp=%d done=%d)",
		toACEQ, toCPQ, toInstQ, toScalarQ, toVecQ,
		instN, scalN, vecN, shadowInstN, shadowScalN, shadowVecN,
		busyBU, busyVMU, busySU, busyLDS, busyVMD, busySD, busyVD, busyLD,
		busySIMDs, len(cu.SIMDUnit),
		cu.isPaused, cu.currentFlushReq != nil,
		wfTotal, wfRunning, wfReady, wfBarrier, wfDispatch, wfCompleted)
	return
}

// oooDebugOn gates all [ooo-dbg]/[ooo-bug] verbose prints and the matching
// assertion checks in the CU vmem path. Toggled by the runner's -ooo-debug
// flag (see runner/runner.go).
func oooDebugOn() bool { return global.GetInstance().GetOOODebug() }

// A ComputeUnit in the timing package provides a detailed and accurate
// simulation of a GCN3 ComputeUnit
type ComputeUnit struct {
	*akita.TickingComponent

	WfDispatcher WfDispatcher
	Decoder      emu.Decoder
	WfPools      []*WavefrontPool

	InFlightInstFetch            []*InstFetchReqInfo
	InFlightScalarMemAccess      []*ScalarMemAccessInfo
	InFlightVectorMemAccess      []VectorMemAccessInfo
	InFlightVectorMemAccessLimit int

	shadowInFlightInstFetch       []*InstFetchReqInfo
	shadowInFlightScalarMemAccess []*ScalarMemAccessInfo
	shadowInFlightVectorMemAccess []VectorMemAccessInfo

	running bool

	Scheduler        Scheduler
	BranchUnit       SubComponent
	VectorMemDecoder SubComponent
	VectorMemUnit    SubComponent
	ScalarDecoder    SubComponent
	VectorDecoder    SubComponent
	LDSDecoder       SubComponent
	ScalarUnit       SubComponent
	SIMDUnit         []SubComponent
	LDSUnit          SubComponent
	SRegFile         RegisterFile
	VRegFile         []RegisterFile

	InstMem          akita.Port
	ScalarMem        akita.Port
	VectorMemModules lmf.LowModuleFinder

	ToACE       akita.Port
	toACESender akitaext.BufferedSender
	ToInstMem   akita.Port
	ToScalarMem akita.Port
	ToVectorMem akita.Port
	ToCP        akita.Port

	inCPRequestProcessingStage akita.Msg
	cpRequestHandlingComplete  bool

	isFlushing                   bool
	isPaused                     bool
	isSendingOutShadowBufferReqs bool
	isHandlingWfCompletionEvent  bool

	toSendToCP akita.Msg

	currentFlushReq   *coreprotocol.CUPipelineFlushReq
	currentRestartReq *coreprotocol.CUPipelineRestartReq

	// vgprLoadWrite records the last vector-memory load that wrote each VGPR,
	// keyed by "SIMD/waveOffset/lane/regIndex". Debug-only (-ooo-debug): used by
	// dumpAddrVGPRProvenance to explain a stale/NULL FLAT address register —
	// did a load ever write it, with what value, or is the load still in flight?
	vgprLoadWrite map[string]vgprWriteRec

	// sgprLoadWrite mirrors vgprLoadWrite for scalar registers, keyed by
	// "sRegOffset/regIndex". A base-pointer SGPR reading 0 is explained here:
	// written by a scalar load with value 0, or never written by a load.
	sgprLoadWrite map[string]vgprWriteRec

	// scalarAddrLastVal tracks the last 8-byte value each scalar-loaded
	// address returned. Debug-only (-ooo-debug). When a later scalar load of
	// the SAME address returns a DIFFERENT value, that is logged — this
	// distinguishes a kernarg that was valid then went stale (cache/coherence
	// corruption) from one that was 0 from the start (setup-vs-read race).
	scalarAddrLastVal map[uint64]uint64
}

// vgprWriteRec is one VGPR write attributed to a vector-memory load.
type vgprWriteRec struct {
	pc    uint64
	val   uint32
	time  akita.VTimeInSec
	wfUID string
}

// Handle processes that events that are scheduled on the ComputeUnit
func (cu *ComputeUnit) Handle(evt akita.Event) error {
	ctx := akita.HookCtx{
		Domain: cu,
		Now:    evt.Time(),
		Pos:    akita.HookPosBeforeEvent,
		Item:   evt,
	}
	cu.InvokeHook(ctx)

	cu.Lock()
	defer cu.Unlock()

	switch evt := evt.(type) {
	case akita.TickEvent:
		cu.TickingComponent.Handle(evt)
	case *WfCompletionEvent:
		cu.handleWfCompletionEvent(evt)
	default:
		log.Panicf("Unable to process evevt of type %s",
			reflect.TypeOf(evt))
	}

	ctx.Pos = akita.HookPosAfterEvent
	cu.InvokeHook(ctx)

	return nil
}

// ControlPort returns the port that can receive controlling messages from the
// Command Processor.
func (cu *ComputeUnit) ControlPort() akita.Port {
	return cu.ToCP
}

// DispatchingPort returns the port that the dispatcher can use to dispatch
// work-groups to the CU.
func (cu *ComputeUnit) DispatchingPort() akita.Port {
	return cu.ToACE
}

// WfPoolSizes returns an array of the numbers of wavefronts that each SIMD unit
// can execute.
func (cu *ComputeUnit) WfPoolSizes() []int {
	return []int{10, 10, 10, 10}
}

// VRegCounts returns an array of the numbers of vector regsiters in each SIMD
// unit.
func (cu *ComputeUnit) VRegCounts() []int {
	return []int{16384, 16384, 16384, 16384}
}

// SRegCount returns the number of scalar register in the Compute Unit.
func (cu *ComputeUnit) SRegCount() int {
	return 3200
}

// LDSBytes returns the number of bytes in the LDS of the CU.
func (cu *ComputeUnit) LDSBytes() int {
	return 64 * 1024
}

// Tick ticks
func (cu *ComputeUnit) Tick(now akita.VTimeInSec) bool {
	cu.Lock()
	defer cu.Unlock()

	madeProgress := false

	madeProgress = cu.runPipeline(now) || madeProgress
	madeProgress = cu.sendToACE(now) || madeProgress
	madeProgress = cu.sendToCP(now) || madeProgress
	madeProgress = cu.processInput(now) || madeProgress
	madeProgress = cu.doFlush(now) || madeProgress

	return madeProgress
}

//nolint:gocyclo
func (cu *ComputeUnit) runPipeline(now akita.VTimeInSec) bool {
	madeProgress := false

	if !cu.isPaused {
		madeProgress = cu.BranchUnit.Run(now) || madeProgress
		madeProgress = cu.ScalarUnit.Run(now) || madeProgress
		madeProgress = cu.ScalarDecoder.Run(now) || madeProgress
		for _, simdUnit := range cu.SIMDUnit {
			madeProgress = simdUnit.Run(now) || madeProgress
		}
		madeProgress = cu.VectorDecoder.Run(now) || madeProgress
		madeProgress = cu.LDSUnit.Run(now) || madeProgress
		madeProgress = cu.LDSDecoder.Run(now) || madeProgress
		madeProgress = cu.VectorMemUnit.Run(now) || madeProgress
		madeProgress = cu.VectorMemDecoder.Run(now) || madeProgress
		madeProgress = cu.Scheduler.Run(now) || madeProgress
	}

	return madeProgress
}

func (cu *ComputeUnit) doFlush(now akita.VTimeInSec) bool {
	madeProgress := false
	if cu.isFlushing {
		//If a flush request arrives before the shadow buffer requests have been sent out
		if cu.isSendingOutShadowBufferReqs {
			madeProgress = cu.reInsertShadowBufferReqsToOriginalBuffers() || madeProgress
		}
		madeProgress = cu.flushPipeline(now) || madeProgress
	}

	if cu.isSendingOutShadowBufferReqs {
		madeProgress = cu.checkShadowBuffers(now) || madeProgress
	}

	return madeProgress
}

func (cu *ComputeUnit) processInput(now akita.VTimeInSec) bool {
	madeProgress := false

	if !cu.isPaused || cu.isSendingOutShadowBufferReqs {
		madeProgress = cu.processInputFromACE(now) || madeProgress
		madeProgress = cu.processInputFromInstMem(now) || madeProgress
		madeProgress = cu.processInputFromScalarMem(now) || madeProgress
		madeProgress = cu.processInputFromVectorMem(now) || madeProgress
	}

	madeProgress = cu.processInputFromCP(now) || madeProgress

	return madeProgress
}

func (cu *ComputeUnit) processInputFromCP(now akita.VTimeInSec) bool {
	req := cu.ToCP.Retrieve(now)
	if req == nil {
		return false
	}

	cu.inCPRequestProcessingStage = req
	switch req := req.(type) {
	case *coreprotocol.CUPipelineRestartReq:
		cu.handlePipelineResume(now, req)
	case *coreprotocol.CUPipelineFlushReq:
		cu.handlePipelineFlushReq(now, req)
	default:
		panic("unknown msg type")
	}

	return true
}

func (cu *ComputeUnit) handlePipelineFlushReq(
	now akita.VTimeInSec,
	req *coreprotocol.CUPipelineFlushReq,
) error {
	cu.isFlushing = true
	cu.currentFlushReq = req

	return nil
}

func (cu *ComputeUnit) handlePipelineResume(
	now akita.VTimeInSec,
	req *coreprotocol.CUPipelineRestartReq,
) error {
	cu.isSendingOutShadowBufferReqs = true
	cu.currentRestartReq = req

	rsp := coreprotocol.CUPipelineRestartRspBuilder{}.
		WithSrc(cu.ToCP).
		WithDst(cu.currentRestartReq.Src).
		WithSendTime(now).
		Build()
	err := cu.ToCP.Send(rsp)

	if err != nil {
		cu.currentRestartReq = nil
		log.Panicf("Unable to send restart rsp to CP")
	}
	return nil
}

func (cu *ComputeUnit) sendToCP(now akita.VTimeInSec) bool {
	if cu.toSendToCP == nil {
		return false
	}

	cu.toSendToCP.Meta().SendTime = now
	sendErr := cu.ToCP.Send(cu.toSendToCP)
	if sendErr == nil {
		cu.toSendToCP = nil
		return true
	}

	return false
}

func (cu *ComputeUnit) sendToACE(now akita.VTimeInSec) bool {
	return cu.toACESender.Tick(now)
}

func (cu *ComputeUnit) flushPipeline(now akita.VTimeInSec) bool {
	if cu.currentFlushReq == nil {
		return false
	}

	if cu.isHandlingWfCompletionEvent == true {
		return false
	}

	cu.shadowInFlightInstFetch = nil
	cu.shadowInFlightScalarMemAccess = nil
	cu.shadowInFlightVectorMemAccess = nil

	cu.populateShadowBuffers()
	cu.setWavesToReady()
	cu.Scheduler.Flush()
	cu.flushInternalComponents()
	cu.Scheduler.Pause()
	cu.isPaused = true

	respondToCP := coreprotocol.CUPipelineFlushRspBuilder{}.
		WithSendTime(now).
		WithSrc(cu.ToCP).
		WithDst(cu.currentFlushReq.Src).
		Build()
	cu.toSendToCP = respondToCP
	cu.currentFlushReq = nil
	cu.isFlushing = false

	return true
}

func (cu *ComputeUnit) flushInternalComponents() {
	cu.BranchUnit.Flush()

	cu.ScalarUnit.Flush()
	cu.ScalarDecoder.Flush()

	for _, simdUnit := range cu.SIMDUnit {
		simdUnit.Flush()
	}

	cu.VectorDecoder.Flush()
	cu.LDSUnit.Flush()
	cu.LDSDecoder.Flush()
	cu.VectorMemDecoder.Flush()
	cu.VectorMemUnit.Flush()
}

func (cu *ComputeUnit) processInputFromACE(now akita.VTimeInSec) bool {
	req := cu.ToACE.Retrieve(now)
	if req == nil {
		return false
	}

	switch req := req.(type) {
	case *coreprotocol.MapWGReq:
		return cu.handleMapWGReq(now, req)
	default:
		panic("unknown req type")
	}
}

func (cu *ComputeUnit) handleMapWGReq(
	now akita.VTimeInSec,
	req *coreprotocol.MapWGReq,
) bool {
	wg := cu.wrapWG(req.WorkGroup, req)

	tracing.TraceReqReceive(req, now, cu)

	for i, wf := range wg.Wfs {
		location := req.Wavefronts[i]
		cu.WfPools[location.SIMDID].AddWf(wf)
		cu.WfDispatcher.DispatchWf(now, wf, req.Wavefronts[i])
		wf.State = wavefront.WfReady

		tracing.StartTask(tracing.DecimalTaskID(wf.UID),
			tracing.MsgIDAtReceiver(req, cu),
			now,
			cu,
			"wavefront",
			"wavefront",
			nil,
		)
	}

	cu.running = true
	cu.TickLater(now)

	return true
}

func (cu *ComputeUnit) handleWfCompletionEvent(evt *WfCompletionEvent) error {
	now := evt.Time()
	wf := evt.Wf
	wg := wf.WG
	wf.State = wavefront.WfCompleted

	tracing.EndTask(tracing.DecimalTaskID(wf.UID), now, cu)

	if cu.isAllWfInWGCompleted(wg) {
		cu.isHandlingWfCompletionEvent = true

		ok := cu.sendWGCompletionMessage(evt, wg)
		if ok {
			cu.clearWGResource(wg)
			tracing.TraceReqComplete(wg.MapReq, now, cu)
		}

		if !cu.hasMoreWfsToRun() {
			cu.running = false
		}
	}

	cu.TickLater(evt.Time())

	return nil
}

func (cu *ComputeUnit) clearWGResource(wg *wavefront.WorkGroup) {
	for _, wf := range wg.Wfs {
		wfPool := cu.WfPools[wf.SIMDID]
		wfPool.RemoveWf(wf)
	}
}

func (cu *ComputeUnit) isAllWfInWGCompleted(wg *wavefront.WorkGroup) bool {
	for _, wf := range wg.Wfs {
		if wf.State != wavefront.WfCompleted {
			return false
		}
	}
	return true
}

func (cu *ComputeUnit) sendWGCompletionMessage(
	evt *WfCompletionEvent,
	wg *wavefront.WorkGroup,
) bool {
	now := evt.Time()
	mapReq := wg.MapReq
	dispatcher := mapReq.Src

	msg := coreprotocol.WGCompletionMsgBuilder{}.
		WithSendTime(now).
		WithSrc(cu.ToACE).
		WithDst(dispatcher).
		WithRspTo(mapReq.ID).
		Build()

	err := cu.ToACE.Send(msg)
	if err != nil {
		newEvent := NewWfCompletionEvent(
			cu.Freq.NextTick(now), cu, evt.Wf)
		cu.Engine.Schedule(newEvent)
		return false
	}

	tracing.TraceReqComplete(mapReq, now, cu)

	cu.isHandlingWfCompletionEvent = false
	return true
}

func (cu *ComputeUnit) hasMoreWfsToRun() bool {
	for _, wfpool := range cu.WfPools {
		if len(wfpool.wfs) > 0 {
			return true
		}
	}
	return false
}

func (cu *ComputeUnit) wrapWG(
	raw *kernels.WorkGroup,
	req *coreprotocol.MapWGReq,
) *wavefront.WorkGroup {
	wg := wavefront.NewWorkGroup(raw, req)

	lds := make([]byte, req.WorkGroup.Packet.GroupSegmentSize)
	wg.LDS = lds

	for _, rawWf := range req.WorkGroup.Wavefronts {
		wf := wavefront.NewWavefront(rawWf)
		wg.Wfs = append(wg.Wfs, wf)
		wf.WG = wg
		wf.SetPID(req.PID)
	}

	return wg
}

func (cu *ComputeUnit) processInputFromInstMem(now akita.VTimeInSec) bool {
	rsp := cu.ToInstMem.Retrieve(now)
	if rsp == nil {
		return false
	}

	switch rsp := rsp.(type) {
	case *protocol.DataReadyRsp:
		cu.handleFetchReturn(now, rsp)
	default:
		log.Panicf("cannot handle request of type %s from ToInstMem port",
			reflect.TypeOf(rsp))
	}
	return true
}

func (cu *ComputeUnit) handleFetchReturn(
	now akita.VTimeInSec,
	rsp *protocol.DataReadyRsp,
) bool {
	if len(cu.InFlightInstFetch) == 0 {
		return false
	}

	info := cu.InFlightInstFetch[0]
	if info.Req.ID != rsp.RespondTo {
		return false
	}

	wf := info.Wavefront
	addr := info.Address
	cu.InFlightInstFetch = cu.InFlightInstFetch[1:]

	if addr == wf.InstBufferStartPC+uint64(len(wf.InstBuffer)) {
		wf.InstBuffer = append(wf.InstBuffer, rsp.Data...)
	}

	wf.IsFetching = false
	wf.LastFetchTime = now

	tracing.TraceReqFinalize(info.Req, now, cu)
	tracing.EndTask(tracing.FetchTaskID(info.Req.ID), now, cu)
	return true
}

func (cu *ComputeUnit) processInputFromScalarMem(now akita.VTimeInSec) bool {
	rsp := cu.ToScalarMem.Retrieve(now)
	if rsp == nil {
		return false
	}

	switch rsp := rsp.(type) {
	case *protocol.DataReadyRsp:
		cu.handleScalarDataLoadReturn(now, rsp)
	default:
		log.Panicf("cannot handle request of type %s from ToScalarMem port",
			reflect.TypeOf(rsp))
	}
	return true
}

func (cu *ComputeUnit) handleScalarDataLoadReturn(
	now akita.VTimeInSec,
	rsp *protocol.DataReadyRsp,
) {
	if len(cu.InFlightScalarMemAccess) == 0 {
		return
	}

	info := cu.InFlightScalarMemAccess[0]
	req := info.Req
	if req.ID != rsp.RespondTo {
		return
	}

	wf := info.Wavefront
	access := RegisterAccess{
		WaveOffset: wf.SRegOffset,
		Reg:        info.DstSGPR,
		RegCount:   len(rsp.Data) / 4,
		Data:       rsp.Data,
	}
	cu.SRegFile.Write(access)

	if oooDebugOn() {
		cu.recordSGPRLoadWrite(wf, info.DstSGPR, access.RegCount, rsp.Data, req.Address, now)
		cu.trackScalarAddrValue(wf, req.Address, rsp.Data, now)
	}

	cu.InFlightScalarMemAccess = cu.InFlightScalarMemAccess[1:]

	cu.logInstTask(now, wf, info.Inst, true)
	tracing.TraceReqFinalize(req, now, cu)

	if cu.isLastRead(req) {
		wf.OutstandingScalarMemAccess--
	}
}

func (cu *ComputeUnit) isLastRead(req *protocol.ReadReq) bool {
	return !req.CanWaitForCoalesce
}

func (cu *ComputeUnit) processInputFromVectorMem(now akita.VTimeInSec) bool {
	rsp := cu.ToVectorMem.Retrieve(now)
	if rsp == nil {
		return false
	}

	switch rsp := rsp.(type) {
	case *protocol.DataReadyRsp:
		cu.handleVectorDataLoadReturn(now, rsp)
	case *protocol.WriteDoneRsp:
		cu.handleVectorDataStoreRsp(now, rsp)
	default:
		log.Panicf("cannot handle request of type %s from ToVectorMem port",
			reflect.TypeOf(rsp))
	}

	return true
}

//nolint:gocyclo
func (cu *ComputeUnit) handleVectorDataLoadReturn(
	now akita.VTimeInSec,
	rsp *protocol.DataReadyRsp,
) {
	if len(cu.InFlightVectorMemAccess) == 0 {
		if oooDebugOn() {
			fmt.Printf("[ooo-dbg] LoadRsp DROPPED (empty InFlight): respondTo=%d\n", rsp.RespondTo)
		}
		return
	}

	// Out-of-order completion: the ROB returns responses in arbitrary order,
	// so match by request ID anywhere in the in-flight list (not just the FIFO
	// head). Transactions of one instruction write disjoint registers, so
	// out-of-order writeback is correct.
	idx := cu.findInFlightVMem(func(e VectorMemAccessInfo) bool {
		return e.Read != nil && e.Read.ID == rsp.RespondTo
	})
	if idx == -1 {
		if oooDebugOn() {
			// This response has no matching in-flight entry — print InFlight state.
			fmt.Printf("[ooo-dbg] LoadRsp UNMATCHED: respondTo=%d inFlightLen=%d\n",
				rsp.RespondTo, len(cu.InFlightVectorMemAccess))
			for i, e := range cu.InFlightVectorMemAccess {
				if e.Read != nil {
					fmt.Printf("[ooo-dbg]   inFlight[%d] READ  id=%d wf=%s PC=0x%x\n",
						i, e.Read.ID, e.Wavefront.UID, e.PC)
				} else if e.Write != nil {
					fmt.Printf("[ooo-dbg]   inFlight[%d] WRITE id=%d wf=%s PC=0x%x\n",
						i, e.Write.ID, e.Wavefront.UID, e.PC)
				}
			}
		}
		return
	}

	info := cu.InFlightVectorMemAccess[idx]
	inst := info.Inst
	wf := info.Wavefront

	if oooDebugOn() {
		fmt.Printf("[ooo-dbg] LoadRsp MATCHED: wf=%s PC=0x%x idx=%d inFlightLen=%d OVMA=%d addr=0x%x\n",
			wf.UID, info.PC, idx, len(cu.InFlightVectorMemAccess), wf.OutstandingVectorMemAccess, info.Read.Address)
		// Dump the first 32 bytes of the cache-line response so we can compare
		// it against the request address. If the data does NOT match what
		// memory at info.Read.Address should hold, the ROB/L1V is misrouting
		// data; if the data matches but a later FlatLoad still issues a
		// garbage address, the corruption is in a separate path.
		dumpLen := len(rsp.Data)
		if dumpLen > 32 {
			dumpLen = 32
		}
		fmt.Printf("[ooo-dbg] LoadRsp DATA : wf=%s PC=0x%x addr=0x%x rspID=%d len=%d data[0..%d]=%x\n",
			wf.UID, info.PC, info.Read.Address, rsp.RespondTo, len(rsp.Data), dumpLen, rsp.Data[:dumpLen])
		// Per-lane summary: print the first non-zero lane and lane0 to keep
		// output bounded. The "value" is the 4-byte uint32 the wavefront
		// would load into its destination VGPR for that lane.
		for _, li := range info.laneInfo {
			off := li.addrOffsetInCacheLine
			if off+4 <= uint64(len(rsp.Data)) {
				v := uint32(rsp.Data[off]) | uint32(rsp.Data[off+1])<<8 |
					uint32(rsp.Data[off+2])<<16 | uint32(rsp.Data[off+3])<<24
				fmt.Printf("[ooo-dbg]   lane%d off=0x%x val=0x%x\n", li.laneID, off, v)
				break
			}
		}
	}

	cu.InFlightVectorMemAccess = append(
		cu.InFlightVectorMemAccess[:idx],
		cu.InFlightVectorMemAccess[idx+1:]...)
	tracing.TraceReqFinalize(info.Read, now, cu)

	for _, laneInfo := range info.laneInfo {
		offset := laneInfo.addrOffsetInCacheLine
		access := RegisterAccess{}
		access.WaveOffset = wf.VRegOffset
		access.Reg = laneInfo.reg
		access.RegCount = laneInfo.regCount
		access.LaneID = laneInfo.laneID
		if inst.FormatType == insts.FLAT && inst.Opcode == 16 { // FLAT_LOAD_UBYTE
			access.Data = insts.Uint32ToBytes(uint32(rsp.Data[offset]))
		} else if inst.FormatType == insts.FLAT && inst.Opcode == 18 {
			access.Data = insts.Uint32ToBytes(uint32(rsp.Data[offset]))
		} else {
			end := offset + uint64(4*laneInfo.regCount)
			if end > uint64(len(rsp.Data)) {
				// Misaligned GCN3 access straddles a cache line boundary.
				// ISA requires natural alignment so this is undefined
				// behaviour; skip rather than panic.
				if oooDebugOn() {
					fmt.Printf("[ooo-dbg] LoadRsp MISALIGNED skip: wf=%s PC=0x%x lane=%d offset=%d end=%d rspLen=%d\n",
						wf.UID, info.PC, laneInfo.laneID, offset, end, len(rsp.Data))
				}
				continue
			}
			access.Data = rsp.Data[offset:end]
		}
		cu.VRegFile[wf.SIMDID].Write(access)
		if oooDebugOn() {
			cu.recordVGPRLoadWrite(wf, access, info.PC, now)
		}
	}

	// The instruction is complete only when ALL its transactions have
	// returned. With out-of-order completion we cannot rely on the
	// "last-issued" (CanWaitForCoalesce==false) transaction arriving last, so
	// detect completion by checking whether any sibling transaction (same
	// dynamic Inst) is still in flight.
	//
	// Completion does NOT immediately retire the vmcnt counter: GCN3
	// s_waitcnt assumes vector-memory ops retire in issue order, so a younger
	// load that finishes first must be held until all older loads retire.
	if cu.instFullyReturned(inst) {
		cu.retireVMemInsts(now, wf, inst)
	}

	// Stuck-inst recovery: a ROB / L1V response can be silently dropped at
	// lines 709 (InFlight empty) or 723 (no ID match) under sub-FIFO+OOO
	// interactions. If a previously-issued inst of the same WF has its head
	// position in vmemIssueOrder but vmemCompleted is false AND no remaining
	// transaction references it, the only path to retirement is closed. Force
	// the mark here so OVMA can drain and s_waitcnt makes progress instead of
	// the whole CU stalling. No-op when nothing is stuck — safe to call always.
	cu.reclaimStuckVMemInsts(now, wf)
}

// reclaimStuckVMemInsts walks the wavefront's vmemIssueOrder and force-marks
// any inst whose transactions are no longer in InFlight or shadowInFlight but
// whose vmemCompleted bit is unset. Pure recovery — when responses are not
// lost it does nothing.
func (cu *ComputeUnit) reclaimStuckVMemInsts(
	now akita.VTimeInSec,
	wf *wavefront.Wavefront,
) {
	order := wf.VMemIssueOrder()
	if len(order) == 0 {
		return
	}
	for _, queued := range order {
		if wf.VMemCompleted(queued) {
			continue
		}
		if !cu.instFullyReturned(queued) {
			continue
		}
		// Unconditional warning: reclaim only fires when a vmem response was
		// dropped (handleVectorDataLoadReturn/StoreRsp). It unblocks s_waitcnt
		// but does NOT recover load data — so a fired reclaim means results may
		// be silently wrong. Surface every occurrence (not gated by ooo-debug).
		log.Printf("[ooo-warn] vmem reclaim force-retired stuck inst: cu=%s wf=%s "+
			"OVMA=%d — a dropped response was masked (load data may be incomplete)",
			cu.Name(), wf.UID, wf.OutstandingVectorMemAccess)
		if oooDebugOn() {
			fmt.Printf("[ooo-dbg] STUCK-INST RECLAIM: wf=%s inst=%p OVMA=%d\n",
				wf.UID, queued, wf.OutstandingVectorMemAccess)
		}
		cu.retireVMemInsts(now, wf, queued)
	}
}

// retireVMemInsts marks inst as completed and decrements the vmcnt/lgkmcnt
// counters for every instruction that can now retire in issue order (see
// wavefront.CompleteVMemInst). This preserves the GCN3 in-order s_waitcnt
// counter contract on top of out-of-order memory completion.
func (cu *ComputeUnit) retireVMemInsts(
	now akita.VTimeInSec,
	wf *wavefront.Wavefront,
	inst *wavefront.Inst,
) {
	retired := wf.CompleteVMemInst(inst)
	for _, ri := range retired {
		wf.OutstandingVectorMemAccess--
		if ri.FormatType == insts.FLAT {
			wf.OutstandingScalarMemAccess--
		}
		cu.logInstTask(now, wf, ri, true)
	}
	if oooDebugOn() && len(retired) > 0 {
		fmt.Printf("[ooo-dbg] VMem RETIRE: wf=%s retired=%d OVMA_after=%d OSMA_after=%d\n",
			wf.UID, len(retired), wf.OutstandingVectorMemAccess, wf.OutstandingScalarMemAccess)
	}
}

// vgprKey identifies one VGPR slot for the load-write provenance tracker.
func vgprKey(simdID, waveOffset, lane, regIndex int) string {
	return fmt.Sprintf("%d/%d/%d/%d", simdID, waveOffset, lane, regIndex)
}

// recordVGPRLoadWrite remembers that a vector-memory load wrote one VGPR.
// Debug-only. access carries the destination reg/lane and the data just
// written; only the first dword is recorded for the report.
func (cu *ComputeUnit) recordVGPRLoadWrite(
	wf *wavefront.Wavefront,
	access RegisterAccess,
	pc uint64,
	now akita.VTimeInSec,
) {
	if cu.vgprLoadWrite == nil {
		cu.vgprLoadWrite = make(map[string]vgprWriteRec)
	}
	if len(access.Data) < 4 {
		return
	}
	val := insts.BytesToUint32(access.Data[0:4])
	key := vgprKey(wf.SIMDID, access.WaveOffset, access.LaneID, access.Reg.RegIndex())
	cu.vgprLoadWrite[key] = vgprWriteRec{pc: pc, val: val, time: now, wfUID: wf.UID}
}

// sgprKey identifies one SGPR slot for the scalar load-write tracker.
func sgprKey(sRegOffset, regIndex int) string {
	return fmt.Sprintf("%d/%d", sRegOffset, regIndex)
}

// recordSGPRLoadWrite remembers that a scalar load wrote one or more SGPRs.
// Debug-only. The "pc" field carries the loaded address (kernarg slot) for
// identification, and val is the per-register dword.
func (cu *ComputeUnit) recordSGPRLoadWrite(
	wf *wavefront.Wavefront,
	dst *insts.Reg,
	regCount int,
	data []byte,
	addr uint64,
	now akita.VTimeInSec,
) {
	if dst == nil {
		return
	}
	if cu.sgprLoadWrite == nil {
		cu.sgprLoadWrite = make(map[string]vgprWriteRec)
	}
	base := dst.RegIndex()
	for r := 0; r < regCount; r++ {
		off := r * 4
		if off+4 > len(data) {
			break
		}
		cu.sgprLoadWrite[sgprKey(wf.SRegOffset, base+r)] = vgprWriteRec{
			pc:    addr,
			val:   insts.BytesToUint32(data[off : off+4]),
			time:  now,
			wfUID: wf.UID,
		}
	}
}

// trackScalarAddrValue logs when a scalar load of an address returns a value
// different from what a previous scalar load of the SAME address returned.
// Debug-only. A valid→0 (or any) transition proves the cached/memory data for
// that address changed under the simulated timing (staleness/coherence), as
// opposed to the address being 0 from the very first read (setup-vs-read race).
func (cu *ComputeUnit) trackScalarAddrValue(
	wf *wavefront.Wavefront, addr uint64, data []byte, now akita.VTimeInSec,
) {
	if len(data) < 8 {
		return
	}
	v := uint64(insts.BytesToUint32(data[0:4])) |
		uint64(insts.BytesToUint32(data[4:8]))<<32
	if cu.scalarAddrLastVal == nil {
		cu.scalarAddrLastVal = make(map[uint64]uint64)
	}
	prev, seen := cu.scalarAddrLastVal[addr]
	if seen && prev != v {
		fmt.Printf("[ooo-scalar] VALUE CHANGED addr=0x%x: 0x%x -> 0x%x wf=%s t=%.6f\n",
			addr, prev, v, wf.UID, now)
	}
	cu.scalarAddrLastVal[addr] = v
}

// readVGPRWord reads the current dword of VGPR v[regIndex] for one lane.
func (cu *ComputeUnit) readVGPRWord(
	wf *wavefront.Wavefront, regIndex, lane int,
) uint32 {
	buf := make([]byte, 4)
	cu.VRegFile[wf.SIMDID].Read(RegisterAccess{
		Reg:        insts.VReg(regIndex),
		RegCount:   1,
		LaneID:     lane,
		WaveOffset: wf.VRegOffset,
		Data:       buf,
	})
	return insts.BytesToUint32(buf)
}

// dumpAddrVGPRProvenance explains why a FLAT instruction is about to issue an
// out-of-range address: for every active lane whose computed address is out of
// the sanity window it prints the address VGPR pair's current contents and the
// last vector-memory load that wrote each half (or "never"), plus the loads
// still in flight for this wavefront. Debug-only; called right before the
// checkIssueAddrSanity panic so the root cause is captured at the source.
func (cu *ComputeUnit) dumpAddrVGPRProvenance(
	now akita.VTimeInSec,
	wf *wavefront.Wavefront,
) {
	inst := wf.DynamicInst()
	if inst == nil || inst.Addr == nil || inst.Addr.Register == nil {
		return
	}
	loIdx := inst.Addr.Register.RegIndex()
	hiIdx := loIdx + 1

	fmt.Printf("[ooo-prov] FAILING INST wf=%s PC=0x%x: %s\n",
		wf.UID, wf.PC, inst.String(nil))

	sp := wf.Scratchpad().AsFlat()
	exec := sp.EXEC

	recStr := func(simd, waveOff, lane, reg int) string {
		if cu.vgprLoadWrite == nil {
			return "never-written-by-load"
		}
		rec, ok := cu.vgprLoadWrite[vgprKey(simd, waveOff, lane, reg)]
		if !ok {
			return "never-written-by-load"
		}
		return fmt.Sprintf("PC=0x%x val=0x%x t=%.6f wf=%s",
			rec.pc, rec.val, rec.time, rec.wfUID)
	}

	for lane := 0; lane < 64; lane++ {
		if !laneMasked(exec, uint(lane)) {
			continue
		}
		a := sp.ADDR[lane]
		if a >= oooVaSanityMin && a < oooVaSanityMax {
			continue
		}
		lo := cu.readVGPRWord(wf, loIdx, lane)
		hi := cu.readVGPRWord(wf, hiIdx, lane)
		fmt.Printf("[ooo-prov] wf=%s PC=0x%x lane=%d addr=0x%x  vAddrReg=v[%d:%d] lo=0x%x hi=0x%x\n",
			wf.UID, wf.PC, lane, a, loIdx, hiIdx, lo, hi)
		fmt.Printf("[ooo-prov]   v%d(lo) lastLoadWrite: %s\n",
			loIdx, recStr(wf.SIMDID, wf.VRegOffset, lane, loIdx))
		fmt.Printf("[ooo-prov]   v%d(hi) lastLoadWrite: %s\n",
			hiIdx, recStr(wf.SIMDID, wf.VRegOffset, lane, hiIdx))
	}

	// Outstanding vmem loads, grouped by dynamic instruction (collapse the
	// per-cache-line transactions of one gather into a single line + count).
	fmt.Printf("[ooo-prov]   wf=%s OVMA=%d outstanding vmem insts (grouped):\n",
		wf.UID, wf.OutstandingVectorMemAccess)
	type vmemGroup struct {
		pc    uint64
		kind  string
		count int
	}
	order := []*wavefront.Inst{}
	groups := map[*wavefront.Inst]*vmemGroup{}
	for i := range cu.InFlightVectorMemAccess {
		e := cu.InFlightVectorMemAccess[i]
		if e.Wavefront != wf {
			continue
		}
		g, ok := groups[e.Inst]
		if !ok {
			kind := "READ"
			if e.Write != nil {
				kind = "WRITE"
			}
			g = &vmemGroup{pc: e.PC, kind: kind}
			groups[e.Inst] = g
			order = append(order, e.Inst)
		}
		g.count++
	}
	for _, in := range order {
		g := groups[in]
		fmt.Printf("[ooo-prov]     inFlight %s PC=0x%x transactions=%d inst=%p\n",
			g.kind, g.pc, g.count, in)
	}

	// Scalar (lgkmcnt) side: a missing base pointer is most often a scalar
	// load that has not landed. Print the scalar counter and any in-flight
	// scalar loads with their destination SGPR so a not-yet-returned base
	// descriptor shows up directly.
	fmt.Printf("[ooo-prov]   wf=%s OSMA=%d outstanding scalar loads:\n",
		wf.UID, wf.OutstandingScalarMemAccess)
	for i := range cu.InFlightScalarMemAccess {
		e := cu.InFlightScalarMemAccess[i]
		if e.Wavefront != wf {
			continue
		}
		dst := -1
		if e.DstSGPR != nil {
			dst = e.DstSGPR.RegIndex()
		}
		addr := uint64(0)
		if e.Req != nil {
			addr = e.Req.Address
		}
		fmt.Printf("[ooo-prov]     inFlight SCALAR dstSGPR=s%d addr=0x%x inst=%p\n",
			dst, addr, e.Inst)
	}

	// Full SGPR dump for the failing wavefront: the FLAT base pointer is built
	// from SGPRs, so this shows whether a valid base (e.g. 0x...region matching
	// the in-flight loads) exists in the scalar file but was not applied, or
	// whether the base SGPRs are themselves 0/garbage.
	if wf.CodeObject != nil {
		buf := make([]byte, 4)
		n := int(wf.CodeObject.WFSgprCount)
		fmt.Printf("[ooo-prov]   wf=%s SGPRs (count=%d):\n", wf.UID, n)
		for i := 0; i < n; i++ {
			cu.SRegFile.Read(RegisterAccess{
				Reg:        insts.SReg(i),
				RegCount:   1,
				WaveOffset: wf.SRegOffset,
				Data:       buf,
			})
			prov := "never-written-by-load"
			if cu.sgprLoadWrite != nil {
				if rec, ok := cu.sgprLoadWrite[sgprKey(wf.SRegOffset, i)]; ok {
					prov = fmt.Sprintf("lastScalarLoad: kernargAddr=0x%x val=0x%x t=%.6f",
						rec.pc, rec.val, rec.time)
				}
			}
			fmt.Printf("[ooo-prov]     s%d=0x%08x  %s\n", i, insts.BytesToUint32(buf), prov)
		}
	}
}

// findInFlightVMem returns the index of the first in-flight vector-memory
// transaction matching pred, or -1.
func (cu *ComputeUnit) findInFlightVMem(
	pred func(VectorMemAccessInfo) bool,
) int {
	for i := range cu.InFlightVectorMemAccess {
		if pred(cu.InFlightVectorMemAccess[i]) {
			return i
		}
	}
	return -1
}

// instFullyReturned reports whether no transaction of the given dynamic
// instruction remains in flight (i.e. the just-removed one was the last).
// During flush/restart cycles some transactions may still be in the shadow
// buffer waiting to be re-sent, so those are checked as well.
func (cu *ComputeUnit) instFullyReturned(inst *wavefront.Inst) bool {
	for i := range cu.InFlightVectorMemAccess {
		if cu.InFlightVectorMemAccess[i].Inst == inst {
			return false
		}
	}
	for i := range cu.shadowInFlightVectorMemAccess {
		if cu.shadowInFlightVectorMemAccess[i].Inst == inst {
			return false
		}
	}
	return true
}

func (cu *ComputeUnit) handleVectorDataStoreRsp(
	now akita.VTimeInSec,
	rsp *protocol.WriteDoneRsp,
) {
	if len(cu.InFlightVectorMemAccess) == 0 {
		return
	}

	// Out-of-order completion (see handleVectorDataLoadReturn): match the
	// store response by request ID anywhere in the in-flight list.
	idx := cu.findInFlightVMem(func(e VectorMemAccessInfo) bool {
		return e.Write != nil && e.Write.ID == rsp.RespondTo
	})
	if idx == -1 {
		if oooDebugOn() {
			fmt.Printf("[ooo-dbg] StoreRsp UNMATCHED: respondTo=%d inFlightLen=%d\n",
				rsp.RespondTo, len(cu.InFlightVectorMemAccess))
		}
		return
	}

	info := cu.InFlightVectorMemAccess[idx]
	inst := info.Inst
	wf := info.Wavefront

	if oooDebugOn() {
		fmt.Printf("[ooo-dbg] StoreRsp MATCHED: wf=%s PC=0x%x idx=%d inFlightLen=%d OVMA=%d addr=0x%x\n",
			wf.UID, info.PC, idx, len(cu.InFlightVectorMemAccess), wf.OutstandingVectorMemAccess, info.Write.Address)
	}

	cu.InFlightVectorMemAccess = append(
		cu.InFlightVectorMemAccess[:idx],
		cu.InFlightVectorMemAccess[idx+1:]...)
	tracing.TraceReqFinalize(info.Write, now, cu)

	if cu.instFullyReturned(inst) {
		cu.retireVMemInsts(now, wf, inst)
	}

	// See handleVectorDataLoadReturn for the rationale.
	cu.reclaimStuckVMemInsts(now, wf)
}

// UpdatePCAndSetReady is self explained
func (cu *ComputeUnit) UpdatePCAndSetReady(wf *wavefront.Wavefront) {
	wf.State = wavefront.WfReady
	wf.PC += uint64(wf.Inst().ByteSize)
	cu.removeStaleInstBuffer(wf)
}

func (cu *ComputeUnit) removeStaleInstBuffer(wf *wavefront.Wavefront) {
	if len(wf.InstBuffer) != 0 {
		for wf.PC >= wf.InstBufferStartPC+64 {
			wf.InstBuffer = wf.InstBuffer[64:]
			wf.InstBufferStartPC += 64
		}
	}
}

func (cu *ComputeUnit) flushCUBuffers() {
	cu.InFlightInstFetch = nil
	cu.InFlightScalarMemAccess = nil
	cu.InFlightVectorMemAccess = nil
}

func (cu *ComputeUnit) logInstTask(
	now akita.VTimeInSec,
	wf *wavefront.Wavefront,
	inst *wavefront.Inst,
	completed bool,
) {
	if completed {
		tracing.EndTask(
			inst.ID,
			now,
			cu,
		)
		return
	}

	tracing.StartTask(
		inst.ID,
		tracing.DecimalTaskID(wf.UID),
		now,
		cu,
		"inst",
		cu.execUnitToString(inst.ExeUnit),
		// inst.InstName,
		map[string]interface{}{
			"inst": inst,
			"wf":   wf,
		},
	)
}

func (cu *ComputeUnit) execUnitToString(u insts.ExeUnit) string {
	switch u {
	case insts.ExeUnitVALU:
		return "VALU"
	case insts.ExeUnitScalar:
		return "Scalar"
	case insts.ExeUnitVMem:
		return "VMem"
	case insts.ExeUnitBranch:
		return "Branch"
	case insts.ExeUnitLDS:
		return "LDS"
	case insts.ExeUnitGDS:
		return "GDS"
	case insts.ExeUnitSpecial:
		return "Special"
	}
	panic("unknown exec unit")
}

func (cu *ComputeUnit) reInsertShadowBufferReqsToOriginalBuffers() bool {
	cu.isSendingOutShadowBufferReqs = false
	for i := 0; i < len(cu.shadowInFlightVectorMemAccess); i++ {
		cu.InFlightVectorMemAccess = append(cu.InFlightVectorMemAccess, cu.shadowInFlightVectorMemAccess[i])
	}

	for i := 0; i < len(cu.shadowInFlightScalarMemAccess); i++ {
		cu.InFlightScalarMemAccess = append(cu.InFlightScalarMemAccess, cu.shadowInFlightScalarMemAccess[i])
	}

	for i := 0; i < len(cu.shadowInFlightInstFetch); i++ {
		cu.InFlightInstFetch = append(cu.InFlightInstFetch, cu.shadowInFlightInstFetch[i])
	}

	return true
}

func (cu *ComputeUnit) checkShadowBuffers(now akita.VTimeInSec) bool {
	numReqsPendingToSend :=
		len(cu.shadowInFlightScalarMemAccess) +
			len(cu.shadowInFlightVectorMemAccess) +
			len(cu.shadowInFlightInstFetch)

	if numReqsPendingToSend == 0 {
		cu.isSendingOutShadowBufferReqs = false
		cu.Scheduler.Resume()
		cu.isPaused = false
		// return true
		return false
	}

	return cu.sendOutShadowBufferReqs(now)
}

func (cu *ComputeUnit) sendOutShadowBufferReqs(now akita.VTimeInSec) bool {
	madeProgress := false

	madeProgress = cu.sendScalarShadowBufferAccesses(now) || madeProgress
	madeProgress = cu.sendVectorShadowBufferAccesses(now) || madeProgress
	madeProgress = cu.sendInstFetchShadowBufferAccesses(now) || madeProgress

	return madeProgress
}

func (cu *ComputeUnit) sendScalarShadowBufferAccesses(
	now akita.VTimeInSec,
) bool {
	if len(cu.shadowInFlightScalarMemAccess) > 0 {
		info := cu.shadowInFlightScalarMemAccess[0]

		req := info.Req
		req.ID = akita.GetIDGenerator().Generate()
		req.SendTime = now
		err := cu.ToScalarMem.Send(req)
		if err == nil {
			cu.InFlightScalarMemAccess =
				append(cu.InFlightScalarMemAccess, info)
			cu.shadowInFlightScalarMemAccess =
				cu.shadowInFlightScalarMemAccess[1:]
			return true
		}
	}

	return false
}

func (cu *ComputeUnit) sendVectorShadowBufferAccesses(
	now akita.VTimeInSec,
) bool {
	if len(cu.shadowInFlightVectorMemAccess) > 0 {
		info := cu.shadowInFlightVectorMemAccess[0]
		if info.Read != nil {
			req := info.Read
			req.ID = akita.GetIDGenerator().Generate()
			req.SendTime = now
			err := cu.ToVectorMem.Send(req)
			if err == nil {
				cu.InFlightVectorMemAccess = append(
					cu.InFlightVectorMemAccess, info)
				cu.shadowInFlightVectorMemAccess = cu.shadowInFlightVectorMemAccess[1:]
				return true
			}
		} else if info.Write != nil {
			req := info.Write
			req.ID = akita.GetIDGenerator().Generate()
			req.SendTime = now
			err := cu.ToVectorMem.Send(req)
			if err == nil {
				cu.InFlightVectorMemAccess = append(cu.InFlightVectorMemAccess, info)
				cu.shadowInFlightVectorMemAccess = cu.shadowInFlightVectorMemAccess[1:]
				return true
			}
		}
	}
	return false
}

func (cu *ComputeUnit) sendInstFetchShadowBufferAccesses(
	now akita.VTimeInSec,
) bool {
	if len(cu.shadowInFlightInstFetch) > 0 {
		info := cu.shadowInFlightInstFetch[0]
		req := info.Req
		req.ID = akita.GetIDGenerator().Generate()
		req.SendTime = now
		err := cu.ToInstMem.Send(req)
		if err == nil {
			cu.InFlightInstFetch = append(cu.InFlightInstFetch, info)
			cu.shadowInFlightInstFetch = cu.shadowInFlightInstFetch[1:]
			return true
		}
	}
	return false
}
func (cu *ComputeUnit) populateShadowBuffers() {
	for i := 0; i < len(cu.InFlightInstFetch); i++ {
		cu.shadowInFlightInstFetch = append(cu.shadowInFlightInstFetch, cu.InFlightInstFetch[i])
	}

	for i := 0; i < len(cu.InFlightScalarMemAccess); i++ {
		cu.shadowInFlightScalarMemAccess = append(cu.shadowInFlightScalarMemAccess, cu.InFlightScalarMemAccess[i])
	}

	for i := 0; i < len(cu.InFlightVectorMemAccess); i++ {
		cu.shadowInFlightVectorMemAccess = append(cu.shadowInFlightVectorMemAccess, cu.InFlightVectorMemAccess[i])
	}

	cu.InFlightScalarMemAccess = nil
	cu.InFlightInstFetch = nil
	cu.InFlightVectorMemAccess = nil
}

func (cu *ComputeUnit) setWavesToReady() {
	for _, wfPool := range cu.WfPools {
		for _, wf := range wfPool.wfs {
			if wf.State != wavefront.WfCompleted {
				wf.State = wavefront.WfReady
				wf.IsFetching = false
			}
		}
	}
}

// NewComputeUnit returns a newly constructed compute unit
func NewComputeUnit(
	name string,
	engine akita.Engine,
) *ComputeUnit {
	cu := new(ComputeUnit)
	cu.TickingComponent = akita.NewTickingComponent(
		name, engine, 1*akita.GHz, cu)

	cu.ToACE = akita.NewLimitNumMsgPort(cu, 4, name+".ToACE")
	cu.toACESender = akitaext.NewBufferedSender(
		cu.ToACE, util.NewBuffer(40960000))
	cu.ToInstMem = akita.NewLimitNumMsgPort(cu, 4, name+".ToInstMem")
	cu.ToScalarMem = akita.NewLimitNumMsgPort(cu, 4, name+".ToScalarMem")
	cu.ToVectorMem = akita.NewLimitNumMsgPort(cu, 4, name+".ToVectorMem")
	cu.ToCP = akita.NewLimitNumMsgPort(cu, 4, name+".ToCP")

	return cu
}
