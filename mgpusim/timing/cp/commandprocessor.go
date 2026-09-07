package cp

import (
	"fmt"

	"gitlab.com/akita/akita"
	"gitlab.com/akita/mem/cache"
	"gitlab.com/akita/mem/idealmemcontroller"
	"gitlab.com/akita/mem/vm/tlb"
	"gitlab.com/akita/mgpusim/coreprotocol"
	"gitlab.com/akita/mgpusim/pagemigrationcontroller"
	"gitlab.com/akita/mgpusim/rdma"
	"gitlab.com/akita/mgpusim/timing/cp/internal/dispatching"
	"gitlab.com/akita/mgpusim/timing/cp/internal/resource"
	"gitlab.com/akita/protocol"
	"gitlab.com/akita/util/akitaext"
	"gitlab.com/akita/util/tracing"

	// "stats"
	// "github.com/montanaflynn/stats"
	"strconv"
)

// CommandProcessor is an Akita component that is responsible for receiving
// requests from the driver and dispatch the requests to other parts of the
// GPU.
type CommandProcessor struct {
	*akita.TickingComponent

	Dispatchers        []dispatching.Dispatcher
	DMAEngine          akita.Port
	Driver             akita.Port
	TLBs               []akita.Port
	MMUs               []akita.Port
	CUs                []akita.Port
	AddressTranslators []akita.Port
	RDMA               akita.Port
	PMC                akita.Port
	L1VCaches          []akita.Port
	L1SCaches          []akita.Port
	L1ICaches          []akita.Port
	L2Caches           []akita.Port
	DRAMControllers    []*idealmemcontroller.Comp
	RTUs               []akita.Port

	ToDriver                   akita.Port
	toDriverSender             akitaext.BufferedSender
	ToDMA                      akita.Port
	toDMASender                akitaext.BufferedSender
	ToCUs                      akita.Port
	toCUsSender                akitaext.BufferedSender
	ToTLBs                     akita.Port
	toTLBsSender               akitaext.BufferedSender
	ToMMUs                     akita.Port
	toMMUsSender               akitaext.BufferedSender
	ToAddressTranslators       akita.Port
	toAddressTranslatorsSender akitaext.BufferedSender
	ToCaches                   akita.Port
	toCachesSender             akitaext.BufferedSender
	ToRDMA                     akita.Port
	toRDMASender               akitaext.BufferedSender
	ToRTU                      akita.Port
	toRTUSender                akitaext.BufferedSender
	ToPMC                      akita.Port
	toPMCSender                akitaext.BufferedSender

	currShootdownRequest *coreprotocol.ShootDownCommand
	currFlushRequest     *coreprotocol.FlushCommand

	numTLBs                      uint64
	numCUAck                     uint64
	numAddrTranslationFlushAck   uint64
	numAddrTranslationRestartAck uint64
	numTLBAck                    uint64
	numCacheACK                  uint64

	shootDownInProcess bool

	bottomKernelLaunchReqIDToTopReqMap map[uint64]*coreprotocol.LaunchKernelReq
	bottomMemCopyH2DReqIDToTopReqMap   map[uint64]*coreprotocol.MemCopyH2DReq
	bottomMemCopyD2HReqIDToTopReqMap   map[uint64]*coreprotocol.MemCopyD2HReq

	// SAC platform only — nil by default (see sac_observer.go).
	kernelObserver KernelObserver

	customHSLpmdUnits uint64

	// simplified
	ChipRdmaEngines []rdma.RDMAEngine
}

// CUInterfaceForCP defines the interface that a CP requires from CU.
type CUInterfaceForCP interface {
	resource.DispatchableCU

	// ControlPort returns a port on the CU that the CP can send controlling
	// messages to.
	ControlPort() akita.Port
}

// RegisterCU allows the Command Processor to control the CU.
func (p *CommandProcessor) RegisterCU(cu CUInterfaceForCP) {
	p.CUs = append(p.CUs, cu.ControlPort())
	for _, d := range p.Dispatchers {
		d.RegisterCU(cu)
	}
}

