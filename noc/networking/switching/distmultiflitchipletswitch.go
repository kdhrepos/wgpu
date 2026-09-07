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
 * DistMultiFlitChipletSwitch
 * -
 *
 * Wormhole flit-level switch. Internal latency is zero (forward-order
 * stages run in a single Tick); link latency lives in
 * MultiFlitSwitchConnection.
 *
 * Branch transformation: when a flit of *protocol.DistributeRsp arrives
 * at the switch whose swId == DistributeRsp.BranchNodeId, the switch
 * accumulates all flits of that message, transforms the DistributeRsp
 * into one or two DataReadyRsp messages (FirstDst, optional SecondDst),
 * re-splits them into flits, and injects the new flits back into the
 * normal route → forward → sendOut path.
 */

// distBranchAssembly accumulates the flits of a DistributeRsp message that
// arrives at a branch switch, then holds the freshly-split DataReadyRsp
// flits until they can be drained into the injectPort's routeBuffer one
// at a time.
type distBranchAssembly struct {
	msg          *protocol.DistributeRsp
	numArrived   int
	numRequired  int
	injectPort   *multiFlitPortComplex
	pendingFlits []*noc.Flit
	emitReady    bool
	receivedTime akita.VTimeInSec
}

// DistMultiFlitChipletSwitch is an Akita component that forwards flits and
// performs branch-node transformation on DistributeRsp messages.
type DistMultiFlitChipletSwitch struct {
	*akita.TickingComponent

	ports                []akita.Port
	portToComplexMapping map[akita.Port]*multiFlitPortComplex
	routingTable         routing.Table
	arbiter              *arbitration.FlitAwareXBarArbiter

	flitByteSize     int
	encodingOverhead float64
	switchLatency    int
	numReqPerCycle   int

	row  int // wafer size
	col  int // wafer size
	y    int // switch position
	x    int // switch position
	swId int // chiplet id

	nextPortId int

	ToRDMAReqPort akita.Port

	branchAssemblies map[uint64]*distBranchAssembly
}

/*
- localPort: port from switch
- remotePort: port from another component
*/
func (s *DistMultiFlitChipletSwitch) addPort(
	pc *multiFlitPortComplex,
) {
	s.ports = append(s.ports, pc.localPort)
	s.portToComplexMapping[pc.localPort] = pc
	s.portToComplexMapping[pc.remotePort] = pc
	s.arbiter.AddBuffer(pc.forwardBuffer)

	s.routingTable.DefineRoute(pc.remotePort, pc.localPort)
}

// Tick runs the switch one cycle. Stages execute forward so a flit can
// traverse the switch in one cycle without backpressure.
func (s *DistMultiFlitChipletSwitch) Tick(now akita.VTimeInSec) bool {
	s.arbiter.Reset()
	s.nextPortId = 0

	madeProgress := false
	for i := 0; i < s.numReqPerCycle; i++ {
		// Reverse stage order (downstream-first) so a full pipeline can
		// slide an entire stage in one tick. branchEmit and recv both
		// feed routeBuffer; branchEmit runs before recv so leftover
		// multi-cast flits drain ahead of newly arrived flits, matching
		// the original recv→branchEmit relative priority.
		madeProgress = s.sendOut(now) || madeProgress
		madeProgress = s.forward(now) || madeProgress
		madeProgress = s.route(now) || madeProgress
		madeProgress = s.branchEmit(now) || madeProgress
		madeProgress = s.recv(now) || madeProgress
	}
	return madeProgress
}

// recv drains each port. Flits of a DistributeRsp targeting this switch as
// branch node are diverted into branchAssemblies. All other flits go to
// the port's routeBuffer.
func (s *DistMultiFlitChipletSwitch) recv(
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
			s.traceHop(complex, flit, now)
			s.logRecv(complex, flit, now)
			s.accumulateBranch(complex, flit, now)
			port.Retrieve(now)
			madeProgress = true
			continue
		}

		if !complex.routeBuffer.CanPush() {
			continue
		}
		s.traceHop(complex, flit, now)
		traceC2CFlit(s, complex, flit, now)
		s.logRecv(complex, flit, now)
		complex.routeBuffer.Push(flit)
		port.Retrieve(now)
		madeProgress = true
	}
	return madeProgress
}

