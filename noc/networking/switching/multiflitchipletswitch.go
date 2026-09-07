package switching

import (
	"fmt"
	"strconv"
	"strings"

	"gitlab.com/akita/akita"
	"gitlab.com/akita/noc"
	"gitlab.com/akita/noc/networking/arbitration"
	"gitlab.com/akita/noc/networking/routing"
	"gitlab.com/akita/protocol"
	"gitlab.com/akita/util"
	"gitlab.com/akita/util/global"
	"gitlab.com/akita/util/tracing"
)

/*
 *
 * MultiFlitChipletSwitchImpl
 * -
 */

// MultiFlitChipletSwitch is an Akita component that can forward request to destination.
type MultiFlitChipletSwitchImpl struct {
	*akita.TickingComponent

	ports                []akita.Port
	portToComplexMapping map[akita.Port]*multiFlitPortComplex
	routingTable         routing.Table
	arbiter              *arbitration.FlitAwareXBarArbiter

	flitByteSize     int
	encodingOverhead float64
	switchLatency    int
	numReqPerCycle   int

	// vctBufSize is the per-output VOQ depth. Under VCT store-and-forward it
	// must exceed the largest packet's flit count so a whole packet can be
	// buffered at one hop before forwarding (see voqFor / the arbiter gate).
	vctBufSize int

	row        int // wafer size
	col        int // wafer size
	y          int // switch position
	x          int // switch position
	nextPortId int

	ToRDMAReqPort akita.Port
}

func (s *MultiFlitChipletSwitchImpl) SortPorts() {
	rspPorts := make([]akita.Port, 0, len(s.ports))
	reqPorts := make([]akita.Port, 0, len(s.ports))

	for _, port := range s.ports {
		if strings.Contains(port.Name(), "Rsp") {
			rspPorts = append(rspPorts, port)
		} else {
			reqPorts = append(reqPorts, port)
		}
	}

	s.ports = append(rspPorts, reqPorts...)
}

/*
- localPort: port from switch
- remotePort: port from another component
*/
func (s *MultiFlitChipletSwitchImpl) addPort(
	pc *multiFlitPortComplex,
) {
	s.ports = append(s.ports, pc.localPort)
	s.portToComplexMapping[pc.localPort] = pc
	s.portToComplexMapping[pc.remotePort] = pc
	// VOQ replaces the single forwardBuffer: the per-output queues are
	// created and registered with the arbiter lazily in route(), so the
	// monolithic forwardBuffer is no longer registered here.
	pc.forwardVOQ = make(map[akita.Port]util.Buffer)

	// in the mesh network, this function will do nothing.
	s.routingTable.DefineRoute(pc.remotePort, pc.localPort)
}

// Tick advances the switch one cycle. The four stages are called in
// reverse data-flow order so each stage frees its downstream buffer
// before the upstream stage tries to push, letting a flit traverse the
// switch in a single cycle (recv → route → forward → tryDeliver) when
// buffers are not saturated. The msg<->flit conversion work lives in the
// EndPoints, so the switch is a pure flit router:
//
//	port → routeBuffer → forwardBuffer → sendOutBuffer → wire
//	     (recv)   (route)   [arbiter](forward)  (tryDeliver)
func (s *MultiFlitChipletSwitchImpl) Tick(now akita.VTimeInSec) bool {
	s.arbiter.Reset()
	s.nextPortId = 0

	madeProgress := false
	madeProgress = s.tryDeliver(now) || madeProgress
	madeProgress = s.forward(now) || madeProgress
	madeProgress = s.route(now) || madeProgress
	madeProgress = s.recv(now) || madeProgress
	return madeProgress
}

// recv drains each port and places incoming flits into the port's
// routeBuffer (the first switch stage).
func (s *MultiFlitChipletSwitchImpl) recv(
	now akita.VTimeInSec,
) (madeProgress bool) {
	for i := 0; i < s.numReqPerCycle; i++ {
		for _, port := range s.ports {
			item := port.Peek()
			if item == nil {
				continue
			}
			complex := s.portToComplexMapping[port]
			if !complex.routeBuffer.CanPush() {
				continue
			}

			flit := item.(*noc.Flit)
			s.traceHop(complex, flit, now)
			traceC2CFlit(s, complex, flit, now)
			s.logRecv(complex, flit, now)

			complex.routeBuffer.Push(flit)
			port.Retrieve(now)
			madeProgress = true
		}
	}
	return madeProgress
}

