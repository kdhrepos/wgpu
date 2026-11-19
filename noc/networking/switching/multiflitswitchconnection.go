package switching

import (
	"fmt"
	"os"

	"gitlab.com/akita/akita"
	"gitlab.com/akita/noc"
	"gitlab.com/akita/protocol"
	"gitlab.com/akita/util"
)

// deadlockWatchdogThreshold: cycles of stalled drain on a single end before
// the connection declares a deadlock. 100k cycles ≈ 100µs at 1GHz — orders
// of magnitude above any reasonable transient stall (full DRAM queue, cache
// miss train, etc.). Anything past this is a structural cyclic dependency.
const deadlockWatchdogThreshold = uint64(100000)

type MultiFlitSwitchConnectionEnd struct {
	port akita.Port
	buf  util.Buffer
	busy bool

	admittedCycle uint64
	admittedCount int
	inFlight      int

	admittedTotal uint64

	// lastProgressCycle is the most recent cycle in which the watchdog
	// considers this end to have made progress: a successful drain to dst,
	// or a fresh admit that grew the in-flight set. When buf has flits but
	// this counter falls too far behind the current cycle, the connection
	// declares a deadlock.
	lastProgressCycle uint64
}

func newMultiFlitSwitchConnectionEnd(
	port akita.Port,
	bufSize int,
) *MultiFlitSwitchConnectionEnd {
	return &MultiFlitSwitchConnectionEnd{
		port: port,
		buf:  util.NewBuffer(bufSize),
	}
}

type MultiFlitSwitchConnection struct {
	*akita.TickingComponent

	leftPort  akita.Port
	leftEnd   *MultiFlitSwitchConnectionEnd
	rightPort akita.Port
	rightEnd  *MultiFlitSwitchConnectionEnd

	row int
	col int

	pipelineWidth  int
	pipelineStages int
}

type flitArrivalEvent struct {
	*akita.EventBase
	msg    akita.Msg
	srcEnd *MultiFlitSwitchConnectionEnd
}

func (c *MultiFlitSwitchConnection) PlugIn(
	port akita.Port,
	sourceSideBufSize int,
) {
	c.Lock()
	defer c.Unlock()
	_ = sourceSideBufSize
	bufCap := c.bufferCapacity()
	switch {
	case c.leftPort == nil:
		c.leftPort = port
		c.leftEnd = newMultiFlitSwitchConnectionEnd(port, bufCap)
	case c.rightPort == nil:
		c.rightPort = port
		c.rightEnd = newMultiFlitSwitchConnectionEnd(port, bufCap)
	default:
		panic("multiflit switch connection already has two ports")
	}
	port.SetConnection(c)
}

func (c *MultiFlitSwitchConnection) Unplug(port akita.Port) {
	panic("not implemented")
}

func (c *MultiFlitSwitchConnection) NotifyAvailable(
	now akita.VTimeInSec, port akita.Port,
) {
	c.TickNow(now)
}

func (c *MultiFlitSwitchConnection) Send(
	msg akita.Msg,
) *akita.SendError {
	c.Lock()
	defer c.Unlock()
	c.msgMustBeValid(msg)

	srcPort, payload := c.unwrapSendArgs(msg)
	srcEnd := c.endByPort(srcPort)
	if !c.tryAdmit(srcEnd, msg.Meta().SendTime) {
		srcEnd.busy = true
		return akita.NewSendError()
	}

	c.scheduleArrival(payload, srcEnd, msg.Meta().SendTime)
	return nil
}

func (c *MultiFlitSwitchConnection) unwrapSendArgs(
	msg akita.Msg,
) (akita.Port, akita.Msg) {
	if flit, ok := msg.(*noc.Flit); ok {
		return flit.Src, flit
	}
	if nm, ok := msg.(*protocol.NetworkMsg); ok {
		return nm.SrcPort, nm.Msg
	}
	panic("expected *noc.Flit or *protocol.NetworkMsg")
}

func (c *MultiFlitSwitchConnection) endByPort(
	p akita.Port,
) *MultiFlitSwitchConnectionEnd {
	switch p {
	case c.leftPort:
		return c.leftEnd
	case c.rightPort:
		return c.rightEnd
	}
	panic("source port unknown")
}

func (c *MultiFlitSwitchConnection) dstPortFor(
	end *MultiFlitSwitchConnectionEnd,
) akita.Port {
	if end == c.leftEnd {
		return c.rightPort
	}
	return c.leftPort
}

func (c *MultiFlitSwitchConnection) tryAdmit(
	end *MultiFlitSwitchConnectionEnd,
	now akita.VTimeInSec,
) bool {
	cycle := c.Freq.Cycle(now)
	if cycle != end.admittedCycle {
		end.admittedCycle = cycle
		end.admittedCount = 0
	}
	if end.admittedCount >= c.pipelineWidth {
		return false
	}
	if end.inFlight >= c.bufferCapacity() {
		return false
	}
	end.admittedCount++
	end.inFlight++
	end.admittedTotal++
	end.lastProgressCycle = cycle
	return true
}