func (s *DistMultiFlitChipletSwitch) isBranchFlit(flit *noc.Flit) bool {
	distRsp, ok := flit.Msg.(*protocol.DistributeRsp)
	if !ok {
		return false
	}
	return distRsp.BranchNodeId == s.swId
}

func (s *DistMultiFlitChipletSwitch) accumulateBranch(
	complex *multiFlitPortComplex,
	flit *noc.Flit,
	now akita.VTimeInSec,
) {
	id := flit.Msg.Meta().ID
	asm, ok := s.branchAssemblies[id]
	if !ok {
		asm = &distBranchAssembly{
			msg:          flit.Msg.(*protocol.DistributeRsp),
			numRequired:  flit.NumFlitInMsg,
			injectPort:   complex,
			receivedTime: now,
		}
		s.branchAssemblies[id] = asm
	}
	asm.numArrived++
}

// branchEmit transforms each completed DistributeRsp into one or two
// DataReadyRsp messages, re-splits them into flits, and drains those
// flits into the inject port's routeBuffer one slot at a time so we
// never overflow a small buffer.
func (s *DistMultiFlitChipletSwitch) branchEmit(
	now akita.VTimeInSec,
) (madeProgress bool) {
	for id, asm := range s.branchAssemblies {
		if asm.numArrived < asm.numRequired {
			continue
		}
		if !asm.emitReady {
			newMsgs := s.transformDistRsp(asm.msg, now)
			for _, m := range newMsgs {
				asm.pendingFlits = append(
					asm.pendingFlits, s.msgToFlits(m)...)
			}
			asm.emitReady = true
		}
		for len(asm.pendingFlits) > 0 &&
			asm.injectPort.routeBuffer.CanPush() {
			asm.injectPort.routeBuffer.Push(asm.pendingFlits[0])
			asm.pendingFlits = asm.pendingFlits[1:]
			madeProgress = true
		}
		if len(asm.pendingFlits) == 0 {
			delete(s.branchAssemblies, id)
		}
	}
	return madeProgress
}

func (s *DistMultiFlitChipletSwitch) transformDistRsp(
	distRsp *protocol.DistributeRsp,
	now akita.VTimeInSec,
) []akita.Msg {
	first := protocol.DataReadyRspBuilder{}.
		WithSendTime(now).
		WithSrc(distRsp.Meta().Src).
		WithDst(distRsp.FirstDst).
		WithRspTo(distRsp.GetRespondTo()).
		WithData(distRsp.Data).
		WithInfo(distRsp.Info).
		Build()
	if distRsp.SecondDst == nil || distRsp.SecondDst == distRsp.FirstDst {
		return []akita.Msg{first}
	}
	second := protocol.DataReadyRspBuilder{}.
		WithSendTime(now).
		WithSrc(distRsp.Meta().Src).
		WithDst(distRsp.SecondDst).
		WithRspTo(distRsp.GetRespondTo()).
		WithData(distRsp.Data).
		WithInfo(distRsp.Info).
		Build()
	return []akita.Msg{first, second}
}

