// Package sac implements the runtime controller of Sharing-Aware Caching
// (SAC, ISCA'23). Per kernel, the controller profiles the first
// `profileWindow` cycles under the memory-side LLC configuration, feeds the
// collected counters into the EAB (Effective Available Bandwidth) analytical
// model, and reconfigures all L2 caches to the SM-side configuration when
// the model predicts a bandwidth win larger than theta.
package sac

import (
	"fmt"
	"math"

	"gitlab.com/akita/akita"
	"gitlab.com/akita/mem/cache"
	"gitlab.com/akita/util"
	"gitlab.com/akita/util/akitaext"
	"gitlab.com/akita/util/global"
)

type ctrlState int

const (
	// no reconfiguration in flight
	ctrlIdle ctrlState = iota
	// profiling window open, decision event scheduled
	ctrlProfiling
	// mid-kernel memory-side -> SM-side flush in flight
	ctrlFlushingToSMSide
	// kernel-end writeback flush in flight (revert to memory-side)
	ctrlReverting
)

// CRDResetter is implemented by writebacksac.Cache.
type CRDResetter interface {
	ResetCRD()
}

// KernelDecision records the outcome of the EAB model for one kernel.
type KernelDecision struct {
	Kernel    int
	Mode      global.SACLLCMode
	EABMem    float64
	EABSM     float64
	RLocal    float64
	LSUMem    float64
	LSUSM     float64
	HitMem    float64
	HitSM     float64
	TotalReqs uint64
}

// Controller drives the per-kernel SAC reconfiguration. It implements
// cp.KernelObserver and driver.KernelCompletionObserver.
type Controller struct {
	*akita.TickingComponent

	ControlPort   akita.Port
	controlSender akitaext.BufferedSender

	l2ControlPorts []akita.Port
	crdResetters   []CRDResetter

	// configuration
	profileWindow int     // cycles
	theta         float64 // SM-side must win by this fraction
	forced        string  // "", "memory", "sm"
	// EAB model bandwidth constants, per chiplet, in GB/s. Only their
	// ratios matter for the memory-side vs SM-side comparison.
	BIntra float64
	BInter float64
	BLLC   float64
	BMem   float64

	state           ctrlState
	gen             int // invalidates stale decision events
	kernelRunning   bool
	pendingFlushRsp int
	kernelCount     int
	decisions       []KernelDecision
}

type decisionEvent struct {
	*akita.EventBase
	gen int
}

// Tick processes flush responses and drains the control sender.
func (c *Controller) Tick(now akita.VTimeInSec) bool {
	madeProgress := c.controlSender.Tick(now)
	madeProgress = c.processRsp(now) || madeProgress
	return madeProgress
}

func (c *Controller) processRsp(now akita.VTimeInSec) bool {
	msg := c.ControlPort.Peek()
	if msg == nil {
		return false
	}

	switch msg.(type) {
	case *cache.FlushRsp:
		c.ControlPort.Retrieve(now)
		c.pendingFlushRsp--
		if c.pendingFlushRsp == 0 {
			c.finishFlush(now)
		}
		return true
	default:
		panic("SAC controller: unknown message on control port")
	}
}

// Handle dispatches the custom decision event; everything else goes to the
// ticking component.
func (c *Controller) Handle(e akita.Event) error {
	switch evt := e.(type) {
	case *decisionEvent:
		c.handleDecision(evt)
		return nil
	default:
		return c.TickingComponent.Handle(e)
	}
}

// ReadyForKernel implements cp.KernelObserver. The CP delays the next
// kernel dispatch while a reconfiguration flush is in flight.
func (c *Controller) ReadyForKernel() bool {
	return c.state == ctrlIdle
}

// OnKernelStart implements cp.KernelObserver.
func (c *Controller) OnKernelStart(now akita.VTimeInSec) {
	s := global.GetInstance().SAC()
	c.kernelCount++
	c.gen++
	c.kernelRunning = true

	switch c.forced {
	case "memory":
		s.Mode = global.SACMemorySide
		return
	case "sm":
		if s.Mode != global.SACSMSide {
			// First switch: write back any dirty lines installed under
			// memory-side semantics (e.g. by H2D copies) before inbound
			// requests start bypassing the home L2.
			c.startFlush(now, ctrlFlushingToSMSide)
		}
		return
	}

	// Dynamic: profile under the memory-side configuration.
	s.Mode = global.SACMemorySide
	s.ResetCounters()
	for _, r := range c.crdResetters {
		r.ResetCRD()
	}
	s.Profiling = true
	c.state = ctrlProfiling

	evt := &decisionEvent{
		EventBase: akita.NewEventBase(
			c.Freq.NCyclesLater(c.profileWindow, now), c),
		gen: c.gen,
	}
	c.Engine.Schedule(evt)
}

