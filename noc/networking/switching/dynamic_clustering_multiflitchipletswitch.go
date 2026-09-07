package switching

import (
	"fmt"
	"math"
	"strings"

	"gitlab.com/akita/akita"
	"gitlab.com/akita/noc"
	"gitlab.com/akita/noc/networking/arbitration"
	"gitlab.com/akita/noc/networking/routing"
	"gitlab.com/akita/protocol"
	"gitlab.com/akita/util/global"
	"gitlab.com/akita/util/tracing"
)

/*
 *
 * DynamicClusteringMultiFlitChipletSwitch
 * -
 *
 * Flit-level wormhole switch with head-triggered per-message multicast at
 * the branch node. When a DistributeRsp's head flit (SeqID == 0) arrives
 * at a switch whose swId == BranchNodeId, recv() spends TWO ticks on the
 * head:
 *   - Tick 1 (first head observation): create DataReadyRsp(Dst=FirstDst),
 *     split it into N flits via msgToFlits, append to the per-msg
 *     pendingFlits queue. The head flit STAYS in the input port.
 *   - Tick 2 (second head observation): create DataReadyRsp(Dst=SecondDst)
 *     if present and != FirstDst, append its N flits to the queue. THEN
 *     retrieve the head from the input port.
 * Body / tail flits of the original DistributeRsp (SeqID > 0) arrive on
 * later ticks and are silently retrieved — their content is already
 * represented in the generated output flits. branchEmit() drains
 * pendingFlits one flit per tick into the inject port's routeBuffer.
 * After all body/tail are absorbed and the queue is empty, the assembly
 * entry is deleted. SortPorts orders response ports ahead of request
 * ports so responses drain first under arbitration.
 */

// distPrefetchBranchAssembly holds per-DistributeRsp branch state at this
// switch. Created at the first observation of the head flit; deleted
// when every body/tail has been silently absorbed AND pendingFlits has
// been fully drained into routeBuffer.
type distPrefetchBranchAssembly struct {
	msg          *protocol.DistributeRsp
	pendingFlits []*noc.Flit            // FirstDst flits ‖ SecondDst flits
	numFlitInMsg int                    // N — total flits of the input wormhole
	bodyConsumed int                    // body/tail flits silently retrieved
	headEmitted  bool                   // FirstDst flits appended to queue
	fullyEmitted bool                   // SecondDst (if any) appended; head retrieved
	injectPort   *multiFlitPortComplex  // input complex (queue drains here)
}

type DynamicClusteringMultiFlitChipletSwitch struct {
	*akita.TickingComponent

	ports                []akita.Port
	portToComplexMapping map[akita.Port]*multiFlitPortComplex
	routingTable         routing.Table
	arbiter              *arbitration.FlitAwareXBarArbiter

	flitByteSize     int
	encodingOverhead float64
	switchLatency    int
	numReqPerCycle   int

	row  int
	col  int
	y    int
	x    int
	swId int

	nextPortId int

	ToRDMAReqPort akita.Port

	// branchAssemblies tracks per-DistributeRsp branch state at this
	// switch, keyed by the originating message ID. Created at the first
	// head-flit observation; deleted when every body/tail has been
	// silently retrieved AND pendingFlits has been drained.
	branchAssemblies map[uint64]*distPrefetchBranchAssembly
}

