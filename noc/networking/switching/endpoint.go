package switching

import (
	"fmt"
	"math"

	"gitlab.com/akita/akita"
	"gitlab.com/akita/noc"
	"gitlab.com/akita/protocol"
	"gitlab.com/akita/util/global"
	"gitlab.com/akita/util/tracing"
)

// partialAssembly tracks one in-progress msg whose flits are still
// arriving. The endpoint indexes these by msg.ID so flits for different
// msgs do not occupy each other's buffer slots — the per-msg map is what
// breaks the original flat-slice HOL deadlock during a flush burst.
type partialAssembly struct {
	msg         akita.Msg
	numArrived  int
	numRequired int
}

// EndPoint is an akita component that deligates sending and receiving actions
// of a few ports.
type EndPoint struct {
	*akita.TickingComponent

	DevicePorts      []akita.Port
	NetworkPort      akita.Port
	DefaultSwitchDst akita.Port

	flitByteSize             int
	encodingOverhead         float64
	msgOutBuf                []akita.Msg
	msgOutBufSize            int
	flitsToSend              []*noc.Flit
	assemblingMsgs           map[uint64]*partialAssembly
	readyMsgs                []akita.Msg
	flitAssemblingBufferSize int
	numReqPerCycle           int

	// flightStarted dedups NetworkFlightTracer StartTask emissions per
	// msg.ID. Send may be invoked many times for the same msg under
	// msgOutBuf backpressure; only the first call must mark the flight's
	// start so the measurement captures the admission-stall the RDMA
	// engine experiences. Entry is dropped on admission success.
	flightStarted map[uint64]bool

	// DeliverHook lets a subclass intercept fully-assembled msgs before
	// they are handed to msg.Meta().Dst.Recv. Return (handled, ok):
	//   handled=true  → the hook owns this msg type and decides outcome.
	//                    ok=true  → msg consumed; tryDeliver pops it.
	//                    ok=false → resource not ready; tryDeliver breaks
	//                               and retries next tick (msg stays).
	//   handled=false → fall through to the default Recv path.
	// Default nil keeps vanilla EndPoint behavior.
	DeliverHook func(msg akita.Msg, now akita.VTimeInSec) (handled bool, ok bool)
}

// Send sends a message.
func (ep *EndPoint) Send(msg akita.Msg) *akita.SendError {
	ep.Lock()
	defer ep.Unlock()

	// Flight tracing starts on the FIRST Send attempt for this msg.ID,
	// before the admission check below — so the time spent waiting for
	// msgOutBuf room (the real source-side backpressure) is included in
	// the measured flight. Dedup via flightStarted: retries on the same
	// msg must not reset startTime. Entry is released on admission
	// success since the RDMA engine will not Send the same msg again.
	traced := msg
	if nm, ok := msg.(*protocol.NetworkMsg); ok {
		traced = nm.Msg
	}
	tracedID := traced.Meta().ID
	kind, kindOk := flightKindForMsg(traced)
	if kindOk && !ep.flightStarted[tracedID] {
		srcChip, dstChip := chipletPairFromMsg(traced)
		tracing.StartTask(
			tracing.NetworkFlightTaskID(tracedID, kind),
			0, msg.Meta().SendTime, ep, kind, "",
			srcChip+"."+dstChip)
		ep.flightStarted[tracedID] = true
	}

	if len(ep.msgOutBuf) >= ep.msgOutBufSize {
		return &akita.SendError{}
	}

	ep.msgOutBuf = append(ep.msgOutBuf, msg)
	ep.TickLater(msg.Meta().SendTime)

	if kindOk {
		delete(ep.flightStarted, tracedID)
	}

	return nil
}

// PlugIn connects a port to the endpoint.
func (ep *EndPoint) PlugIn(port akita.Port, srcBufCap int) {

	port.SetConnection(ep)
	ep.DevicePorts = append(ep.DevicePorts, port)
	ep.msgOutBufSize = srcBufCap
}

// NotifyAvailable triggers the endpoint to continue to tick.
func (ep *EndPoint) NotifyAvailable(now akita.VTimeInSec, port akita.Port) {
	ep.TickLater(now)
}

// Unplug removes the association of a port and an endpoint.
func (ep *EndPoint) Unplug(port akita.Port) {
	panic("not implemented")
}

