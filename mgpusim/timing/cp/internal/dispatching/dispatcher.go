package dispatching

import (
	"fmt"

	"github.com/vbauerster/mpb/v4"
	"github.com/vbauerster/mpb/v4/decor"
	"gitlab.com/akita/akita"
	"gitlab.com/akita/mgpusim/coreprotocol"
	"gitlab.com/akita/mgpusim/kernels"
	"gitlab.com/akita/mgpusim/timing/cp/internal/resource"
	"gitlab.com/akita/util/tracing"
)

var barGroup *mpb.Progress

// nextKernelSeq hands out a process-wide, monotonically increasing kernel
// sequence number so per-kernel progress is ordered globally across all
// dispatchers. Serial engine => no atomic needed.
var nextKernelSeq int

func init() {
	barGroup = mpb.New()
}

// A Dispatcher is a sub-component of a command processor that can dispatch
// work-groups to compute units.
type Dispatcher interface {
	tracing.NamedHookable
	RegisterCU(cu resource.DispatchableCU)
	IsDispatching() bool
	StartDispatching(req *coreprotocol.LaunchKernelReq)
	Tick(now akita.VTimeInSec) (madeProgress bool)
}

// A DispatcherImpl is a ticking component that can dispatch work-groups.
type DispatcherImpl struct {
	akita.HookableBase

	cp                     tracing.NamedHookable
	name                   string
	respondingPort         akita.Port
	dispatchingPort        akita.Port
	alg                    algorithm
	dispatching            *coreprotocol.LaunchKernelReq
	currWG                 dispatchLocation
	cycleLeft              int
	numDispatchedWGs       int
	numCompletedWGs        int
	inflightWGs            map[uint64]dispatchLocation
	originalReqs           map[uint64]*coreprotocol.MapWGReq
	latencyTable           []int
	constantKernelOverhead int

	// Persistent per-kernel progress (survives kernel resets), keyed by global
	// kernel sequence number. Independent of the progress bar.
	currentKernelSeq   int
	completedPerKernel map[int]int
	totalPerKernel     map[int]int

	showProgressBar bool
	progressBar     *mpb.Bar
}

// Name returns the name of the dispatcher
func (d *DispatcherImpl) Name() string {
	return d.name
}

// DeadlockSummary reports dispatch state, in-flight WG count, and per-port
// pending flags.
func (d *DispatcherImpl) DeadlockSummary() (line string, anyPending bool) {
	dispatchingActive := d.dispatching != nil
	respQ := d.respondingPort != nil && d.respondingPort.Peek() != nil
	dispQ := d.dispatchingPort != nil && d.dispatchingPort.Peek() != nil
	inflight := len(d.inflightWGs)
	totalWG := 0
	if d.alg != nil {
		totalWG = d.alg.NumWG()
	}
	anyPending = dispatchingActive || respQ || dispQ ||
		inflight > 0 || d.numDispatchedWGs != d.numCompletedWGs
	line = fmt.Sprintf(
		"dispatching=%v resp=%v disp=%v inflightWG=%d dispatched=%d completed=%d totalWG=%d cycleLeft=%d",
		dispatchingActive, respQ, dispQ, inflight,
		d.numDispatchedWGs, d.numCompletedWGs, totalWG, d.cycleLeft)
	return
}

// PerKernelProgress returns, keyed by global kernel sequence number, the
// {completed, total} work-group counts accumulated over the whole run. Unlike
// the per-tick counters this persists across kernels. Independent of the
// progress bar.
func (d *DispatcherImpl) PerKernelProgress() map[int][2]int {
	out := make(map[int][2]int, len(d.totalPerKernel))
	for seq, total := range d.totalPerKernel {
		out[seq] = [2]int{d.completedPerKernel[seq], total}
	}
	return out
}

// RegisterCU allows the dispatcher to dispatch work-groups to the CU.
func (d *DispatcherImpl) RegisterCU(cu resource.DispatchableCU) {
	d.alg.RegisterCU(cu)
}

// IsDispatching checks if the dispatcher is dispatching another kernel.
func (d *DispatcherImpl) IsDispatching() bool {
	return d.dispatching != nil
}

// StartDispatching lets the dispatcher to start dispatch another kernel.
func (d *DispatcherImpl) StartDispatching(req *coreprotocol.LaunchKernelReq) {
	d.mustNotBeDispatchingAnotherKernel()

	d.alg.StartNewKernel(kernels.KernelLaunchInfo{
		CodeObject: req.HsaCo,
		Packet:     req.Packet,
		PacketAddr: req.PacketAddress,
		WGFilter:   req.WGFilter,
	})
	d.dispatching = req

	d.numDispatchedWGs = 0
	d.numCompletedWGs = 0

	if d.totalPerKernel == nil {
		d.totalPerKernel = make(map[int]int)
		d.completedPerKernel = make(map[int]int)
	}
	d.currentKernelSeq = nextKernelSeq
	nextKernelSeq++
	d.totalPerKernel[d.currentKernelSeq] = d.alg.NumWG()

	if d.showProgressBar {
		d.initializeProgressBar(req.ID)
	}
}

