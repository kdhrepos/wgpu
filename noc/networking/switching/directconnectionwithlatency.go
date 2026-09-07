package switching

import (
	"strings"

	"gitlab.com/akita/akita"
	"gitlab.com/akita/protocol"
	"gitlab.com/akita/util"
	"gitlab.com/akita/util/pipelining"
)

type directConnectionWithLatencyEnd struct {
	port     akita.Port
	buf      util.Buffer
	bufSize  int
	busy     bool
	pipeline pipelining.Pipeline
}

// DirectConnectionWithLatency connects two components without latency
type DirectConnectionWithLatency struct {
	*akita.TickingComponent

	engine         akita.Engine
	nextPortID     int
	ports          []akita.Port
	ends           map[akita.Port]*directConnectionWithLatencyEnd
	pipelineWidth  int
	pipelineStages int
}

// PlugIn marks the port connects to this DirectConnection.
func (c *DirectConnectionWithLatency) PlugIn(
	port akita.Port,
	sourceSideBufSize int,
) {
	c.Lock()
	defer c.Unlock()

	c.ports = append(c.ports, port)
	end := &directConnectionWithLatencyEnd{}
	end.port = port
	end.buf = util.NewBuffer(sourceSideBufSize)
	// end.bufSize = sourceSideBufSize
	c.ends[port] = end

	end.pipeline = pipelining.
		MakeBuilder().
		WithNumStage(c.pipelineStages).
		WithCyclePerStage(1).
		WithPipelineWidth(c.pipelineWidth).
		WithPostPipelineBuffer(end.buf).
		Build(c.Name() + "_pipeline")

	port.SetConnection(c)
}

// Unplug marks the port no longer connects to this DirectConnectionWithLatency.
func (c *DirectConnectionWithLatency) Unplug(port akita.Port) {
	panic("not implemented")
}

// NotifyAvailable is called by a port to notify that the connection can
// deliver to the port again.
func (c *DirectConnectionWithLatency) NotifyAvailable(now akita.VTimeInSec, port akita.Port) {
	c.TickNow(now)
}

type connectionPipelineItem struct {
	taskID uint64
	msg    akita.Msg
}

func (f connectionPipelineItem) TaskID() uint64 {
	return f.taskID
}

// Send of a DirectConnectionWithLatency schedules a DeliveryEvent immediately
func (c *DirectConnectionWithLatency) Send(msg akita.Msg) *akita.SendError {
	c.Lock()
	defer c.Unlock()

	c.msgMustBeValid(msg)

	if m, ok := msg.(*protocol.NetworkMsg); ok {
		msg = m.Msg
	}

	srcEnd := c.ends[msg.Meta().Src]

	if !srcEnd.pipeline.CanAccept() {
		srcEnd.busy = true

		// msg.Meta().Dst.NotifyAvailable(msg.Meta().SendTime)
		// c.TickNow(msg.Meta().SendTime)

		return akita.NewSendError()
	}

	pipelineItem := connectionPipelineItem{
		taskID: akita.GetIDGenerator().Generate(),
		msg:    msg,
	}

	srcEnd.pipeline.Accept(msg.Meta().SendTime, pipelineItem)

	c.TickNow(msg.Meta().SendTime)

	return nil
}

func (c *DirectConnectionWithLatency) msgMustBeValid(msg akita.Msg) {
	c.portMustNotBeNil(msg.Meta().Src)
	c.portMustNotBeNil(msg.Meta().Dst)
	c.portMustBeConnected(msg.Meta().Src)
	c.portMustBeConnected(msg.Meta().Dst)
	c.srcDstMustNotBeTheSame(msg)
}

func (c *DirectConnectionWithLatency) portMustNotBeNil(port akita.Port) {
	if port == nil {
		panic("src or dst is not given")
	}
}

func (c *DirectConnectionWithLatency) portMustBeConnected(port akita.Port) {
	if _, connected := c.ends[port]; !connected {
		panic("src or dst is not connected")
	}
}

func (c *DirectConnectionWithLatency) srcDstMustNotBeTheSame(msg akita.Msg) {
	if msg.Meta().Src == msg.Meta().Dst && !strings.Contains(msg.Meta().Src.Name(), "RTU") {
		panic("sending back to src")
	}
}

func (c *DirectConnectionWithLatency) Tick(now akita.VTimeInSec) bool {
	madeProgress := false

	madeProgress = c.movePipeline(now) || madeProgress
	madeProgress = c.forwardMsgs(now) || madeProgress

	return madeProgress
}

func (c *DirectConnectionWithLatency) movePipeline(now akita.VTimeInSec) bool {
	madeProgress := false

	for _, end := range c.ends {
		madeProgress = end.pipeline.Tick(now) || madeProgress
	}

	return madeProgress
}

func (c *DirectConnectionWithLatency) forwardMsgs(now akita.VTimeInSec) bool {
	madeProgress := false
	for i := 0; i < len(c.ports); i++ {
		portID := (i + c.nextPortID) % len(c.ports)
		port := c.ports[portID]
		end := c.ends[port]
		madeProgress = c.forwardMany(end, now) || madeProgress
	}
	c.nextPortID = (c.nextPortID + 1) % len(c.ports)
	return madeProgress
}

func (c *DirectConnectionWithLatency) forwardMany(
	end *directConnectionWithLatencyEnd,
	now akita.VTimeInSec,
) bool {
	madeProgress := false
	for {
		if end.buf.Peek() == nil {
			break
		}

		pipelineItem := end.buf.Peek().(connectionPipelineItem)
		head := pipelineItem.msg
		head.Meta().RecvTime = now

		err := head.Meta().Dst.Recv(head) // send a message to destination port
		if err != nil {
			break
		}

		madeProgress = true
		end.buf.Pop()

		if end.busy {
			end.port.NotifyAvailable(now)
			end.busy = false
		}
	}

	return madeProgress
}

// func (c *DirectConnectionWithLatency) forwardOne(
// 	end *directConnectionWithLatencyEnd,
// 	now akita.VTimeInSec,
// ) bool {
// 	madeProgress := false
// 	if len(end.buf) == 0 {
// 		return madeProgress
// 	}

// 	head := end.buf[0]
// 	head.Meta().RecvTime = now

// 	err := head.Meta().Dst.Recv(head)
// 	if err != nil {
// 		return madeProgress
// 	}

// 	madeProgress = true
// 	end.buf = end.buf[1:]

// 	if end.busy {
// 		end.port.NotifyAvailable(now)
// 		end.busy = false
// 	}

// 	return madeProgress
// }

// NewDirectConnection creates a new DirectConnection object
func NewDirectConnectionWithLatency(
	name string,
	engine akita.Engine,
	freq akita.Freq,
	pipelineWidth int,
	pipelineStages int,
) *DirectConnectionWithLatency {
	c := new(DirectConnectionWithLatency)
	c.TickingComponent = akita.NewSecondaryTickingComponent(name, engine, freq, c)
	c.ends = make(map[akita.Port]*directConnectionWithLatencyEnd)

	c.pipelineWidth = pipelineWidth
	c.pipelineStages = pipelineStages
	return c
}
