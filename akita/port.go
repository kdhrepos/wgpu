package akita

import (
	// "fmt"

	"log"
	// "strings"
)

// A Port is owned by a component and is used to plugin connections
type Port interface {
	// Embed interface
	Named
	Hookable

	SetConnection(conn Connection)
	Component() Component

	// For connection
	Recv(msg Msg) *SendError
	NotifyAvailable(now VTimeInSec)

	// For component
	Send(msg Msg) *SendError
	Retrieve(now VTimeInSec) Msg
	Peek() Msg
}

// A Port is owned by a component and is used to plugin connections
type MsgBufferContainer interface {
	GetBuffer() []Msg
}

// PortEndSimulationChecker checks if the port buffer is empty at the end of
// the simulation. If the port is not empty, there is something wrong in the
// simulation.
type PortEndSimulationChecker struct {
	Port Port
}

// Handle checks if the port is empty or not.
func (c *PortEndSimulationChecker) Handle(e Event) error {
	if c.Port.Peek() != nil {
		log.Panic("port is not free")
	}
	return nil
}

// LimitNumMsgPort is a type of port that can hold at most a certain number
// of messages.
type LimitNumMsgPort struct {
	HookableBase

	name string
	comp Component
	conn Connection

	buf         []Msg
	bufCapacity int
	portBusy    bool
}

// HookPosPortMsgRecvd marks when an inbound message arrives at a the given port
var HookPosPortMsgRecvd = &HookPos{Name: "Port Msg Recv"}

// HookPosPortMsgRetrieve marks when an outbound message is sent over a connection
var HookPosPortMsgRetrieve = &HookPos{Name: "Port Msg  Retrieve"}

// SetConnection sets which connection plugged in to this port.
func (p *LimitNumMsgPort) SetConnection(conn Connection) {
	p.conn = conn
}

// Component returns the owner component of the port.
func (p *LimitNumMsgPort) Component() Component {
	return p.comp
}

// Name returns the name of the port.
func (p *LimitNumMsgPort) Name() string {
	return p.name
}

// Send is used to send a message out from a component
func (p *LimitNumMsgPort) Send(msg Msg) *SendError {
	err := p.conn.Send(msg)
	return err
}

// Recv is used to deliver a message to a component
func (p *LimitNumMsgPort) Recv(msg Msg) *SendError {

	if len(p.buf) >= p.bufCapacity {
		p.portBusy = true
		return NewSendError()
	}

	hookCtx := HookCtx{
		Domain: p,
		Now:    msg.Meta().RecvTime,
		Pos:    HookPosPortMsgRecvd,
		Item:   msg,
	}
	p.InvokeHook(hookCtx)

	p.buf = append(p.buf, msg)

	// Pre-arm portBusy when this Recv just filled the buffer. The next
	// Retrieve/RetrieveAt will then unconditionally fire
	// conn.NotifyAvailable, ensuring the connection's Tick is
	// rescheduled even if no Recv ever returns a SendError. This
	// closes the missed-wake-up window where the buf transitions
	// gradually full → has-room → full again, but no Recv ever
	// observes a full-and-rejected state to set portBusy.
	if len(p.buf) >= p.bufCapacity {
		p.portBusy = true
	}

	if p.comp != nil {
		p.comp.NotifyRecv(msg.Meta().RecvTime, p)
	}
	return nil
}

// Retrieve is used by the component to take a message from the incoming buffer
func (p *LimitNumMsgPort) Retrieve(now VTimeInSec) Msg {

	if len(p.buf) == 0 {
		return nil
	}

	msg := p.buf[0]
	p.buf = p.buf[1:]
	hookCtx := HookCtx{
		Domain: p,
		Now:    now,
		Pos:    HookPosPortMsgRetrieve,
		Item:   msg,
	}
	p.InvokeHook(hookCtx)

	if p.portBusy || len(p.buf) >= p.bufCapacity {
		p.portBusy = false
		p.conn.NotifyAvailable(now, p)
	}

	return msg
}