// isLocalEndpointPort reports whether the given port is the local end
// of a device↔switch link (RDMA engine or endpoint), not an inter-
// switch wire. Matches the same name patterns traceHop uses to skip
// non-hop traversals.
func isLocalEndpointPort(p akita.Port) bool {
	name := p.Name()
	return strings.Contains(name, "network_port") ||
		strings.Contains(name, "RDMA")
}

// flightKindForMsg classifies a payload msg as a request- or response-
// kind for the NetworkFlightTracer. Returns ok=false for msg types that
// are not part of the RDMA req/rsp flow (e.g., control messages).
// Called by endpoint.go; the constants are NetworkFlight* because the
// endpoint-to-endpoint window is what this kind labels — the
// RDMA-engine-to-RDMA-engine window uses RDMAFlight* constants and is
// emitted from mgpusim/rdma/rdmaengine.go.
func flightKindForMsg(m akita.Msg) (string, bool) {
	switch m.(type) {
	case *protocol.TransReadReq:
		return tracing.NetworkFlightKindReq, true
	case *protocol.TransDataReadyRsp:
		return tracing.NetworkFlightKindRsp, true
	case protocol.AccessReq:
		return tracing.NetworkFlightKindReq, true
	case protocol.AccessRsp:
		return tracing.NetworkFlightKindRsp, true
	}
	return "", false
}

// chipletPairFromMsg returns the "chiplet_NN" tokens for the original
// src and dst component port names embedded in msg.Meta(). Used as the
// task Detail field so the flight tracers can compute Manhattan hop
// bins.
func chipletPairFromMsg(m akita.Msg) (string, string) {
	srcParts := strings.Split(m.Meta().Src.Name(), ".")
	dstParts := strings.Split(m.Meta().Dst.Name(), ".")
	src := ""
	dst := ""
	if len(srcParts) >= 2 {
		src = srcParts[1]
	}
	if len(dstParts) >= 2 {
		dst = dstParts[1]
	}
	return src, dst
}

// chipletID strips the "chiplet_NN" token returned by chipletPairFromMsg
// to its integer index. ok is false when the token is empty or malformed.
func chipletID(token string) (int, bool) {
	if !strings.HasPrefix(token, "chiplet_") {
		return 0, false
	}
	id, err := strconv.Atoi(strings.TrimPrefix(token, "chiplet_"))
	if err != nil {
		return 0, false
	}
	return id, true
}

// traceC2CFlit records one message's flit count against its original
// src->dst chiplet pair, counted once at the source-injection point (head
// flit arriving from a local endpoint). Req and rsp inject at opposite
// chiplets, so each directed edge accumulates its own flit weight (req=1,
// data rsp=NumFlitInMsg). Same-chiplet and non-endpoint arrivals are
// skipped — only true cross-chiplet network injections count.
func traceC2CFlit(
	domain tracing.NamedHookable,
	complex *multiFlitPortComplex,
	flit *noc.Flit,
	now akita.VTimeInSec,
) {
	if !isLocalEndpointPort(complex.remotePort) {
		return
	}
	if flit.SeqID != 0 {
		return
	}
	srcTok, dstTok := chipletPairFromMsg(flit.Msg)
	src, okS := chipletID(srcTok)
	dst, okD := chipletID(dstTok)
	if !okS || !okD || src == dst {
		return
	}
	tracing.StartTask(0, 0, now, domain, tracing.C2CFlitKind, "",
		tracing.C2CFlitStep{
			SrcChip: src,
			DstChip: dst,
			Flits:   uint64(flit.NumFlitInMsg),
		})
}