func (c *MultiFlitSwitchConnection) LeftPortName() string {
	return c.leftPort.Name()
}

func (c *MultiFlitSwitchConnection) RightPortName() string {
	return c.rightPort.Name()
}

func (c *MultiFlitSwitchConnection) PipelineWidth() int {
	return c.pipelineWidth
}

func (c *MultiFlitSwitchConnection) AdmittedLeftToRight() uint64 {
	return c.leftEnd.admittedTotal
}

func (c *MultiFlitSwitchConnection) AdmittedRightToLeft() uint64 {
	return c.rightEnd.admittedTotal
}

func (c *MultiFlitSwitchConnection) Cycles(now akita.VTimeInSec) uint64 {
	return c.Freq.Cycle(now)
}

// LeftPort / RightPort expose the two ports for deadlock diagnostics (so the
// dumper can label which switches the connection sits between).
func (c *MultiFlitSwitchConnection) LeftPort() akita.Port  { return c.leftPort }
func (c *MultiFlitSwitchConnection) RightPort() akita.Port { return c.rightPort }

// DeadlockSummary reports per-end in-transit/queued flits and busy state for
// the quiesce-deadlock dumper. buf = flits that arrived but cannot drain into
// the destination port; inFlight = admitted flits still on the wire (in-buf
// plus in-transit). A non-empty value means flits are stuck on this wire and
// neither endpoint switch's local buffers will show them.
func (c *MultiFlitSwitchConnection) DeadlockSummary() (line string, anyPending bool) {
	endSummary := func(end *MultiFlitSwitchConnectionEnd) (bufN, inFlight int, busy bool) {
		if end == nil {
			return
		}
		if end.buf != nil {
			bufN = end.buf.Size()
		}
		return bufN, end.inFlight, end.busy
	}
	// dstRoom: can this end deliver its head flit to the downstream port right
	// now? buf>0 with dstRoom=true means the link holds undeliverable-looking
	// flits while the destination HAS room == lost wakeup on this link; with
	// dstRoom=false it is genuine backpressure.
	dstRoom := func(end *MultiFlitSwitchConnectionEnd) bool {
		if end == nil {
			return false
		}
		dst := c.dstPortFor(end)
		if cs, ok := dst.(interface{ CanSend(int) bool }); ok {
			return cs.CanSend(1)
		}
		return false
	}
	lBuf, lIF, lBusy := endSummary(c.leftEnd)
	rBuf, rIF, rBusy := endSummary(c.rightEnd)
	lRoom, rRoom := dstRoom(c.leftEnd), dstRoom(c.rightEnd)
	anyPending = lBuf+rBuf+lIF+rIF > 0 || lBusy || rBusy
	verdict := ""
	if lBuf > 0 && lRoom {
		verdict += " [LOST-WAKEUP left: buf but dst has room]"
	}
	if rBuf > 0 && rRoom {
		verdict += " [LOST-WAKEUP right: buf but dst has room]"
	}
	line = fmt.Sprintf(
		"left=buf=%d inflight=%d busy=%v dstRoom=%v | "+
			"right=buf=%d inflight=%d busy=%v dstRoom=%v | pipe=%dx%d%s",
		lBuf, lIF, lBusy, lRoom, rBuf, rIF, rBusy, rRoom,
		c.pipelineWidth, c.pipelineStages, verdict)
	return
}

func (c *MultiFlitSwitchConnection) bufferCapacity() int {
	cap := int(float64(c.pipelineWidth * c.pipelineStages))
	if cap < 1 { // device↔switch links (pipelineStages == 0)
		cap = c.pipelineWidth
	}
	return cap
}

func (c *MultiFlitSwitchConnection) scheduleArrival(
	payload akita.Msg,
	srcEnd *MultiFlitSwitchConnectionEnd,
	sendTime akita.VTimeInSec,
) {
	arrivalTime := c.Freq.NCyclesLater(
		c.pipelineStages, sendTime)

	evt := &flitArrivalEvent{msg: payload, srcEnd: srcEnd}
	evt.EventBase = akita.NewEventBase(arrivalTime, c)
	c.Engine.Schedule(evt)
}

func (c *MultiFlitSwitchConnection) Handle(e akita.Event) error {
	if evt, ok := e.(*flitArrivalEvent); ok {
		return c.handleArrival(evt)
	}
	return c.TickingComponent.Handle(e)
}

func (c *MultiFlitSwitchConnection) handleArrival(
	evt *flitArrivalEvent,
) error {
	c.Lock()
	defer c.Unlock()
	evt.srcEnd.buf.Push(evt.msg)
	c.drainEnd(evt.srcEnd, evt.Time())
	return nil
}