// Tick ticks
// DeadlockSummary reports per-port pending flags on every CP port.
func (p *CommandProcessor) DeadlockSummary() (line string, anyPending bool) {
	check := func(port akita.Port) bool {
		return port != nil && port.Peek() != nil
	}
	driverQ := check(p.ToDriver)
	dmaQ := check(p.ToDMA)
	cusQ := check(p.ToCUs)
	tlbsQ := check(p.ToTLBs)
	mmusQ := check(p.ToMMUs)
	atsQ := check(p.ToAddressTranslators)
	cachesQ := check(p.ToCaches)
	rdmaQ := check(p.ToRDMA)
	rtuQ := check(p.ToRTU)
	pmcQ := check(p.ToPMC)
	anyPending = driverQ || dmaQ || cusQ || tlbsQ || mmusQ || atsQ ||
		cachesQ || rdmaQ || rtuQ || pmcQ
	line = fmt.Sprintf(
		"driver=%v dma=%v cus=%v tlbs=%v mmus=%v ats=%v caches=%v rdma=%v rtu=%v pmc=%v",
		driverQ, dmaQ, cusQ, tlbsQ, mmusQ, atsQ, cachesQ, rdmaQ, rtuQ, pmcQ)
	return
}

func (p *CommandProcessor) Tick(now akita.VTimeInSec) bool {
	madeProgress := false

	madeProgress = p.sendMsgsOut(now) || madeProgress
	madeProgress = p.tickDispatchers(now) || madeProgress
	madeProgress = p.processReqFromDriver(now) || madeProgress
	madeProgress = p.processRspFromInternal(now) || madeProgress

	return madeProgress
}

func (p *CommandProcessor) sendMsgsOut(now akita.VTimeInSec) bool {
	madeProgress := false

	madeProgress = p.sendMsgsOutFromPort(now, p.toDriverSender) || madeProgress
	madeProgress = p.sendMsgsOutFromPort(now, p.toDMASender) || madeProgress
	madeProgress = p.sendMsgsOutFromPort(now, p.toCUsSender) || madeProgress
	madeProgress = p.sendMsgsOutFromPort(now, p.toTLBsSender) || madeProgress
	madeProgress = p.sendMsgsOutFromPort(now, p.toMMUsSender) || madeProgress
	madeProgress = p.sendMsgsOutFromPort(now, p.toAddressTranslatorsSender) || madeProgress
	madeProgress = p.sendMsgsOutFromPort(now, p.toCachesSender) || madeProgress
	madeProgress = p.sendMsgsOutFromPort(now, p.toRDMASender) || madeProgress
	madeProgress = p.sendMsgsOutFromPort(now, p.toRTUSender) || madeProgress
	madeProgress = p.sendMsgsOutFromPort(now, p.toPMCSender) || madeProgress

	return madeProgress
}

func (p *CommandProcessor) sendMsgsOutFromPort(
	now akita.VTimeInSec,
	sender akitaext.BufferedSender,
) (madeProgress bool) {
	for {
		ok := sender.Tick(now)
		if ok {
			madeProgress = true
		} else {
			return madeProgress
		}
	}
}

func (p *CommandProcessor) tickDispatchers(
	now akita.VTimeInSec,
) (madeProgress bool) {
	for _, d := range p.Dispatchers {
		for i := 0; i < 32; i++ {
			madeProgress = d.Tick(now) || madeProgress
		}
	}

	return madeProgress
}

func (p *CommandProcessor) processReqFromDriver(now akita.VTimeInSec) bool {
	msg := p.ToDriver.Peek()
	if msg == nil {
		return false
	}

	switch req := msg.(type) {
	case *coreprotocol.LaunchKernelReq:
		return p.processLaunchKernelReq(now, req)
	case *coreprotocol.FlushCommand:
		return p.processFlushCommand(now, req)
	case *coreprotocol.MemCopyD2HReq, *coreprotocol.MemCopyH2DReq:
		if p.kernelObserver != nil && !p.kernelObserver.ReadyForKernel() {
			// SAC: delay memcopies while an LLC writeback flush is in
			// flight, so D2H never reads stale DRAM.
			return false
		}
		return p.processMemCopyReq(now, req)
	case *coreprotocol.RDMADrainCmdFromDriver:
		return p.processRDMADrainCmd(now, req)
	case *coreprotocol.RDMARestartCmdFromDriver:
		return p.processRDMARestartCommand(now, req)
	case *coreprotocol.ShootDownCommand:
		return p.processShootdownCommand(now, req)
	case *coreprotocol.GPURestartReq:
		return p.processGPURestartReq(now, req)
	case *coreprotocol.PageMigrationReqToCP:
		return p.processPageMigrationReq(now, req)
	}

	panic("never")
}