// traceHop emits the hop-trace step that the HopTracer counts. The hop
// counts only inter-switch traversals; flits arriving from a local
// endpoint are not counted. Only the head flit (SeqID == 0) of a
// multi-flit message contributes, so a hop counts once per message per
// switch — matching the pre-wormhole msg-level accounting that the
// HopTracer's fixed-size buckets assume.
func (s *MultiFlitChipletSwitchImpl) traceHop(
	complex *multiFlitPortComplex,
	flit *noc.Flit,
	now akita.VTimeInSec,
) {
	if strings.Contains(complex.remotePort.Name(), "network_port") {
		return
	}
	if strings.Contains(complex.remotePort.Name(), "RDMA") {
		return
	}
	if flit.SeqID != 0 {
		return
	}
	tracing.AddFlitHops(uint64((flit.Msg.Meta().TrafficBytes + 15) / 16))
	tracing.AddTaskStep(flit.Msg.Meta().ID, now, s, "")
}

// logRecv prints a debug line when the global verbose flag is on so
// per-switch flit arrival can be traced manually.
func (s *MultiFlitChipletSwitchImpl) logRecv(
	complex *multiFlitPortComplex,
	flit *noc.Flit,
	now akita.VTimeInSec,
) {
	if !global.GetInstance().GetVerbose() {
		return
	}
	fmt.Printf(
		"recv, %s, on %s from %s, msg %d (%d/%d), at %.10f\n",
		s.Name(),
		complex.localPort.Name(),
		complex.remotePort.Name(),
		flit.Msg.Meta().ID,
		flit.SeqID+1,
		flit.NumFlitInMsg,
		now,
	)
}

// route pops flits from each port's routeBuffer, assigns the flit's
// OutputBuf via the routing table, and pushes the flit to that port's
// forwardBuffer for arbitration.
func (s *MultiFlitChipletSwitchImpl) route(
	now akita.VTimeInSec,
) (madeProgress bool) {
	for i := 0; i < s.numReqPerCycle; i++ {
		for _, port := range s.ports {
			complex := s.portToComplexMapping[port]
			item := complex.routeBuffer.Peek()
			if item == nil {
				continue
			}
			flit := item.(*noc.Flit)
			outPort := s.routingTable.FindPort(flit.Msg.Meta().Dst)
			voq := s.voqFor(complex, outPort)
			if !voq.CanPush() {
				continue
			}
			flit.OutputBuf = s.portToComplexMapping[outPort].sendOutBuffer
			complex.routeBuffer.Pop()
			voq.Push(flit)
			madeProgress = true
		}
	}
	return madeProgress
}

// voqFor returns the per-output-port virtual output queue for flits routed
// from this input complex to outPort, creating and registering it with the
// arbiter on first use. Keying by output port confines each queue to one
// destination, so a head flit blocked on a full output never stalls a flit
// bound for a different, free output (head-of-line blocking removed).
func (s *MultiFlitChipletSwitchImpl) voqFor(
	complex *multiFlitPortComplex,
	outPort akita.Port,
) util.Buffer {
	if complex.forwardVOQ == nil {
		complex.forwardVOQ = make(map[akita.Port]util.Buffer)
	}
	voq, ok := complex.forwardVOQ[outPort]
	if !ok {
		voq = util.NewBuffer(s.vctBufSize)
		complex.forwardVOQ[outPort] = voq
		s.arbiter.AddBuffer(voq)
	}
	return voq
}

// forward selects flits from arbitrated forwardBuffers and pushes each
// flit to its previously assigned OutputBuf (the target port's
// sendOutBuffer).
func (s *MultiFlitChipletSwitchImpl) forward(
	now akita.VTimeInSec,
) (madeProgress bool) {
	for i := 0; i < s.numReqPerCycle; i++ {
		buffers := s.arbiter.Arbitrate(now)
		for _, buf := range buffers {
			item := buf.Peek()
			if item == nil {
				// ASSERT: arbiter selected an empty buffer — but it already
				// did markOneFlitSent/outgoingReqs++ for it → reservation
				// desync. Should never happen.
				panic("forward: arbiter selected empty buffer (reservation desync)")
			}
			flit := item.(*noc.Flit)
			if !flit.OutputBuf.CanPush() {
				// ASSERT: arbiter selected a buffer whose OutputBuf is full,
				// but it pre-checks CanPush + already counted the flit →
				// reservation desync / wormhole corruption.
				panic("forward: selected flit's OutputBuf full (reservation desync)")
			}
			flit.OutputBuf.Push(flit)
			buf.Pop()
			madeProgress = true
		}
	}
	return madeProgress
}