// Tick update the endpoint state.
func (ep *EndPoint) Tick(now akita.VTimeInSec) bool {
	ep.Lock()
	defer ep.Unlock()

	madeProgress := false

	for i := 0; i < ep.numReqPerCycle; i++ {
		// tryDeliver first so the device-side port (e.g. RDMA's
		// RequestPort/PtwReqPort) receives any assembled msg as early as
		// possible — Recv triggers NotifyRecv which wakes the device for
		// the same tick, letting RDMA drain inbound msgs in the same
		// cycle. sendFlitOut → prepareFlits keeps the egress sliding so
		// a freed flitsToSend slot is immediately refilled with the next
		// msg. recv runs last; new flits arriving this tick are delivered
		// on the next tick. Trade-off: ingress sliding (recv → tryDeliver)
		// costs one cycle, accepted because device drain is the
		// system-wide throughput bottleneck.
		madeProgress = ep.tryDeliver(now) || madeProgress
		madeProgress = ep.sendFlitOut(now) || madeProgress
		madeProgress = ep.prepareFlits(now) || madeProgress
		madeProgress = ep.recv(now) || madeProgress
	}

	return madeProgress
}

// DeadlockSummary reports the endpoint's egress/ingress occupancy for the
// driver's quiesce dump. msgOut/flits = device→network egress; assembling/ready
// = network→device ingress; netQ = a flit waiting on the network port.
func (ep *EndPoint) DeadlockSummary() (line string, anyPending bool) {
	ep.Lock()
	defer ep.Unlock()
	outN := len(ep.msgOutBuf)
	flitN := len(ep.flitsToSend)
	asmN := len(ep.assemblingMsgs)
	readyN := len(ep.readyMsgs)
	netQ := ep.NetworkPort != nil && ep.NetworkPort.Peek() != nil

	// Reassembly progress: a partial msg with 0<arrived<required whose remaining
	// flits never come == wormhole/HoL jam upstream in the switch network. Sum
	// arrived/required across in-progress msgs and count stuck partials.
	partialStuck, arrived, required := 0, 0, 0
	for _, pa := range ep.assemblingMsgs {
		arrived += pa.numArrived
		required += pa.numRequired
		if pa.numArrived > 0 && pa.numArrived < pa.numRequired {
			partialStuck++
		}
	}
	// Can an assembled (ready) msg be delivered to a device port right now?
	// ready>0 with devCanSend=true == lost wakeup at this endpoint;
	// devCanSend=false == device-side backpressure.
	devRoom := false
	for _, p := range ep.DevicePorts {
		if cs, ok := p.(interface{ CanSend(int) bool }); ok && cs.CanSend(1) {
			devRoom = true
			break
		}
	}

	anyPending = outN > 0 || flitN > 0 || asmN > 0 || readyN > 0 || netQ
	line = fmt.Sprintf(
		"msgOut=%d flits=%d assembling=%d(stuck=%d arrived=%d/req=%d) "+
			"ready=%d(devCanSend=%v) netQ=%v",
		outN, flitN, asmN, partialStuck, arrived, required,
		readyN, devRoom, netQ)
	return
}

func (ep *EndPoint) sendFlitOut(now akita.VTimeInSec) bool {
	if len(ep.flitsToSend) == 0 {
		return false
	}

	ep.flitsToSend[0].SendTime = now
	err := ep.NetworkPort.Send(ep.flitsToSend[0])
	if err == nil {
		ep.flitsToSend = ep.flitsToSend[1:]
		if len(ep.flitsToSend) == 0 {
			for _, p := range ep.DevicePorts {
				p.NotifyAvailable(now)
			}
		}
		return true
	}
	return false
}

func (ep *EndPoint) prepareFlits(now akita.VTimeInSec) bool {
	if len(ep.flitsToSend) > 0 {
		return false
	}

	if len(ep.msgOutBuf) == 0 {
		return false
	}

	msg := ep.msgOutBuf[0]
	ep.msgOutBuf = ep.msgOutBuf[1:]

	if len(ep.msgOutBuf)+1 >= ep.msgOutBufSize {
		for _, p := range ep.DevicePorts {
			p.NotifyAvailable(now)
		}
	}

	// Unwrap the protocol.NetworkMsg envelope (used by RDMA engines as a
	// legacy carrier of SrcPort). The network operates on the inner msg so
	// that the receiving RDMA engine sees the same payload it would have
	// seen in the pre-wormhole pipeline (which always unwrapped at the
	// connection boundary before delivery).
	if nm, ok := msg.(*protocol.NetworkMsg); ok {
		msg = nm.Msg
	}

	ep.flitsToSend = ep.msgToFlits(msg)
	return true
}