func (c *MultiFlitSwitchConnection) Tick(
	now akita.VTimeInSec,
) bool {
	madeProgress := false
	c.Lock()
	defer c.Unlock()
	madeProgress = c.drainEnd(c.leftEnd, now) || madeProgress
	madeProgress = c.drainEnd(c.rightEnd, now) || madeProgress
	madeProgress = c.notifyIfBusyReady(c.leftEnd, now) ||
		madeProgress
	madeProgress = c.notifyIfBusyReady(c.rightEnd, now) ||
		madeProgress
	// c.checkDeadlock(c.leftEnd, now)
	// c.checkDeadlock(c.rightEnd, now)
	return madeProgress
}

// checkDeadlock is the network-level watchdog. When an end's buf has held
// flits past the threshold without any drain progress, it drops the head
// flit to release the connection-level stall. This is best-effort: the
// downstream component awaiting the dropped flit will still hang at the
// protocol layer, but other connections caught in the same dependency
// cycle may now drain. Drop events are logged loudly so the user can see
// the structural cycle in the trace and fix the root cause.
func (c *MultiFlitSwitchConnection) checkDeadlock(
	end *MultiFlitSwitchConnectionEnd,
	now akita.VTimeInSec,
) {
	if end.buf.Peek() == nil {
		return
	}
	cycle := c.Freq.Cycle(now)
	if cycle-end.lastProgressCycle < deadlockWatchdogThreshold {
		return
	}
	head := end.buf.Peek().(akita.Msg)
	dst := c.dstPortFor(end)
	fmt.Fprintf(os.Stderr,
		"[noc] deadlock-watchdog drop: connection=%s src-port=%s "+
			"dst-port=%s stuck=%d cycles buf=%d inFlight=%d "+
			"head{id=%d src=%s dst=%s type=%T}\n",
		c.Name(), end.port.Name(), dst.Name(),
		cycle-end.lastProgressCycle,
		end.buf.Size(), end.inFlight,
		head.Meta().ID, head.Meta().Src.Name(),
		head.Meta().Dst.Name(), head)
	end.buf.Pop()
	end.inFlight--
	end.lastProgressCycle = cycle
	if end.busy {
		end.port.NotifyAvailable(now)
		end.busy = false
	}
}

func (c *MultiFlitSwitchConnection) drainEnd(
	end *MultiFlitSwitchConnectionEnd,
	now akita.VTimeInSec,
) bool {
	madeProgress := false
	dst := c.dstPortFor(end)
	for i := 0; i < c.pipelineWidth; i++ {
		head := end.buf.Peek()
		if head == nil {
			break
		}
		msg := head.(akita.Msg)
		msg.Meta().RecvTime = now
		if err := dst.Recv(msg); err != nil {
			// dst.NotifyAvailable(now) // DO NOT DELETE FOR DEADLOCK AVOIDANCE
			break
		}
		end.buf.Pop()
		end.inFlight--
		end.lastProgressCycle = c.Freq.Cycle(now)
		if end.busy {
			end.port.NotifyAvailable(now)
			end.busy = false
		}
		madeProgress = true
	}
	return madeProgress
}

func (c *MultiFlitSwitchConnection) notifyIfBusyReady(
	end *MultiFlitSwitchConnectionEnd,
	now akita.VTimeInSec,
) bool {
	if !end.busy {
		return false
	}
	cycle := c.Freq.Cycle(now)
	cycleSaturated := cycle == end.admittedCycle &&
		end.admittedCount >= c.pipelineWidth
	inFlightSaturated := end.inFlight >= c.bufferCapacity()
	if cycleSaturated || inFlightSaturated {
		return false
	}
	end.port.NotifyAvailable(now)
	end.busy = false
	return true
}

func (c *MultiFlitSwitchConnection) msgMustBeValid(msg akita.Msg) {
	if msg.Meta().Src == nil {
		panic("src is not given")
	}
	if msg.Meta().Dst == nil {
		panic("dst is not given")
	}
}

// NewMultiFlitSwitchConnection creates a new MultiFlitSwitchConnection.
// pipelineStages is interpreted as the link latency in cycles; an arriving
// flit is delivered exactly that many cycles after its SendTime. pipelineWidth
// is the maximum number of flits admitted per cycle on each direction.
func NewMultiFlitSwitchConnection(
	name string,
	engine akita.Engine,
	freq akita.Freq,
	pipelineWidth int,
	pipelineStages int,
	row, col int,
) *MultiFlitSwitchConnection {
	c := new(MultiFlitSwitchConnection)
	c.TickingComponent = akita.NewSecondaryTickingComponent(
		name, engine, freq, c)
	c.Engine = engine
	c.Freq = freq
	c.row = row
	c.col = col
	c.pipelineWidth = pipelineWidth
	c.pipelineStages = pipelineStages
	return c
}