func (p *CommandProcessor) processRspFromInternal(now akita.VTimeInSec) bool {
	madeProgress := false

	madeProgress = p.processRspFromDMAs(now) || madeProgress
	madeProgress = p.processRspFromRDMAs(now) || madeProgress
	madeProgress = p.processRspFromCUs(now) || madeProgress
	madeProgress = p.processRspFromATs(now) || madeProgress
	madeProgress = p.processRspFromCaches(now) || madeProgress
	madeProgress = p.processRspFromTLBs(now) || madeProgress
	madeProgress = p.processRspFromPMC(now) || madeProgress

	return madeProgress
}

func (p *CommandProcessor) processRspFromDMAs(now akita.VTimeInSec) bool {
	msg := p.ToDMA.Peek()
	if msg == nil {
		return false
	}

	switch req := msg.(type) {
	case *coreprotocol.MemCopyD2HReq, *coreprotocol.MemCopyH2DReq:
		return p.processMemCopyRsp(now, req)
	}

	panic("never")
}

func (p *CommandProcessor) processRspFromRDMAs(now akita.VTimeInSec) bool {
	msg := p.ToRDMA.Peek()
	if msg == nil {
		return false
	}

	switch req := msg.(type) {
	case *rdma.DrainRsp:
		return p.processRDMADrainRsp(now, req)
	case *rdma.RestartRsp:
		return p.processRDMARestartRsp(now, req)
	}

	panic("never")
}

func (p *CommandProcessor) processRspFromCUs(now akita.VTimeInSec) bool {
	msg := p.ToCUs.Peek()
	if msg == nil {
		return false
	}

	switch req := msg.(type) {
	case *coreprotocol.CUPipelineFlushRsp:
		return p.processCUPipelineFlushRsp(now, req)
	case *coreprotocol.CUPipelineRestartRsp:
		return p.processCUPipelineRestartRsp(now, req)
	}

	return false
}

func (p *CommandProcessor) processRspFromCaches(now akita.VTimeInSec) bool {
	msg := p.ToCaches.Peek()
	if msg == nil {
		return false
	}

	switch req := msg.(type) {
	case *cache.FlushRsp:
		if p.numCacheACK == 1 {
			fmt.Println("Processing cache flush rsp with numCacheACK =", p.numCacheACK)
		}
		return p.processCacheFlushRsp(now, req)
	case *cache.RestartRsp:
		return p.processCacheRestartRsp(now, req)
	}

	panic("never")
}

func (p *CommandProcessor) processRspFromATs(now akita.VTimeInSec) bool {
	item := p.ToAddressTranslators.Peek()
	if item == nil {
		return false
	}

	msg := item.(*protocol.ControlMsg)

	if p.numAddrTranslationFlushAck > 0 {
		return p.processAddressTranslatorFlushRsp(now, msg)
	} else if p.numAddrTranslationRestartAck > 0 {
		return p.processAddressTranslatorRestartRsp(now, msg)
	}

	panic("never")
}

func (p *CommandProcessor) processRspFromTLBs(now akita.VTimeInSec) bool {
	msg := p.ToTLBs.Peek()
	if msg == nil {
		return false
	}

	switch req := msg.(type) {
	case *tlb.TLBFlushRsp:
		return p.processTLBFlushRsp(now, req)
	case *tlb.TLBRestartRsp:
		return p.processTLBRestartRsp(now, req)
	}

	panic("never")
}

func (p *CommandProcessor) processRspFromPMC(now akita.VTimeInSec) bool {
	msg := p.ToPMC.Peek()
	if msg == nil {
		return false
	}

	switch req := msg.(type) {
	case *pagemigrationcontroller.PageMigrationRspFromPMC:
		return p.processPageMigrationRsp(now, req)
	}

	panic("never")
}

func (p *CommandProcessor) processLaunchKernelReq(
	now akita.VTimeInSec,
	req *coreprotocol.LaunchKernelReq,
) bool {
	if p.kernelObserver != nil && !p.kernelObserver.ReadyForKernel() {
		// SAC LLC reconfiguration in flight: leave the request queued and
		// retry once the observer reports ready.
		return false
	}

	d := p.findAvailableDispatcher()

	if d == nil {
		return false
	}

	d.StartDispatching(req)
	p.ToDriver.Retrieve(now)

	if p.kernelObserver != nil {
		p.kernelObserver.OnKernelStart(now)
	}

	tracing.TraceReqReceive(req, now, p)
	// tracing.TraceReqInitiate(&reqToBottom, now, p,
	// 	tracing.MsgIDAtReceiver(req, p))

	return true
}