// recv accepts at most one flit per call from the NetworkPort. The cap
// counts distinct in-progress msg IDs, not flits — so once a msg has
// started arriving, the remaining flits of that msg are admitted even if
// the map is at capacity. This prevents the head-of-line deadlock that
// the original flat-slice buffer suffered under flush bursts.
func (ep *EndPoint) recv(now akita.VTimeInSec) bool {
	recved := ep.NetworkPort.Peek()
	if recved == nil {
		return false
	}
	flit := recved.(*noc.Flit)
	id := flit.Msg.Meta().ID

	pa, exists := ep.assemblingMsgs[id]
	if !exists {
		if len(ep.assemblingMsgs) >= ep.flitAssemblingBufferSize {
			return false
		}
		pa = &partialAssembly{
			msg:         flit.Msg,
			numRequired: flit.NumFlitInMsg,
		}
		ep.assemblingMsgs[id] = pa
	}
	pa.numArrived++
	ep.NetworkPort.Retrieve(now)

	if pa.numArrived >= pa.numRequired {
		ep.readyMsgs = append(ep.readyMsgs, pa.msg)
		delete(ep.assemblingMsgs, id)
	}
	return true
}

// tryDeliver drains assembled msgs to their device-side destinations.
// Order is preserved per destination: once a destination Recv fails, later
// msgs to that same destination are kept for retry. A DeliverHook msg (e.g.
// DynamicClusteringEndPoint fan-out) that cannot re-inject into msgOutBuf is
// kept and skipped rather than head-of-line-blocking terminal msgs bound for
// the local device — draining those frees the MSHR / network-egress room the
// blocked fan-out is itself waiting on.
func (ep *EndPoint) tryDeliver(now akita.VTimeInSec) bool {
	madeProgress := false
	writeIdx := 0
	var blockedDsts []akita.Port

	for _, msg := range ep.readyMsgs {
		msg.Meta().RecvTime = now

		if ep.DeliverHook != nil {
			handled, ok := ep.DeliverHook(msg, now)
			if handled {
				if ok {
					madeProgress = true
					continue
				}
				ep.readyMsgs[writeIdx] = msg
				writeIdx++
				continue
			}
		}

		dst := msg.Meta().Dst
		if portInList(blockedDsts, dst) {
			ep.readyMsgs[writeIdx] = msg
			writeIdx++
			continue
		}

		if err := dst.Recv(msg); err != nil {
			ep.logDeliverFail(msg, now)
			blockedDsts = append(blockedDsts, dst)
			ep.readyMsgs[writeIdx] = msg
			writeIdx++
			continue
		}
		// Flight tracing ends here: the assembled msg has just been
		// accepted by the destination RDMA engine's port. Pairs with
		// the StartTask emitted on the source side in EndPoint.Send.
		if kind, ok := flightKindForMsg(msg); ok {
			tracing.EndTask(
				tracing.NetworkFlightTaskID(msg.Meta().ID, kind), now, ep)
		}
		madeProgress = true
	}

	ep.readyMsgs = ep.readyMsgs[:writeIdx]
	return madeProgress
}

func portInList(list []akita.Port, p akita.Port) bool {
	for _, q := range list {
		if q == p {
			return true
		}
	}
	return false
}

// logDeliverFail prints a verbose debug line when the endpoint cannot
// hand an assembled message to its device-side destination (RDMA engine
// port full). The message stays at the head of readyMsgs for retry.
func (ep *EndPoint) logDeliverFail(msg akita.Msg, now akita.VTimeInSec) {
	if !global.GetInstance().GetVerbose() {
		return
	}
	fmt.Printf(
		"Error: deliver fail, %s, msg %d -> %s, at %.10f\n",
		ep.Name(),
		msg.Meta().ID,
		msg.Meta().Dst.Name(),
		now,
	)
}

