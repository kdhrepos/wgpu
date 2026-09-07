package akita

type directConnectionEnd struct {
	port     Port
	buf      []Msg
	bufSize  int
	busy     bool
	dstCount map[Port]int
}

// DirectConnection connects two components without latency
type DirectConnection struct {
	*TickingComponent

	engine     Engine
	ports      []Port
	ends       map[Port]*directConnectionEnd
	nextPortID int
}

// PlugIn marks the port connects to this DirectConnection.
func (c *DirectConnection) PlugIn(port Port, sourceSideBufSize int) {

	c.ports = append(c.ports, port)
	end := &directConnectionEnd{}
	end.port = port
	end.bufSize = sourceSideBufSize
	end.dstCount = make(map[Port]int)
	c.ends[port] = end

	port.SetConnection(c)
}

// SrcBufLen reports the occupancy and capacity of the source-side buffer
// reserved for `port`. ok=false when the port is not plugged in. Read-only;
// used by deadlock reporting to see backpressure that Send() observes.
func (c *DirectConnection) SrcBufLen(port Port) (used, capacity int, ok bool) {
	end, found := c.ends[port]
	if !found || end == nil {
		return 0, 0, false
	}
	return len(end.buf), end.bufSize, true
}

// SrcBufDstCount returns how many messages already queued in src's source-side
// buffer are addressed to dst. O(1): maintained incrementally on Send/deliver.
// Read-only; lets a sender reserve dst capacity for in-flight messages.
func (c *DirectConnection) SrcBufDstCount(src, dst Port) (count int, ok bool) {
	end, found := c.ends[src]
	if !found || end == nil {
		return 0, false
	}
	return end.dstCount[dst], true
}

// Unplug marks the port no longer connects to this DirectConnection.
func (c *DirectConnection) Unplug(port Port) {
	// if port is not connected, panic
	if _, connected := c.ends[port]; !connected {
		panic("port is not connected")
	}

	// delete port from ports
	for i, p := range c.ports {
		if p == port {
			c.ports = append(c.ports[:i], c.ports[i+1:]...)
			break
		}
	}

	// delete end from ends
	delete(c.ends, port)

	// set port's connection to nil
	port.SetConnection(nil)
}

// NotifyAvailable is called by a port to notify that the connection can
// deliver to the port again.
func (c *DirectConnection) NotifyAvailable(now VTimeInSec, port Port) {
	c.TickNow(now)
}

// Send of a DirectConnection schedules a DeliveryEvent immediately
func (c *DirectConnection) Send(msg Msg) *SendError {

	c.msgMustBeValid(msg)

	srcEnd := c.ends[msg.Meta().Src]

	if len(srcEnd.buf) >= srcEnd.bufSize {

		// msg.Meta().Dst.NotifyAvailable(msg.Meta().SendTime)
		// c.TickNow(msg.Meta().SendTime)

		srcEnd.busy = true
		return NewSendError()
	}

	srcEnd.buf = append(srcEnd.buf, msg)
	srcEnd.dstCount[msg.Meta().Dst]++

	if len(srcEnd.buf) >= srcEnd.bufSize {
		srcEnd.busy = true
	}

	c.TickNow(msg.Meta().SendTime)

	return nil
}

func (c *DirectConnection) msgMustBeValid(msg Msg) {
	c.portMustNotBeNil(msg.Meta().Src)
	c.portMustNotBeNil(msg.Meta().Dst)
	c.portMustBeConnected(msg.Meta().Src)
	c.portMustBeConnected(msg.Meta().Dst)
	c.srcDstMustNotBeTheSame(msg)
}

func (c *DirectConnection) portMustNotBeNil(port Port) {
	if port == nil {
		panic("src or dst is not given")
	}
}

func (c *DirectConnection) portMustBeConnected(port Port) {
	if _, connected := c.ends[port]; !connected {
		panic("src or dst is not connected")
	}
}

func (c *DirectConnection) srcDstMustNotBeTheSame(msg Msg) {
	if msg.Meta().Src == msg.Meta().Dst {
		panic("sending back to src")
	}
}

func (c *DirectConnection) Tick(now VTimeInSec) bool {
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

func (c *DirectConnection) forwardMany(
	end *directConnectionEnd,
	now VTimeInSec,
) bool {
	madeProgress := false

	for {
		if len(end.buf) == 0 {
			break
		}

		head := end.buf[0]
		head.Meta().RecvTime = now

		err := head.Meta().Dst.Recv(head) // send a message to destination port
		if err != nil {
			// end.port.NotifyAvailable(now)
			// if strings.Contains(head.Meta().Dst.Name(), "L2") {
			// 	end.buf = append(end.buf[1:], head)
			// }
			break
		}

		madeProgress = true
		end.buf = end.buf[1:]
		end.dstCount[head.Meta().Dst]--

		if end.busy {
			end.port.NotifyAvailable(now)
			end.busy = false
		}
	}

	return madeProgress
}

func (c *DirectConnection) forwardOne(
	end *directConnectionEnd,
	now VTimeInSec,
) bool {
	madeProgress := false
	if len(end.buf) == 0 {
		return madeProgress
	}

	head := end.buf[0]
	head.Meta().RecvTime = now

	err := head.Meta().Dst.Recv(head)
	if err != nil {
		return madeProgress
	}

	madeProgress = true
	end.buf = end.buf[1:]
	end.dstCount[head.Meta().Dst]--

	if end.busy {
		end.port.NotifyAvailable(now)
		end.busy = false
	}

	return madeProgress
}

// NewDirectConnection creates a new DirectConnection object
func NewDirectConnection(
	name string,
	engine Engine,
	freq Freq,
) *DirectConnection {
	c := new(DirectConnection)
	c.TickingComponent = NewSecondaryTickingComponent(name, engine, freq, c)
	c.ends = make(map[Port]*directConnectionEnd)
	return c
}