// tryDeliver drains sendOutBuffer of each port to the underlying connection.
// The flit's Src/Dst are rewritten to reflect this hop's link endpoints.
func (s *MultiFlitChipletSwitchImpl) tryDeliver(
	now akita.VTimeInSec,
) (madeProgress bool) {
	for i := 0; i < s.numReqPerCycle; i++ {
		for _, port := range s.ports {
			complex := s.portToComplexMapping[port]
			item := complex.sendOutBuffer.Peek()
			if item == nil {
				continue
			}
			flit := item.(*noc.Flit)
			flit.Meta().Src = complex.localPort
			flit.Meta().Dst = complex.remotePort
			flit.Meta().SendTime = now
			if err := complex.localPort.Send(flit); err == nil {
				complex.sendOutBuffer.Pop()
				madeProgress = true
			} else {
				s.logSendOutFail(complex, flit, now)
				tracing.StartTask(0, 0, now, s,
					tracing.SendOutFailKind, "",
					complex.localPort.Name())
			}
		}
	}
	return madeProgress
}

// logSendOutFail prints a verbose debug line when sending a flit to the
// next hop fails (the downstream port/connection rejected it). The flit
// stays in sendOutBuffer for retry.
func (s *MultiFlitChipletSwitchImpl) logSendOutFail(
	complex *multiFlitPortComplex,
	flit *noc.Flit,
	now akita.VTimeInSec,
) {
	if !global.GetInstance().GetVerbose() {
		return
	}

	fmt.Printf(
		"Error: sendOut fail, %s, on %s -> %s, msg %d (%d/%d), at %.10f\n",
		s.Name(),
		complex.localPort.Name(),
		complex.remotePort.Name(),
		flit.Msg.Meta().ID,
		flit.SeqID+1,
		flit.NumFlitInMsg,
		now,
	)
}

// DeadlockSummary walks each port complex and sums the depths of the
// pipeline-stage buffers plus the count of ports still holding an
// un-retrieved incoming flit. anyPending is true when any buffer or port
// has work in flight; the driver's quiesce-deadlock detector uses it to
// flag switches that are stuck with no forward progress.
func (s *MultiFlitChipletSwitchImpl) DeadlockSummary() (line string, anyPending bool) {
	// Split every buffer by message-class direction (port name carries "Req"
	// or "Rsp") so the dump shows WHICH network direction is jammed. mm's
	// multi-flit data replies (4 flits) saturate the Rsp side while Req stays
	// clear, so a Rsp>>Req split here localizes the response-network deadlock.
	var qRoute, qFwd, qSend, rRoute, rFwd, rSend int // q=Req, r=Rsp
	portsWithMsg, reqPortMsg, rspPortMsg := 0, 0, 0
	for _, p := range s.ports {
		if p == nil {
			continue
		}
		isRsp := strings.Contains(p.Name(), "Rsp")
		if p.Peek() != nil {
			portsWithMsg++
			if isRsp {
				rspPortMsg++
			} else {
				reqPortMsg++
			}
		}
		pc := s.portToComplexMapping[p]
		if pc == nil {
			continue
		}
		rt, fw, so := 0, 0, 0
		if pc.routeBuffer != nil {
			rt = pc.routeBuffer.Size()
		}
		for _, voq := range pc.forwardVOQ {
			if voq != nil {
				fw += voq.Size()
			}
		}
		if pc.sendOutBuffer != nil {
			so = pc.sendOutBuffer.Size()
		}
		if isRsp {
			rRoute += rt
			rFwd += fw
			rSend += so
		} else {
			qRoute += rt
			qFwd += fw
			qSend += so
		}
	}
	anyPending = qRoute+qFwd+qSend+rRoute+rFwd+rSend+portsWithMsg > 0
	line = fmt.Sprintf(
		"portsWithMsg=%d(Req=%d/Rsp=%d) route[Req=%d/Rsp=%d] "+
			"forward[Req=%d/Rsp=%d] sendOut[Req=%d/Rsp=%d]",
		portsWithMsg, reqPortMsg, rspPortMsg,
		qRoute, rRoute, qFwd, rFwd, qSend, rSend)
	if anyPending {
		line += " || " + s.wormSplitHoLDiag()
	}
	return
}