func (s *DistMultiFlitChipletSwitch) msgToFlits(
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

// traceHop emits a HopTracer step on the head flit of a message arriving
// from another switch.
func (s *DistMultiFlitChipletSwitch) traceHop(
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

func (s *DistMultiFlitChipletSwitch) logRecv(
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

func (s *DistMultiFlitChipletSwitch) route(
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

func (s *DistMultiFlitChipletSwitch) assignFlitOutputBuf(f *noc.Flit) {
	outPort := s.routingTable.FindPort(f.Msg.Meta().Dst)
	complex := s.portToComplexMapping[outPort]
	f.OutputBuf = complex.sendOutBuffer
}

func (s *DistMultiFlitChipletSwitch) forward(
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

func (s *DistMultiFlitChipletSwitch) sendOut(
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

func (s *DistMultiFlitChipletSwitch) logSendOutFail(
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
 * Builder for DistMultiFlitChipletSwitch
 *
 */

// DistMultiFlitChipletSwitchBuilder can build DistMultiFlitChipletSwitchs
type DistMultiFlitChipletSwitchBuilder struct {
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

func MakeDistMultiFlitChipletSwitchBuilder() *DistMultiFlitChipletSwitchBuilder {
	b := DistMultiFlitChipletSwitchBuilder{}
	return &b
}

func (b *DistMultiFlitChipletSwitchBuilder) WithEngine(engine akita.Engine) *DistMultiFlitChipletSwitchBuilder {
	b.engine = engine
	return b
}

func (b *DistMultiFlitChipletSwitchBuilder) WithFreq(freq akita.Freq) *DistMultiFlitChipletSwitchBuilder {
	b.freq = freq
	return b
}

func (b *DistMultiFlitChipletSwitchBuilder) WithArbiter(arbiter arbitration.Arbiter) *DistMultiFlitChipletSwitchBuilder {
	b.arbiter = arbiter
	return b
}

func (b *DistMultiFlitChipletSwitchBuilder) WithRoutingTable(rt routing.Table) *DistMultiFlitChipletSwitchBuilder {
	b.routingTable = rt
	return b
}

func (b *DistMultiFlitChipletSwitchBuilder) WithNumReqPerCycle(numReqPerCycle int) *DistMultiFlitChipletSwitchBuilder {
	b.numReqPerCycle = numReqPerCycle
	return b
}

func (b *DistMultiFlitChipletSwitchBuilder) WithMaxOutgoingReqsPerChiplet(maxOutgoingReqsPerChiplet int) *DistMultiFlitChipletSwitchBuilder {
	return b
}

func (b *DistMultiFlitChipletSwitchBuilder) WithGridSize(r, c int) *DistMultiFlitChipletSwitchBuilder {
	b.row = r
	b.col = c
	return b
}

func (b *DistMultiFlitChipletSwitchBuilder) WithSwitchPosition(y, x int) *DistMultiFlitChipletSwitchBuilder {
	b.y = y
	b.x = x
	return b
}

func (b *DistMultiFlitChipletSwitchBuilder) WithSwitchLatency(sw int) *DistMultiFlitChipletSwitchBuilder {
	b.switchLatency = sw
	return b
}

func (b *DistMultiFlitChipletSwitchBuilder) WithFlitByteSize(flitByteSize int) *DistMultiFlitChipletSwitchBuilder {
	b.flitByteSize = flitByteSize
	return b
}

// Build creates a new DistMultiFlitChipletSwitch.
func (b DistMultiFlitChipletSwitchBuilder) Build(name string) *DistMultiFlitChipletSwitch {
	b.engineMustBeGiven()
	b.freqMustNotBeZero()
	b.routingTableMustBeGiven()
	b.arbiterMustBeGiven()

	s := &DistMultiFlitChipletSwitch{}
	s.TickingComponent = akita.NewTickingComponent(name, b.engine, b.freq, s)
	s.routingTable = b.routingTable
	s.arbiter = b.arbiter.(*arbitration.FlitAwareXBarArbiter)
	s.portToComplexMapping = make(map[akita.Port]*multiFlitPortComplex)
	s.branchAssemblies = make(map[uint64]*distBranchAssembly)
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

func (b DistMultiFlitChipletSwitchBuilder) engineMustBeGiven() {
	if b.engine == nil {
		panic("engine of DistMultiFlitChipletSwitch is not given")
	}
}

func (b DistMultiFlitChipletSwitchBuilder) freqMustNotBeZero() {
	if b.freq == 0 {
		panic("DistMultiFlitChipletSwitch frequency cannot be 0")
	}
}

func (b DistMultiFlitChipletSwitchBuilder) routingTableMustBeGiven() {
	if b.routingTable == nil {
		panic("DistMultiFlitChipletSwitch requires a routing table to operate")
	}
}

func (b DistMultiFlitChipletSwitchBuilder) arbiterMustBeGiven() {
	if b.arbiter == nil {
		panic("DistMultiFlitChipletSwitch requires an arbiter to operate")
	}
}