// OnKernelComplete implements driver.KernelCompletionObserver. When the
// kernel ran (or ends) in SM-side mode, the dirty (possibly remote) lines
// are written back and the LLC reverts to memory-side before the next
// kernel may start.
func (c *Controller) OnKernelComplete(now akita.VTimeInSec) {
	s := global.GetInstance().SAC()
	c.gen++
	c.kernelRunning = false

	if c.state == ctrlProfiling {
		// Kernel was shorter than the profiling window.
		s.Profiling = false
		c.state = ctrlIdle
	}

	if c.state == ctrlIdle && s.Mode == global.SACSMSide {
		c.startFlush(now, ctrlReverting)
	}
	// If a mid-kernel flush is still in flight (ctrlFlushingToSMSide),
	// finishFlush observes kernelRunning=false and skips the mode switch.
}

func (c *Controller) startFlush(now akita.VTimeInSec, st ctrlState) {
	c.state = st
	c.pendingFlushRsp = len(c.l2ControlPorts)
	for _, p := range c.l2ControlPorts {
		req := cache.FlushReqBuilder{}.
			WithSendTime(now).
			WithSrc(c.ControlPort).
			WithDst(p).
			InvalidateAllCacheLines().
			Build()
		c.controlSender.Send(req)
	}
	c.TickLater(now)
}

func (c *Controller) finishFlush(now akita.VTimeInSec) {
	s := global.GetInstance().SAC()

	switch c.state {
	case ctrlFlushingToSMSide:
		if c.kernelRunning {
			s.Mode = global.SACSMSide
			if global.GetInstance().GetVerbose() {
				fmt.Printf("[SAC] kernel %d: LLC reconfigured to %v at %.9f\n",
					c.kernelCount, s.Mode, float64(now))
			}
		}
		// If the kernel already ended, stay memory-side; nothing to revert.
	case ctrlReverting:
		if c.forced != "sm" {
			s.Mode = global.SACMemorySide
		}
	}
	c.state = ctrlIdle
}

func (c *Controller) handleDecision(evt *decisionEvent) {
	if evt.gen != c.gen || c.state != ctrlProfiling {
		return
	}
	now := evt.Time()

	s := global.GetInstance().SAC()
	s.Profiling = false

	d := c.computeEAB(s)
	d.Kernel = c.kernelCount
	useSMSide := d.EABSM > (1+c.theta)*d.EABMem
	if useSMSide {
		d.Mode = global.SACSMSide
	} else {
		d.Mode = global.SACMemorySide
	}
	c.decisions = append(c.decisions, d)
	allDecisions = append(allDecisions, d)

	fmt.Printf("[SAC] kernel %d decision: %v "+
		"(EAB mem=%.1f sm=%.1f GB/s, Rlocal=%.3f, LSU mem=%.3f sm=%.3f, "+
		"hit mem=%.3f sm=%.3f, reqs=%d)\n",
		d.Kernel, d.Mode, d.EABMem, d.EABSM, d.RLocal,
		d.LSUMem, d.LSUSM, d.HitMem, d.HitSM, d.TotalReqs)

	if useSMSide {
		c.startFlush(now, ctrlFlushingToSMSide)
	} else {
		c.state = ctrlIdle
	}
}

// computeEAB evaluates the EAB model (SAC ISCA'23 §3.3, Table 1) for both
// LLC configurations using the counters collected during the profiling
// window.
func (c *Controller) computeEAB(s *global.SACState) KernelDecision {
	rL := 1.0
	if s.TotalReqs > 0 {
		rL = float64(s.LocalReqs) / float64(s.TotalReqs)
	}
	rR := 1 - rL

	hitMem := 0.0
	if s.LookupsMem > 0 {
		hitMem = float64(s.HitsMem) / float64(s.LookupsMem)
	}
	hitSM := hitMem
	if s.CRDReqs > 0 {
		hitSM = float64(s.CRDHits) / float64(s.CRDReqs)
	}

	lsuMem := lsu(s.SliceReqsMem)
	lsuSM := lsu(s.SliceReqsSM)

	inf := math.Inf(1)
	eabSide := func(bSMLLC, bLLCHit, bLLCMiss, bLLCMem, bMem float64) float64 {
		return math.Min(bSMLLC, bLLCHit+math.Min(bLLCMiss, math.Min(bLLCMem, bMem)))
	}

	// Memory-side: local requests ride the intra-chip network, remote
	// requests the inter-chip links; LLC misses always access the local
	// memory partition of the home chiplet (unbounded link).
	memLocal := eabSide(
		c.BIntra,
		c.BLLC*lsuMem*hitMem*rL,
		c.BLLC*lsuMem*(1-hitMem)*rL,
		inf,
		c.BMem*rL)
	memRemote := eabSide(
		c.BInter,
		c.BLLC*lsuMem*hitMem*rR,
		c.BLLC*lsuMem*(1-hitMem)*rR,
		inf,
		c.BMem*rR)

	// SM-side: the intra-chip network is shared by local and remote
	// requests; misses to remote data must cross the inter-chip links.
	smLocal := eabSide(
		c.BIntra*rL,
		c.BLLC*lsuSM*hitSM*rL,
		c.BLLC*lsuSM*(1-hitSM)*rL,
		inf,
		c.BMem*rL)
	smRemote := eabSide(
		c.BIntra*rR,
		c.BLLC*lsuSM*hitSM*rR,
		c.BLLC*lsuSM*(1-hitSM)*rR,
		c.BInter,
		c.BMem*rR)

	return KernelDecision{
		EABMem:    memLocal + memRemote,
		EABSM:     smLocal + smRemote,
		RLocal:    rL,
		LSUMem:    lsuMem,
		LSUSM:     lsuSM,
		HitMem:    hitMem,
		HitSM:     hitSM,
		TotalReqs: s.TotalReqs,
	}
}