// Peek returns the first message in the port without removing it.
func (p *LimitNumMsgPort) Peek() Msg {

	if len(p.buf) == 0 {
		return nil
	}

	msg := p.buf[0]
	return msg
}

// PeekReady walks the buffer in order and returns the first message that
// satisfies ready, along with its index. ok=false when no message is ready.
// Callers must hand the returned idx to RetrieveAt to consume that message;
// using Retrieve would consume the head instead.
func (p *LimitNumMsgPort) PeekReady(
	ready func(Msg) bool,
) (msg Msg, idx int, ok bool) {
	for i, m := range p.buf {
		if ready(m) {
			return m, i, true
		}
	}
	return nil, -1, false
}

// RetrieveAt removes the message at idx (the position returned by PeekReady)
// and fires the same hook + NotifyAvailable that Retrieve does. Returns nil
// when idx is out of range.
func (p *LimitNumMsgPort) RetrieveAt(now VTimeInSec, idx int) Msg {

	if idx < 0 || idx >= len(p.buf) {
		return nil
	}

	msg := p.buf[idx]
	p.buf = append(p.buf[:idx], p.buf[idx+1:]...)
	hookCtx := HookCtx{
		Domain: p,
		Now:    now,
		Pos:    HookPosPortMsgRetrieve,
		Item:   msg,
	}
	p.InvokeHook(hookCtx)

	if p.portBusy {
		p.portBusy = false
		p.conn.NotifyAvailable(now, p)
	}

	return msg
}

// NotifyAvailable is called by the connection to notify the port that the
// connection is available again
func (p *LimitNumMsgPort) NotifyAvailable(now VTimeInSec) {
	if p.comp != nil {
		p.comp.NotifyPortFree(now, p)
	}
}

func (p *LimitNumMsgPort) CanSend(num int) bool {
	return len(p.buf)+num <= p.bufCapacity
}

// OutBufLen reports the occupancy/capacity of this port's source-side buffer
// inside its connection (what Send() actually gates on). ok=false when the
// connection does not expose it. Read-only, for deadlock reporting.
func (p *LimitNumMsgPort) OutBufLen() (used, capacity int, ok bool) {
	type srcBufQuerier interface {
		SrcBufLen(Port) (int, int, bool)
	}
	if c, has := p.conn.(srcBufQuerier); has {
		return c.SrcBufLen(p)
	}
	return 0, 0, false
}

// OutBufDstCount reports how many messages in this port's source-side buffer
// target dst. ok=false when the connection does not expose it. Read-only,
// used by senders to reserve dst capacity for in-flight messages.
func (p *LimitNumMsgPort) OutBufDstCount(dst Port) (count int, ok bool) {
	type srcDstQuerier interface {
		SrcBufDstCount(Port, Port) (int, bool)
	}
	if c, has := p.conn.(srcDstQuerier); has {
		return c.SrcBufDstCount(p, dst)
	}
	return 0, false
}

func (p *LimitNumMsgPort) Push(msg Msg) {

	if len(p.buf)+1 > p.bufCapacity {
		panic("cannot push more msg into the port")
	}

	p.buf = append(p.buf, msg)
}

func (p *LimitNumMsgPort) HeadToTail() {

	n := len(p.buf)

	if n <= 1 {
		return
	}

	head := p.buf[0]
	p.buf = append(p.buf[1:], head)
}

// GetBuffer
func (p *LimitNumMsgPort) GetBuffer() []Msg {

	return p.buf
}

func (p *LimitNumMsgPort) SetBuffer(
	buf []Msg,
) {

	p.buf = buf
}

// NewLimitNumMsgPort creates a new port that works for the provided component
func NewLimitNumMsgPort(
	comp Component,
	capacity int,
	name string,
) *LimitNumMsgPort {
	if comp == nil {
		panic("Port must belong to a component")
	}

	p := new(LimitNumMsgPort)
	p.comp = comp
	p.bufCapacity = capacity
	p.name = name
	return p
}