func (p *CommandProcessor) findAvailableDispatcher() dispatching.Dispatcher {
	for _, d := range p.Dispatchers {
		if !d.IsDispatching() {
			return d
		}
	}

	return nil
}
func (p *CommandProcessor) processRDMADrainCmd(
	now akita.VTimeInSec,
	cmd *coreprotocol.RDMADrainCmdFromDriver,
) bool {
	req := rdma.DrainReqBuilder{}.
		WithSendTime(now).
		WithSrc(p.ToRDMA).
		WithDst(p.RDMA).
		Build()

	p.toRDMASender.Send(req)
	p.ToDriver.Retrieve(now)

	return true
}

func (p *CommandProcessor) processRDMADrainRsp(
	now akita.VTimeInSec,
	rsp *rdma.DrainRsp,
) bool {
	req := coreprotocol.NewRDMADrainRspToDriver(now, p.ToDriver, p.Driver)

	p.toDriverSender.Send(req)
	p.ToRDMA.Retrieve(now)

	return true
}

func (p *CommandProcessor) processShootdownCommand(
	now akita.VTimeInSec,
	cmd *coreprotocol.ShootDownCommand,
) bool {
	if p.shootDownInProcess == true {
		return false
	}

	p.currShootdownRequest = cmd
	p.shootDownInProcess = true

	for i := 0; i < len(p.CUs); i++ {
		p.numCUAck++
		req := coreprotocol.CUPipelineFlushReqBuilder{}.
			WithSendTime(now).
			WithSrc(p.ToCUs).
			WithDst(p.CUs[i]).
			Build()
		p.toCUsSender.Send(req)
	}

	p.ToDriver.Retrieve(now)

	return true
}

func (p *CommandProcessor) processCUPipelineFlushRsp(
	now akita.VTimeInSec,
	rsp *coreprotocol.CUPipelineFlushRsp,
) bool {
	p.numCUAck--

	if p.numCUAck == 0 {
		for i := 0; i < len(p.AddressTranslators); i++ {
			req := protocol.ControlMsgBuilder{}.
				WithSendTime(now).
				WithSrc(p.ToAddressTranslators).
				WithDst(p.AddressTranslators[i]).
				ToDiscardTransactions().
				Build()
			p.toAddressTranslatorsSender.Send(req)
			p.numAddrTranslationFlushAck++
		}
	}

	p.ToCUs.Retrieve(now)

	return true
}

func (p *CommandProcessor) processAddressTranslatorFlushRsp(
	now akita.VTimeInSec,
	msg *protocol.ControlMsg,
) bool {
	p.numAddrTranslationFlushAck--

	if p.numAddrTranslationFlushAck == 0 {
		for _, port := range p.L1SCaches {
			p.flushAndResetL1Cache(now, port)
		}

		for _, port := range p.L1VCaches {
			p.flushAndResetL1Cache(now, port)
		}

		for _, port := range p.L1ICaches {
			p.flushAndResetL1Cache(now, port)
		}

		for _, port := range p.L2Caches {
			p.flushAndResetL2Cache(now, port)
		}
	}

	p.ToAddressTranslators.Retrieve(now)

	return true
}

func (p *CommandProcessor) flushAndResetL1Cache(
	now akita.VTimeInSec,
	port akita.Port,
) {
	req := cache.FlushReqBuilder{}.
		WithSendTime(now).
		WithSrc(p.ToCaches).
		WithDst(port).
		PauseAfterFlushing().
		DiscardInflight().
		InvalidateAllCacheLines().
		Build()

	p.toCachesSender.Send(req)
	p.numCacheACK++
}

func (p *CommandProcessor) flushAndResetL2Cache(now akita.VTimeInSec, port akita.Port) {
	req := cache.FlushReqBuilder{}.
		WithSendTime(now).
		WithSrc(p.ToCaches).
		WithDst(port).
		PauseAfterFlushing().
		DiscardInflight().
		InvalidateAllCacheLines().
		Build()

	p.toCachesSender.Send(req)
	p.numCacheACK++
}