// wormSplitHoLDiag classifies the Rsp-network deadlock. At quiesce the network
// is frozen, so a msg with fewer flits present here than NumFlitInMsg is
// genuinely SPLIT across the switch boundary (wormhole spanning -> VCT helps).
// HoLports counts forward buffers whose head flit's output is full while a
// follower's output has room (head-of-line blocking -> needs virtual channels,
// VCT alone won't fix). partial>>complete => worm-spanning dominant;
// complete>>partial with HoLports>0 => HoL/capacity dominant.
func (s *MultiFlitChipletSwitchImpl) wormSplitHoLDiag() string {
	type lister interface{ GetBuffer() []interface{} }
	present := map[uint64]int{}
	total := map[uint64]int{}
	addBuf := func(b interface{}) {
		l, ok := b.(lister)
		if !ok {
			return
		}
		for _, e := range l.GetBuffer() {
			f, ok := e.(*noc.Flit)
			if !ok || f.Msg == nil {
				continue
			}
			id := f.Msg.Meta().ID
			present[id]++
			total[id] = f.NumFlitInMsg
		}
	}
	holPorts := 0
	for _, p := range s.ports {
		if p == nil || !strings.Contains(p.Name(), "Rsp") {
			continue
		}
		pc := s.portToComplexMapping[p]
		if pc == nil {
			continue
		}
		if pc.routeBuffer != nil {
			addBuf(pc.routeBuffer)
		}
		for _, voq := range pc.forwardVOQ {
			if voq != nil {
				addBuf(voq)
				holPorts += headOfLineBlocked(voq)
			}
		}
		if pc.sendOutBuffer != nil {
			addBuf(pc.sendOutBuffer)
		}
	}
	partialMsgs, completeMsgs, partialFlits := 0, 0, 0
	for id, n := range present {
		if n < total[id] {
			partialMsgs++
			partialFlits += n
		} else {
			completeMsgs++
		}
	}
	return fmt.Sprintf(
		"RspMsgs=%d(partial=%d/complete=%d partialFlits=%d) HoLports=%d",
		len(present), partialMsgs, completeMsgs, partialFlits, holPorts)
}

// headOfLineBlocked returns 1 if the forward buffer's head flit cannot advance
// (its output buffer is full) while some flit behind it CAN advance (its output
// has room) — i.e. an independent message is stalled purely by the head.
func headOfLineBlocked(b interface{}) int {
	l, ok := b.(interface{ GetBuffer() []interface{} })
	if !ok {
		return 0
	}
	elems := l.GetBuffer()
	if len(elems) < 2 {
		return 0
	}
	head, ok := elems[0].(*noc.Flit)
	if !ok || head.OutputBuf == nil || head.OutputBuf.CanPush() {
		return 0
	}
	for _, e := range elems[1:] {
		f, ok := e.(*noc.Flit)
		if ok && f.OutputBuf != nil && f.OutputBuf.CanPush() {
			return 1
		}
	}
	return 0
}

/*
 *
 * Builder for MultiFlitChipletSwitchImpl
 *
 */

// MultiFlitChipletSwitchImplBuilder can build MultiFlitChipletSwitchImpls
type MultiFlitChipletSwitchImplBuilder struct {
	engine         akita.Engine
	freq           akita.Freq
	routingTable   routing.Table
	arbiter        arbitration.Arbiter
	numReqPerCycle int
	switchLatency  int
	flitByteSize   int
	vctBufSize     int

	row int
	col int
	y   int
	x   int
}

func MakeMultiFlitChipletSwitchImplBuilder() *MultiFlitChipletSwitchImplBuilder {
	b := MultiFlitChipletSwitchImplBuilder{}
	return &b
}

// WithEngine sets the engine that the MultiFlitChipletSwitchImpl to build uses.
func (b *MultiFlitChipletSwitchImplBuilder) WithEngine(engine akita.Engine) *MultiFlitChipletSwitchImplBuilder {
	b.engine = engine
	return b
}

// WithFreq sets the frequency that the MultiFlitChipletSwitchImpl to build works at.
func (b *MultiFlitChipletSwitchImplBuilder) WithFreq(freq akita.Freq) *MultiFlitChipletSwitchImplBuilder {
	b.freq = freq
	return b
}