// lsu computes the LLC Slice Uniformity: 1 for a uniform request
// distribution across slices, 1/N when all requests hit a single slice.
func lsu(reqs []uint64) float64 {
	if len(reqs) == 0 {
		return 1
	}
	max := uint64(0)
	for _, r := range reqs {
		if r > max {
			max = r
		}
	}
	if max == 0 {
		return 1
	}
	sum := 0.0
	for _, r := range reqs {
		sum += float64(r) / float64(max)
	}
	return sum / float64(len(reqs))
}

// Decisions returns the per-kernel decisions made so far (for metric
// reporting).
func (c *Controller) Decisions() []KernelDecision {
	return c.decisions
}

// allDecisions aggregates decisions across all controllers in the process
// (there is one per simulation) so the runner can report them without a
// handle to the controller.
var allDecisions []KernelDecision

// AllDecisions returns every decision recorded in this process.
func AllDecisions() []KernelDecision {
	return allDecisions
}

// Builder builds a SAC controller.
type Builder struct {
	engine        akita.Engine
	freq          akita.Freq
	profileWindow int
	theta         float64
	forced        string
	bIntra        float64
	bInter        float64
	bLLC          float64
	bMem          float64
}

// MakeBuilder returns a Builder with paper-default parameters.
func MakeBuilder() Builder {
	return Builder{
		freq:          1 * akita.GHz,
		profileWindow: 2000,
		theta:         0.05,
		bIntra:        1024,
		bInter:        256,
		bLLC:          1024,
		bMem:          1024,
	}
}

func (b Builder) WithEngine(e akita.Engine) Builder {
	b.engine = e
	return b
}

func (b Builder) WithFreq(f akita.Freq) Builder {
	b.freq = f
	return b
}

// WithProfileWindow sets the profiling window in cycles.
func (b Builder) WithProfileWindow(n int) Builder {
	b.profileWindow = n
	return b
}

// WithTheta sets the threshold by which the SM-side EAB must exceed the
// memory-side EAB to trigger reconfiguration.
func (b Builder) WithTheta(t float64) Builder {
	b.theta = t
	return b
}

// WithForcedMode forces "memory" or "sm" for every kernel (no profiling);
// "" or "dynamic" enables the EAB-driven decision.
func (b Builder) WithForcedMode(m string) Builder {
	if m == "dynamic" {
		m = ""
	}
	b.forced = m
	return b
}

// WithBandwidths overrides the EAB model constants (per chiplet, GB/s):
// intra-chip network, inter-chip links, raw LLC, raw memory.
func (b Builder) WithBandwidths(intra, inter, llc, mem float64) Builder {
	if intra > 0 {
		b.bIntra = intra
	}
	if inter > 0 {
		b.bInter = inter
	}
	if llc > 0 {
		b.bLLC = llc
	}
	if mem > 0 {
		b.bMem = mem
	}
	return b
}

// Build creates the controller. L2 control ports and CRD resetters are
// registered afterwards via RegisterL2.
func (b Builder) Build(name string) *Controller {
	c := &Controller{
		profileWindow: b.profileWindow,
		theta:         b.theta,
		forced:        b.forced,
		BIntra:        b.bIntra,
		BInter:        b.bInter,
		BLLC:          b.bLLC,
		BMem:          b.bMem,
	}
	c.TickingComponent = akita.NewTickingComponent(name, b.engine, b.freq, c)
	c.ControlPort = akita.NewLimitNumMsgPort(c, 16, name+".ControlPort")
	return c
}

// RegisterL2 registers one L2 cache's control port (flush target) and its
// CRD resetter. The control sender must be created after all L2s are
// registered, via FinalizePorts.
func (c *Controller) RegisterL2(controlPort akita.Port, crd CRDResetter) {
	c.l2ControlPorts = append(c.l2ControlPorts, controlPort)
	c.crdResetters = append(c.crdResetters, crd)
}

// FinalizePorts sizes the buffered control sender to hold a full broadcast
// of flush requests to every registered L2.
func (c *Controller) FinalizePorts() {
	c.controlSender = akitaext.NewBufferedSender(
		c.ControlPort, util.NewBuffer(len(c.l2ControlPorts)+16))
}