func (p *CommandProcessor) processCacheFlushRsp(
	now akita.VTimeInSec,
	rsp *cache.FlushRsp,
) bool {
	p.numCacheACK--
	if p.numCacheACK == 0 {
		if p.shootDownInProcess {
			return p.processCacheFlushCausedByTLBShootdown(now, rsp)
		}
		return p.processRegularCacheFlush(now, rsp)
	}

	p.ToCaches.Retrieve(now)
	return true
}

func (p *CommandProcessor) processRegularCacheFlush(
	now akita.VTimeInSec,
	flushRsp *cache.FlushRsp,
) bool {
	// Caches fully flushed = RDMA drained: safe to reset kernel-specific
	// PC-table state. Only the dynamic-clustering engine implements
	// Reset(); other engines have no PC table and are skipped.
	for _, e := range p.ChipRdmaEngines {
		if r, ok := e.(interface{ Reset() }); ok {
			r.Reset()
		}
	}

	p.currFlushRequest.Src, p.currFlushRequest.Dst =
		p.currFlushRequest.Dst, p.currFlushRequest.Src
	p.currFlushRequest.SendTime = now

	p.toDriverSender.Send(p.currFlushRequest)

	p.ToCaches.Retrieve(now)

	return true
}

func (p *CommandProcessor) processCacheFlushCausedByTLBShootdown(
	now akita.VTimeInSec,
	flushRsp *cache.FlushRsp,
) bool {
	for i := 0; i < len(p.TLBs); i++ {
		shootDownCmd := p.currShootdownRequest
		req := tlb.TLBFlushReqBuilder{}.
			WithSendTime(now).
			WithSrc(p.ToTLBs).
			WithDst(p.TLBs[i]).
			WithPID(shootDownCmd.PID).
			WithVAddrs(shootDownCmd.VAddr).
			Build()

		p.toTLBsSender.Send(req)
		p.numTLBAck++
	}

	p.ToCaches.Retrieve(now)
	return true
}

func (p *CommandProcessor) processTLBFlushRsp(
	now akita.VTimeInSec,
	rsp *tlb.TLBFlushRsp,
) bool {
	p.numTLBAck--

	if p.numTLBAck == 0 {
		req := coreprotocol.NewShootdownCompleteRsp(now, p.ToDriver, p.Driver)
		p.toDriverSender.Send(req)

		p.shootDownInProcess = false
	}

	p.ToTLBs.Retrieve(now)

	return true
}

func (p *CommandProcessor) processRDMARestartCommand(
	now akita.VTimeInSec,
	cmd *coreprotocol.RDMARestartCmdFromDriver,
) bool {
	req := rdma.RestartReqBuilder{}.
		WithSrc(p.ToRDMA).
		WithDst(p.RDMA).
		WithSendTime(now).
		Build()

	p.toRDMASender.Send(req)

	p.ToDriver.Retrieve(now)

	return true
}

func (p *CommandProcessor) processRDMARestartRsp(now akita.VTimeInSec, rsp *rdma.RestartRsp) bool {
	req := coreprotocol.NewRDMARestartRspToDriver(now, p.ToDriver, p.Driver)
	p.toDriverSender.Send(req)
	p.ToRDMA.Retrieve(now)

	return true
}

func (p *CommandProcessor) processGPURestartReq(
	now akita.VTimeInSec,
	cmd *coreprotocol.GPURestartReq,
) bool {
	for _, port := range p.L2Caches {
		p.restartCache(now, port)
	}
	for _, port := range p.L1ICaches {
		p.restartCache(now, port)
	}
	for _, port := range p.L1SCaches {
		p.restartCache(now, port)
	}

	for _, port := range p.L1VCaches {
		p.restartCache(now, port)
	}

	p.ToDriver.Retrieve(now)

	return true
}

func (p *CommandProcessor) restartCache(now akita.VTimeInSec, port akita.Port) {
	req := cache.RestartReqBuilder{}.
		WithSendTime(now).
		WithSrc(p.ToCaches).
		WithDst(port).
		Build()

	p.toCachesSender.Send(req)

	p.numCacheACK++
}

func (p *CommandProcessor) processCacheRestartRsp(
	now akita.VTimeInSec,
	rsp *cache.RestartRsp,
) bool {
	p.numCacheACK--
	if p.numCacheACK == 0 {
		for i := 0; i < len(p.TLBs); i++ {
			p.numTLBAck++

			req := tlb.TLBRestartReqBuilder{}.
				WithSendTime(now).
				WithSrc(p.ToTLBs).
				WithDst(p.TLBs[i]).
				Build()
			p.toTLBsSender.Send(req)
		}
	}

	p.ToCaches.Retrieve(now)

	return true
}