// WithArbiter sets the arbiter to be used by the swtich to build.
func (b *MultiFlitChipletSwitchImplBuilder) WithArbiter(arbiter arbitration.Arbiter) *MultiFlitChipletSwitchImplBuilder {
	b.arbiter = arbiter
	return b
}

// WithRoutingTable sets the routing table to be used by the MultiFlitChipletSwitchImpl to build.
func (b *MultiFlitChipletSwitchImplBuilder) WithRoutingTable(rt routing.Table) *MultiFlitChipletSwitchImplBuilder {
	b.routingTable = rt
	return b
}

func (b *MultiFlitChipletSwitchImplBuilder) WithNumReqPerCycle(numReqPerCycle int) *MultiFlitChipletSwitchImplBuilder {
	b.numReqPerCycle = numReqPerCycle
	return b
}

func (b *MultiFlitChipletSwitchImplBuilder) WithMaxOutgoingReqsPerChiplet(maxOutgoingReqsPerChiplet int) *MultiFlitChipletSwitchImplBuilder {
	return b
}

func (b *MultiFlitChipletSwitchImplBuilder) WithGridSize(r, c int) *MultiFlitChipletSwitchImplBuilder {
	b.row = r
	b.col = c
	return b
}

func (b *MultiFlitChipletSwitchImplBuilder) WithSwitchPosition(y, x int) *MultiFlitChipletSwitchImplBuilder {
	b.y = y
	b.x = x
	return b
}

func (b *MultiFlitChipletSwitchImplBuilder) WithSwitchLatency(sw int) *MultiFlitChipletSwitchImplBuilder {
	b.switchLatency = sw
	return b
}

func (b *MultiFlitChipletSwitchImplBuilder) WithFlitByteSize(f int) *MultiFlitChipletSwitchImplBuilder {
	b.flitByteSize = f
	return b
}

// WithVCTBufSize sets the per-output VOQ depth. Must exceed the largest
// packet's flit count for VCT store-and-forward to hold a whole packet.
func (b *MultiFlitChipletSwitchImplBuilder) WithVCTBufSize(n int) *MultiFlitChipletSwitchImplBuilder {
	b.vctBufSize = n
	return b
}

// Build creates a new MultiFlitChipletSwitchImpl
func (b MultiFlitChipletSwitchImplBuilder) Build(name string) *MultiFlitChipletSwitchImpl {

	b.engineMustBeGiven()
	b.freqMustNotBeZero()
	b.routingTableMustBeGiven()
	b.arbiterMustBeGiven()

	s := &MultiFlitChipletSwitchImpl{}
	s.TickingComponent = akita.NewTickingComponent(name, b.engine, b.freq, s)
	s.routingTable = b.routingTable
	s.arbiter = b.arbiter.(*arbitration.FlitAwareXBarArbiter)
	s.portToComplexMapping = make(map[akita.Port]*multiFlitPortComplex)
	s.numReqPerCycle = b.numReqPerCycle
	s.flitByteSize = b.flitByteSize
	// Default VOQ depth >> max packet (cacheline 64B / 16B = 4 flits) so VCT
	// store-and-forward can always buffer a whole packet at one hop.
	s.vctBufSize = b.vctBufSize
	if s.vctBufSize == 0 {
		s.vctBufSize = 16
	}
	s.encodingOverhead = 0
	s.switchLatency = b.switchLatency
	s.row = b.row
	s.col = b.col

	return s
}

func (b MultiFlitChipletSwitchImplBuilder) engineMustBeGiven() {
	if b.engine == nil {
		panic("engine of MultiFlitChipletSwitchImpl is not given")
	}
}

func (b MultiFlitChipletSwitchImplBuilder) freqMustNotBeZero() {
	if b.freq == 0 {
		panic("MultiFlitChipletSwitchImpl frequency cannot be 0")
	}
}

func (b MultiFlitChipletSwitchImplBuilder) routingTableMustBeGiven() {
	if b.routingTable == nil {
		panic("MultiFlitChipletSwitchImpl requires a routing table to operate")
	}
}

func (b MultiFlitChipletSwitchImplBuilder) arbiterMustBeGiven() {
	if b.arbiter == nil {
		panic("MultiFlitChipletSwitchImpl requires an arbiter to operate")
	}
}