func (ep *EndPoint) msgToFlits(msg akita.Msg) []*noc.Flit {
	numFlit := 1
	if msg.Meta().TrafficBytes > 0 {
		trafficByte := msg.Meta().TrafficBytes
		trafficByte += int(math.Ceil(
			float64(trafficByte) * ep.encodingOverhead))
		numFlit = (trafficByte-1)/ep.flitByteSize + 1
	}

	// Record the flit count for this unique packet so the NumFlitTracer can
	// aggregate it (kind="num-flits", Detail=numFlit as uint64).
	tracing.StartTask(
		msg.Meta().ID, 0, 0,
		ep, "num-flits", "", uint64(numFlit))

	flits := make([]*noc.Flit, numFlit)
	for i := 0; i < numFlit; i++ {
		flits[i] = noc.FlitBuilder{}.
			WithSrc(ep.NetworkPort).
			WithDst(ep.DefaultSwitchDst).
			WithSeqID(i).
			WithNumFlitInMsg(numFlit).
			WithMsg(msg).
			Build()
	}
	return flits
}

// EndPointBuilder can build End Points.
type EndPointBuilder struct {
	engine                   akita.Engine
	freq                     akita.Freq
	flitByteSize             int
	encodingOverhead         float64
	flitAssemblingBufferSize int
	networkPortBufferSize    int
	devicePorts              []akita.Port
	numReqPerCycle           int
}

// MakeEndPointBuilder creates a new EndPointBuilder with default
// configureations.
func MakeEndPointBuilder() EndPointBuilder {

	return EndPointBuilder{
		flitByteSize:             16,
		flitAssemblingBufferSize: 64,
		networkPortBufferSize:    4,
		freq:                     1 * akita.GHz,
		numReqPerCycle:           1,
	}
}

// WithEngine sets the engine of the End Point to build.
func (b EndPointBuilder) WithEngine(e akita.Engine) EndPointBuilder {
	b.engine = e
	return b
}

// WithFreq sets the frequency of the End Point to built.
func (b EndPointBuilder) WithFreq(freq akita.Freq) EndPointBuilder {
	b.freq = freq
	return b
}

// WithFlitByteSize sets the flit byte size that the End Point supports.
func (b EndPointBuilder) WithFlitByteSize(n int) EndPointBuilder {
	b.flitByteSize = n
	return b
}

// WithFreq sets the frequency of the End Point to built.
func (b EndPointBuilder) WithNumReqPerCycle(numReqPerCycle int) EndPointBuilder {
	b.numReqPerCycle = numReqPerCycle
	return b
}

// WithEncodingOverhead sets the encoding overhead.
func (b EndPointBuilder) WithEncodingOverhead(o float64) EndPointBuilder {
	b.encodingOverhead = o
	return b
}

// WithNetworkPortBufferSize sets the network port buffer size of the end point.
func (b EndPointBuilder) WithNetworkPortBufferSize(n int) EndPointBuilder {
	b.networkPortBufferSize = n
	return b
}

// WithDevicePorts sets a list of ports that communicate directly through the
// End Point.
func (b EndPointBuilder) WithDevicePorts(ports []akita.Port) EndPointBuilder {
	b.devicePorts = ports
	return b
}

// Build creates a new End Point.
func (b EndPointBuilder) Build(name string) *EndPoint {

	b.engineMustBeGiven()
	b.freqMustBeGiven()
	b.flitByteSizeMustBeGiven()

	ep := &EndPoint{}
	ep.TickingComponent = akita.NewTickingComponent(
		name, b.engine, b.freq, ep)
	ep.flitByteSize = b.flitByteSize
	ep.flitAssemblingBufferSize = b.flitAssemblingBufferSize
	ep.assemblingMsgs = make(map[uint64]*partialAssembly)
	ep.flightStarted = make(map[uint64]bool)
	ep.numReqPerCycle = b.numReqPerCycle
	ep.NetworkPort = akita.NewLimitNumMsgPort(
		ep, b.networkPortBufferSize,
		fmt.Sprintf("%s.network_port", ep.Name()))

	for _, dp := range b.devicePorts {
		ep.PlugIn(dp, 2*b.numReqPerCycle)
	}

	return ep
}

func (b EndPointBuilder) engineMustBeGiven() {
	if b.engine == nil {
		panic("engine is not given")
	}
}

func (b EndPointBuilder) freqMustBeGiven() {
	if b.freq == 0 {
		panic("freq must be given")
	}
}

func (b EndPointBuilder) flitByteSizeMustBeGiven() {
	if b.flitByteSize == 0 {
		panic("flit byte size must be given")
	}
}