func (p *CommandProcessor) processTLBRestartRsp(
	now akita.VTimeInSec,
	rsp *tlb.TLBRestartRsp,
) bool {
	p.numTLBAck--

	if p.numTLBAck == 0 {
		for i := 0; i < len(p.AddressTranslators); i++ {
			req := protocol.ControlMsgBuilder{}.
				WithSendTime(now).
				WithSrc(p.ToAddressTranslators).
				WithDst(p.AddressTranslators[i]).
				ToRestart().
				Build()
			p.toAddressTranslatorsSender.Send(req)

			// fmt.Printf("Restarting %s\n", p.AddressTranslators[i].Name())

			p.numAddrTranslationRestartAck++
		}
	}

	p.ToTLBs.Retrieve(now)

	return true
}

func (p *CommandProcessor) processAddressTranslatorRestartRsp(
	now akita.VTimeInSec,
	rsp *protocol.ControlMsg,
) bool {
	p.numAddrTranslationRestartAck--

	if p.numAddrTranslationRestartAck == 0 {
		for i := 0; i < len(p.CUs); i++ {
			req := coreprotocol.CUPipelineRestartReqBuilder{}.
				WithSendTime(now).
				WithSrc(p.ToCUs).
				WithDst(p.CUs[i]).
				Build()
			p.toCUsSender.Send(req)

			p.numCUAck++
		}
	}

	p.ToAddressTranslators.Retrieve(now)

	return true
}

func (p *CommandProcessor) processCUPipelineRestartRsp(
	now akita.VTimeInSec,
	rsp *coreprotocol.CUPipelineRestartRsp,
) bool {
	p.numCUAck--

	if p.numCUAck == 0 {
		rsp := coreprotocol.NewGPURestartRsp(now, p.ToDriver, p.Driver)
		p.toDriverSender.Send(rsp)
	}

	p.ToCUs.Retrieve(now)

	return true
}

func (p *CommandProcessor) processPageMigrationReq(
	now akita.VTimeInSec,
	cmd *coreprotocol.PageMigrationReqToCP,
) bool {
	req := pagemigrationcontroller.PageMigrationReqToPMCBuilder{}.
		WithSendTime(now).
		WithSrc(p.ToPMC).
		WithDst(p.PMC).
		WithPageSize(cmd.PageSize).
		WithPMCPortOfRemoteGPU(cmd.DestinationPMCPort).
		WithReadFrom(cmd.ToReadFromPhysicalAddress).
		WithWriteTo(cmd.ToWriteToPhysicalAddress).
		Build()
	p.toPMCSender.Send(req)

	p.ToDriver.Retrieve(now)

	return true
}

func (p *CommandProcessor) processPageMigrationRsp(
	now akita.VTimeInSec,
	rsp *pagemigrationcontroller.PageMigrationRspFromPMC,
) bool {
	req := coreprotocol.NewPageMigrationRspToDriver(now, p.ToDriver, p.Driver)

	p.toDriverSender.Send(req)

	p.ToPMC.Retrieve(now)

	return true
}

func (p *CommandProcessor) processFlushCommand(
	now akita.VTimeInSec,
	cmd *coreprotocol.FlushCommand,
) bool {
	if p.numCacheACK > 0 {
		return false
	}

	for _, port := range p.L1ICaches {
		p.flushCache(now, port)
	}

	for _, port := range p.L1SCaches {
		p.flushCache(now, port)
	}

	for _, port := range p.L1VCaches {
		p.flushCache(now, port)
	}

	for _, port := range p.L2Caches {
		p.flushCache(now, port)
	}

	// PC-table reset is done in processRegularCacheFlush, after all caches
	// ACK (RDMA drained) — never here mid-flush, which would drop in-flight
	// writeback transactions.

	p.currFlushRequest = cmd
	if p.numCacheACK == 0 {
		p.currFlushRequest.Src, p.currFlushRequest.Dst =
			p.currFlushRequest.Dst, p.currFlushRequest.Src
		p.currFlushRequest.SendTime = now
		p.toDriverSender.Send(p.currFlushRequest)
	}

	p.ToDriver.Retrieve(now)

	return true
}