func (d *DispatcherImpl) initializeProgressBar(kernelID uint64) {
	if !d.showProgressBar {
		return
	}

	d.progressBar = barGroup.AddBar(
		int64(d.alg.NumWG()),
		mpb.PrependDecorators(
			decor.Name(fmt.Sprintf("At %s, Kernel: %d, ", d.Name(), kernelID)),
			decor.Counters(0, "%d/%d"),
		),
		mpb.AppendDecorators(
			decor.Percentage(),
			decor.AverageSpeed(0, " %.2f/s, "),
			decor.AverageETA(decor.ET_STYLE_HHMMSS),
		),
	)

	d.progressBar.SetTotal(int64(d.alg.NumWG()), false)
}

func (d *DispatcherImpl) mustNotBeDispatchingAnotherKernel() {
	if d.IsDispatching() {
		panic("dispatcher is dispatching another request")
	}
}

// Tick updates the state of the dispatcher.
func (d *DispatcherImpl) Tick(now akita.VTimeInSec) (madeProgress bool) {
	if d.cycleLeft > 0 {
		d.cycleLeft--
		return true
	}

	if d.dispatching != nil {
		if d.KernelCompleted() {
			fmt.Println("Completing kernel, ",
				d.name,
				d.numCompletedWGs, "/", d.numDispatchedWGs)
			madeProgress = d.completeKernel(now) || madeProgress
		} else {
			madeProgress = d.dispatchNextWG(now) || madeProgress
		}
	}

	madeProgress = d.processMessagesFromCU(now) || madeProgress

	return madeProgress
}

func (d *DispatcherImpl) processMessagesFromCU(now akita.VTimeInSec) bool {
	msg := d.dispatchingPort.Peek()
	if msg == nil {
		return false
	}

	switch msg := msg.(type) {
	case *coreprotocol.WGCompletionMsg:
		location, ok := d.inflightWGs[msg.RspTo]
		if !ok {
			return false
		}

		d.alg.FreeResources(location)
		delete(d.inflightWGs, msg.RspTo)
		d.numCompletedWGs++
		d.completedPerKernel[d.currentKernelSeq]++
		if d.numCompletedWGs == d.alg.NumWG() {
			d.cycleLeft = d.constantKernelOverhead
		}

		d.dispatchingPort.Retrieve(now)

		originalReq := d.originalReqs[msg.RspTo]
		delete(d.originalReqs, msg.RspTo)
		tracing.TraceReqFinalize(originalReq, now, d)

		if d.showProgressBar {
			d.progressBar.Increment()
		}

		return true
		// default:
		// panic("unknown msg type")
	}

	return false
}

func (d *DispatcherImpl) KernelCompleted() bool {
	if d.currWG.valid {
		return false
	}

	if d.alg.HasNext() {
		return false
	}

	if d.numCompletedWGs < d.numDispatchedWGs {
		return false
	}

	return true
}

func (d *DispatcherImpl) completeKernel(now akita.VTimeInSec) (
	madeProgress bool,
) {
	req := d.dispatching
	req.Src, req.Dst = req.Dst, req.Src
	req.SendTime = now
	err := d.respondingPort.Send(req)

	if err == nil {
		d.dispatching = nil

		tracing.TraceReqComplete(req, now, d.cp)

		return true
	}

	req.Src, req.Dst = req.Dst, req.Src
	return false
}

func (d *DispatcherImpl) dispatchNextWG(
	now akita.VTimeInSec,
) (madeProgress bool) {
	if !d.currWG.valid {
		if !d.alg.HasNext() {
			return false
		}

		d.currWG = d.alg.Next()
		if !d.currWG.valid {
			return false
		}
	}

	reqBuilder := coreprotocol.MapWGReqBuilder{}.
		WithSrc(d.dispatchingPort).
		WithDst(d.currWG.cu).
		WithSendTime(now).
		WithPID(d.dispatching.PID).
		WithWG(d.currWG.wg)
	for _, l := range d.currWG.locations {
		reqBuilder = reqBuilder.AddWf(l)
	}
	req := reqBuilder.Build()
	err := d.dispatchingPort.Send(req)

	if err == nil {
		d.currWG.valid = false
		d.numDispatchedWGs++
		d.inflightWGs[req.ID] = d.currWG
		d.originalReqs[req.ID] = req
		d.cycleLeft = d.latencyTable[len(d.currWG.locations)]

		tracing.TraceReqInitiate(req, now, d,
			tracing.MsgIDAtReceiver(d.dispatching, d.cp))
		return true
	}

	return false
}