// SortPorts orders response ports before request ports so the arbiter
// drains Rsp first when iterating.
func (s *DynamicClusteringMultiFlitChipletSwitch) SortPorts() {
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

func (s *DynamicClusteringMultiFlitChipletSwitch) addPort(
	pc *multiFlitPortComplex,
) {
	s.ports = append(s.ports, pc.localPort)
	s.portToComplexMapping[pc.localPort] = pc
	s.portToComplexMapping[pc.remotePort] = pc
	s.arbiter.AddBuffer(pc.forwardBuffer)

	s.routingTable.DefineRoute(pc.remotePort, pc.localPort)
}

func (s *DynamicClusteringMultiFlitChipletSwitch) Tick(now akita.VTimeInSec) bool {
	s.arbiter.Reset()
	s.nextPortId = 0

	madeProgress := false
	for i := 0; i < s.numReqPerCycle; i++ {
		// Reverse stage order (downstream-first) so a full pipeline can
		// slide an entire stage in one tick. branchEmit and recv both
		// feed routeBuffer; branchEmit runs before recv so previously-
		// generated pendingFlits drain ahead of newly arrived flits.
		// Same shape as distmultiflitchipletswitch.go.
		madeProgress = s.sendOut(now) || madeProgress
		madeProgress = s.forward(now) || madeProgress
		madeProgress = s.route(now) || madeProgress
		madeProgress = s.branchEmit(now) || madeProgress
		madeProgress = s.recv(now) || madeProgress
	}
	return madeProgress
}

// recv drains each port. Branch flits (DistributeRsp targeting this
// swId) are handled by recvBranchFlit; all other flits land in their
// port's routeBuffer as in the master switch.
func (s *DynamicClusteringMultiFlitChipletSwitch) recv(
	now akita.VTimeInSec,
) (madeProgress bool) {
	for _, port := range s.ports {
		item := port.Peek()
		if item == nil {
			continue
		}
		complex := s.portToComplexMapping[port]
		flit := item.(*noc.Flit)

		if s.isBranchFlit(flit) {
			if s.recvBranchFlit(complex, port, flit, now) {
				madeProgress = true
			}
			continue
		}

		if !complex.routeBuffer.CanPush() {
			continue
		}
		s.traceHop(complex, flit, now)
		s.traceFlightStart(complex, flit, now)
		s.traceFlightEnd(complex, flit, now)
		s.logRecv(complex, flit, now)

		complex.routeBuffer.Push(flit)
		port.Retrieve(now)
		madeProgress = true
	}
	return madeProgress
}

// recvBranchFlit handles a flit of a DistributeRsp whose BranchNodeId
// matches this switch. Behavior depends on the flit's role:
//
//   - Head flit (SeqID == 0), 1st observation:
//       create assembly, build FirstDst DataReadyRsp + its N flits,
//       append to pendingFlits. Head STAYS in the input port.
//   - Head flit, 2nd observation (next tick):
//       build SecondDst DataReadyRsp (if SecondDst != nil && !=
//       FirstDst) + its N flits, append to pendingFlits. Head is now
//       retrieved.
//   - Body / tail flit (SeqID > 0):
//       silently retrieved (their content is already in pendingFlits
//       built from the head). Assembly entry tracks consumption.
//
// Returns true iff the input port was advanced this tick.
func (s *DynamicClusteringMultiFlitChipletSwitch) recvBranchFlit(
	complex *multiFlitPortComplex,
	port akita.Port,
	flit *noc.Flit,
	now akita.VTimeInSec,
) bool {
	distRsp := flit.Msg.(*protocol.DistributeRsp)
	id := distRsp.Meta().ID

	if flit.SeqID == 0 {
		return s.recvBranchHead(complex, port, flit, distRsp, id, now)
	}
	return s.recvBranchBody(port, flit, id, now)
}

// recvBranchHead handles the two-tick head-flit lifecycle: 1st tick
// builds FirstDst flits, 2nd tick builds SecondDst flits + retrieves
// the head.
func (s *DynamicClusteringMultiFlitChipletSwitch) recvBranchHead(
	complex *multiFlitPortComplex,
	port akita.Port,
	flit *noc.Flit,
	distRsp *protocol.DistributeRsp,
	id uint64,
	now akita.VTimeInSec,
) bool {
	asm, exists := s.branchAssemblies[id]
	if !exists {
		asm = &distPrefetchBranchAssembly{
			msg:          distRsp,
			numFlitInMsg: flit.NumFlitInMsg,
			injectPort:   complex,
		}
		s.branchAssemblies[id] = asm
		// Tracing fires exactly once per branch message — at the very
		// first head observation. The 2nd-tick head observation must
		// not re-fire these hooks.
		s.traceHop(complex, flit, now)
		s.traceFlightStart(complex, flit, now)
		s.traceFlightEnd(complex, flit, now)
	}
	s.logRecv(complex, flit, now)

	if !asm.headEmitted {
		first := s.transformToDataReadyRsp(distRsp, distRsp.FirstDst, now)
		asm.pendingFlits = append(asm.pendingFlits, s.msgToFlits(first)...)
		asm.headEmitted = true
		// Head stays in the port; do NOT Retrieve.
		return true
	}

	// 2nd observation: append SecondDst flits (if applicable) and
	// retrieve the head.
	if distRsp.SecondDst != nil && distRsp.SecondDst != distRsp.FirstDst {
		second := s.transformToDataReadyRsp(distRsp, distRsp.SecondDst, now)
		asm.pendingFlits = append(asm.pendingFlits, s.msgToFlits(second)...)
	}
	asm.fullyEmitted = true
	port.Retrieve(now)
	s.tryDeleteAssembly(id, asm)
	return true
}

// recvBranchBody silently retrieves a body / tail flit whose content
// has already been represented in pendingFlits via the head's
// transformation. Panics if the head has not been seen yet — wormhole
// ordering invariant.
func (s *DynamicClusteringMultiFlitChipletSwitch) recvBranchBody(
	port akita.Port,
	flit *noc.Flit,
	id uint64,
	now akita.VTimeInSec,
) bool {
	asm, ok := s.branchAssemblies[id]
	if !ok {
		panic(fmt.Sprintf(
			"branch body flit before head: msg %d seq %d",
			id, flit.SeqID))
	}
	port.Retrieve(now)
	asm.bodyConsumed++
	s.tryDeleteAssembly(id, asm)
	return true
}

// branchEmit drains every assembly's pendingFlits into its injectPort's
// routeBuffer at one flit per CanPush opportunity. After the queue is
// empty and all body / tail flits have been consumed, the assembly is
// removed.
func (s *DynamicClusteringMultiFlitChipletSwitch) branchEmit(
	now akita.VTimeInSec,
) (madeProgress bool) {
	for id, asm := range s.branchAssemblies {
		for len(asm.pendingFlits) > 0 &&
			asm.injectPort.routeBuffer.CanPush() {
			asm.injectPort.routeBuffer.Push(asm.pendingFlits[0])
			asm.pendingFlits = asm.pendingFlits[1:]
			madeProgress = true
		}
		s.tryDeleteAssembly(id, asm)
	}
	return madeProgress
}

// tryDeleteAssembly removes the assembly entry once the head's
// transformation has fully emitted, every body / tail has been
// absorbed, and pendingFlits has been drained.
func (s *DynamicClusteringMultiFlitChipletSwitch) tryDeleteAssembly(
	id uint64, asm *distPrefetchBranchAssembly,
) {
	if !asm.fullyEmitted {
		return
	}
	if asm.bodyConsumed != asm.numFlitInMsg-1 {
		return
	}
	if len(asm.pendingFlits) > 0 {
		return
	}
	delete(s.branchAssemblies, id)
}

// transformToDataReadyRsp builds one DataReadyRsp for the given
// destination, mirroring the data / info / RspTo of the original
// DistributeRsp.
func (s *DynamicClusteringMultiFlitChipletSwitch) transformToDataReadyRsp(
	distRsp *protocol.DistributeRsp,
	dst akita.Port,
	now akita.VTimeInSec,
) akita.Msg {
	return protocol.DataReadyRspBuilder{}.
		WithSendTime(now).
		WithSrc(distRsp.Meta().Src).
		WithDst(dst).
		WithRspTo(distRsp.GetRespondTo()).
		WithData(distRsp.Data).
		WithInfo(distRsp.Info).
		Build()
}

// msgToFlits splits a message into N flits based on its TrafficBytes
// and the switch's flitByteSize / encodingOverhead. Every flit shares
// the same Msg reference so EndPoint reassembly (keyed by Meta().ID)
// counts them as one message. Mirrors the sibling distmultiflit
// switch's msgToFlits.
func (s *DynamicClusteringMultiFlitChipletSwitch) msgToFlits(
	msg akita.Msg,
) []*noc.Flit {
	numFlit := 1
	if msg.Meta().TrafficBytes > 0 {
		trafficByte := msg.Meta().TrafficBytes
		trafficByte += int(math.Ceil(
			float64(trafficByte) * s.encodingOverhead))
		numFlit = (trafficByte-1)/s.flitByteSize + 1
	}

	flits := make([]*noc.Flit, numFlit)
	for i := 0; i < numFlit; i++ {
		flits[i] = noc.FlitBuilder{}.
			WithSrc(msg.Meta().Src).
			WithDst(msg.Meta().Dst).
			WithSeqID(i).
			WithNumFlitInMsg(numFlit).
			WithMsg(msg).
			Build()
	}
	return flits
}

func (s *DynamicClusteringMultiFlitChipletSwitch) isBranchFlit(flit *noc.Flit) bool {
	distRsp, ok := flit.Msg.(*protocol.DistributeRsp)
	if !ok {
		return false
	}
	return distRsp.BranchNodeId == s.swId
}

// traceFlightStart fires when the head flit of an RDMA req/rsp first
// enters the network at the source chiplet's switch — i.e., this switch
// just received the flit from a local RDMA/endpoint port. Marks the
// beginning of the network-only transit interval. Mirrors master
// multiflitchipletswitch.go.
func (s *DynamicClusteringMultiFlitChipletSwitch) traceFlightStart(
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
	kind, ok := flightKindForMsg(flit.Msg)
	if !ok {
		return
	}
	srcChip, dstChip := chipletPairFromMsg(flit.Msg)
	tracing.StartTask(
		tracing.NetworkFlightTaskID(flit.Msg.Meta().ID, kind),
		0, now, s, kind, "", srcChip+"."+dstChip)
}

// traceFlightEnd fires when the head flit arrives at the destination
// chiplet's switch — the flit came in from an inter-switch port and its
// next hop (per routing) goes to a local endpoint port. Marks the end
// of the network-only transit interval.
func (s *DynamicClusteringMultiFlitChipletSwitch) traceFlightEnd(
	complex *multiFlitPortComplex,
	flit *noc.Flit,
	now akita.VTimeInSec,
) {
	if isLocalEndpointPort(complex.remotePort) {
		return
	}
	if flit.SeqID != 0 {
		return
	}
	kind, ok := flightKindForMsg(flit.Msg)
	if !ok {
		return
	}
	outPort := s.routingTable.FindPort(flit.Msg.Meta().Dst)
	if outPort == nil {
		return
	}
	outComplex, found := s.portToComplexMapping[outPort]
	if !found || !isLocalEndpointPort(outComplex.remotePort) {
		return
	}
	tracing.EndTask(
		tracing.NetworkFlightTaskID(flit.Msg.Meta().ID, kind), now, s)
}

func (s *DynamicClusteringMultiFlitChipletSwitch) traceHop(
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

func (s *DynamicClusteringMultiFlitChipletSwitch) logRecv(
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

// route advances flits from each input complex's routeBuffer to the
// chosen output complex's forwardBuffer. Branch flits have already been
// rewritten as single-destination DataReadyRsp flits in recv(), so this
// stage is uniformly the master single-destination path.
func (s *DynamicClusteringMultiFlitChipletSwitch) route(
	now akita.VTimeInSec,
) (madeProgress bool) {
	for _, port := range s.ports {
		complex := s.portToComplexMapping[port]
		item := complex.routeBuffer.Peek()
		if item == nil {
			continue
		}
		if !complex.forwardBuffer.CanPush() {
			continue
		}
		flit := item.(*noc.Flit)
		s.assignFlitOutputBuf(flit)
		complex.routeBuffer.Pop()
		complex.forwardBuffer.Push(flit)
		madeProgress = true
	}
	return madeProgress
}

func (s *DynamicClusteringMultiFlitChipletSwitch) assignFlitOutputBuf(f *noc.Flit) {
	outPort := s.routingTable.FindPort(f.Msg.Meta().Dst)
	complex := s.portToComplexMapping[outPort]
	f.OutputBuf = complex.sendOutBuffer
}

func (s *DynamicClusteringMultiFlitChipletSwitch) forward(
	now akita.VTimeInSec,
) (madeProgress bool) {
	buffers := s.arbiter.Arbitrate(now)
	for _, buf := range buffers {
		item := buf.Peek()
		if item == nil {
			continue
		}
		flit := item.(*noc.Flit)
		if !flit.OutputBuf.CanPush() {
			continue
		}
		flit.OutputBuf.Push(flit)
		buf.Pop()
		madeProgress = true
	}
	return madeProgress
}

func (s *DynamicClusteringMultiFlitChipletSwitch) sendOut(
	now akita.VTimeInSec,
) (madeProgress bool) {
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
	return madeProgress
}

func (s *DynamicClusteringMultiFlitChipletSwitch) logSendOutFail(
	complex *multiFlitPortComplex,
	flit *noc.Flit,
	now akita.VTimeInSec,
) {
	if !global.GetInstance().GetVerbose() {
		return
	}
	fmt.Printf(
		"sendOut fail, %s, on %s -> %s, msg %d (%d/%d), at %.10f\n",
		s.Name(),
		complex.localPort.Name(),
		complex.remotePort.Name(),
		flit.Msg.Meta().ID,
		flit.SeqID+1,
		flit.NumFlitInMsg,
		now,
	)
}

/*
 *
 * Builder for DynamicClusteringMultiFlitChipletSwitch
 *
 */

type DynamicClusteringMultiFlitChipletSwitchBuilder struct {
	engine         akita.Engine
	freq           akita.Freq
	routingTable   routing.Table
	arbiter        arbitration.Arbiter
	numReqPerCycle int
	switchLatency  int
	flitByteSize   int

	row int
	col int
	y   int
	x   int
}

func MakeDynamicClusteringMultiFlitChipletSwitchBuilder() *DynamicClusteringMultiFlitChipletSwitchBuilder {
	b := DynamicClusteringMultiFlitChipletSwitchBuilder{}
	return &b
}

func (b *DynamicClusteringMultiFlitChipletSwitchBuilder) WithEngine(engine akita.Engine) *DynamicClusteringMultiFlitChipletSwitchBuilder {
	b.engine = engine
	return b
}

func (b *DynamicClusteringMultiFlitChipletSwitchBuilder) WithFreq(freq akita.Freq) *DynamicClusteringMultiFlitChipletSwitchBuilder {
	b.freq = freq
	return b
}

func (b *DynamicClusteringMultiFlitChipletSwitchBuilder) WithArbiter(arbiter arbitration.Arbiter) *DynamicClusteringMultiFlitChipletSwitchBuilder {
	b.arbiter = arbiter
	return b
}

func (b *DynamicClusteringMultiFlitChipletSwitchBuilder) WithRoutingTable(rt routing.Table) *DynamicClusteringMultiFlitChipletSwitchBuilder {
	b.routingTable = rt
	return b
}

func (b *DynamicClusteringMultiFlitChipletSwitchBuilder) WithNumReqPerCycle(numReqPerCycle int) *DynamicClusteringMultiFlitChipletSwitchBuilder {
	b.numReqPerCycle = numReqPerCycle
	return b
}

func (b *DynamicClusteringMultiFlitChipletSwitchBuilder) WithMaxOutgoingReqsPerChiplet(maxOutgoingReqsPerChiplet int) *DynamicClusteringMultiFlitChipletSwitchBuilder {
	return b
}

func (b *DynamicClusteringMultiFlitChipletSwitchBuilder) WithGridSize(r, c int) *DynamicClusteringMultiFlitChipletSwitchBuilder {
	b.row = r
	b.col = c
	return b
}

func (b *DynamicClusteringMultiFlitChipletSwitchBuilder) WithSwitchPosition(y, x int) *DynamicClusteringMultiFlitChipletSwitchBuilder {
	b.y = y
	b.x = x
	return b
}

func (b *DynamicClusteringMultiFlitChipletSwitchBuilder) WithSwitchLatency(sw int) *DynamicClusteringMultiFlitChipletSwitchBuilder {
	b.switchLatency = sw
	return b
}

func (b *DynamicClusteringMultiFlitChipletSwitchBuilder) WithFlitByteSize(flitByteSize int) *DynamicClusteringMultiFlitChipletSwitchBuilder {
	b.flitByteSize = flitByteSize
	return b
}

func (b DynamicClusteringMultiFlitChipletSwitchBuilder) Build(name string) *DynamicClusteringMultiFlitChipletSwitch {
	b.engineMustBeGiven()
	b.freqMustNotBeZero()
	b.routingTableMustBeGiven()
	b.arbiterMustBeGiven()

	s := &DynamicClusteringMultiFlitChipletSwitch{}
	s.TickingComponent = akita.NewTickingComponent(name, b.engine, b.freq, s)
	s.routingTable = b.routingTable
	s.arbiter = b.arbiter.(*arbitration.FlitAwareXBarArbiter)
	s.portToComplexMapping = make(map[akita.Port]*multiFlitPortComplex)
	s.branchAssemblies = make(map[uint64]*distPrefetchBranchAssembly)
	s.numReqPerCycle = b.numReqPerCycle
	s.flitByteSize = b.flitByteSize
	s.encodingOverhead = 0
	s.switchLatency = b.switchLatency
	s.row = b.row
	s.col = b.col
	s.y = b.y
	s.x = b.x
	s.swId = b.y*b.col + b.x

	return s
}

func (b DynamicClusteringMultiFlitChipletSwitchBuilder) engineMustBeGiven() {
	if b.engine == nil {
		panic("engine of DynamicClusteringMultiFlitChipletSwitch is not given")
	}
}

func (b DynamicClusteringMultiFlitChipletSwitchBuilder) freqMustNotBeZero() {
	if b.freq == 0 {
		panic("DynamicClusteringMultiFlitChipletSwitch frequency cannot be 0")
	}
}

func (b DynamicClusteringMultiFlitChipletSwitchBuilder) routingTableMustBeGiven() {
	if b.routingTable == nil {
		panic("DynamicClusteringMultiFlitChipletSwitch requires a routing table to operate")
	}
}

func (b DynamicClusteringMultiFlitChipletSwitchBuilder) arbiterMustBeGiven() {
	if b.arbiter == nil {
		panic("DynamicClusteringMultiFlitChipletSwitch requires an arbiter to operate")
	}
}