func (p *CommandProcessor) flushCache(now akita.VTimeInSec, port akita.Port) {
	flushReq := cache.FlushReqBuilder{}.
		WithSendTime(now).
		WithSrc(p.ToCaches).
		WithDst(port).
		Build()
	p.toCachesSender.Send(flushReq)
	p.numCacheACK++
}

func (p *CommandProcessor) cloneMemCopyH2DReq(
	req *coreprotocol.MemCopyH2DReq,
) *coreprotocol.MemCopyH2DReq {
	cloned := *req
	cloned.ID = akita.GetIDGenerator().Generate()
	p.bottomMemCopyH2DReqIDToTopReqMap[cloned.ID] = req
	return &cloned
}

func (p *CommandProcessor) cloneMemCopyD2HReq(
	req *coreprotocol.MemCopyD2HReq,
) *coreprotocol.MemCopyD2HReq {
	cloned := *req
	cloned.ID = akita.GetIDGenerator().Generate()
	p.bottomMemCopyD2HReqIDToTopReqMap[cloned.ID] = req
	return &cloned
}

func (p *CommandProcessor) processMemCopyReq(
	now akita.VTimeInSec,
	req akita.Msg,
) bool {
	var cloned akita.Msg
	switch req := req.(type) {
	case *coreprotocol.MemCopyH2DReq:
		cloned = p.cloneMemCopyH2DReq(req)
	case *coreprotocol.MemCopyD2HReq:
		cloned = p.cloneMemCopyD2HReq(req)
	default:
		panic("unknown type")
	}

	cloned.Meta().Dst = p.DMAEngine
	cloned.Meta().Src = p.ToDMA
	cloned.Meta().SendTime = now

	p.toDMASender.Send(cloned)
	p.ToDriver.Retrieve(now)

	tracing.TraceReqReceive(req, now, p)
	tracing.TraceReqInitiate(cloned, now, p, tracing.MsgIDAtReceiver(req, p))

	return true
}

func (p *CommandProcessor) findAndRemoveOriginalMemCopyRequest(
	rsp akita.Msg,
) akita.Msg {
	switch rsp := rsp.(type) {
	case *coreprotocol.MemCopyH2DReq:
		origionalReq := p.bottomMemCopyH2DReqIDToTopReqMap[rsp.ID]
		delete(p.bottomMemCopyH2DReqIDToTopReqMap, rsp.ID)
		return origionalReq
	case *coreprotocol.MemCopyD2HReq:
		originalReq := p.bottomMemCopyD2HReqIDToTopReqMap[rsp.ID]
		delete(p.bottomMemCopyD2HReqIDToTopReqMap, rsp.ID)
		return originalReq
	default:
		panic("unknown type")
	}
}

func (p *CommandProcessor) processMemCopyRsp(
	now akita.VTimeInSec,
	req akita.Msg,
) bool {
	originalReq := p.findAndRemoveOriginalMemCopyRequest(req)
	originalReq.Meta().Dst = p.Driver
	originalReq.Meta().Src = p.ToDriver
	originalReq.Meta().SendTime = now
	p.toDriverSender.Send(originalReq)
	p.ToDMA.Retrieve(now)

	tracing.TraceReqComplete(originalReq, now, p)
	tracing.TraceReqFinalize(req, now, p)

	return true
}

// DispatchProgressPerKernel merges per-kernel work-group progress across all
// dispatchers, keyed by global kernel sequence number. Each value is
// {completed, total}. Independent of the progress bar.
func (p *CommandProcessor) DispatchProgressPerKernel() map[int][2]int {
	out := make(map[int][2]int)
	for _, d := range p.Dispatchers {
		di, ok := d.(*dispatching.DispatcherImpl)
		if !ok {
			continue
		}
		for seq, ct := range di.PerKernelProgress() {
			v := out[seq]
			v[0] += ct[0]
			v[1] += ct[1]
			out[seq] = v
		}
	}
	return out
}

func (p *CommandProcessor) IsKernelCompleted() bool {
	for _, dispatcher := range p.Dispatchers {
		if dispatcher, ok := dispatcher.(*dispatching.DispatcherImpl); ok {
			if !dispatcher.KernelCompleted() {
				return false
			}
		}
	}

	return true
}

func getChipletNum(component string) int {
	// fmt.Println(component, component[14:15])
	chiplet, _ := strconv.Atoi(component[14:15])
	return chiplet
}
