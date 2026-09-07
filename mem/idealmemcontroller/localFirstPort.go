package idealmemcontroller

import (
	"gitlab.com/akita/akita"
)

type LocalFirstPort struct {
	akita.HookableBase

	name string
	comp akita.Component
	conn akita.Connection

	buf         *SmartBuffer
	bufCapacity int
	portBusy    bool
}

// HookPosPortMsgRecvd marks when an inbound message arrives at a the given port
var HookPosPortMsgRecvd = &akita.HookPos{Name: "Port Msg Recv"}

// HookPosPortMsgRetrieve marks when an outbound message is sent over a connection
var HookPosPortMsgRetrieve = &akita.HookPos{Name: "Port Msg  Retrieve"}

// SetConnection sets which connection plugged in to this port.
func (p *LocalFirstPort) SetConnection(conn akita.Connection) {
	p.conn = conn
}

// Component returns the owner component of the port.
func (p *LocalFirstPort) Component() akita.Component {
	return p.comp
}

// Name returns the name of the port.
func (p *LocalFirstPort) Name() string {
	return p.name
}

// Send is used to send a message out from a component
func (p *LocalFirstPort) Send(msg akita.Msg) *akita.SendError {
	err := p.conn.Send(msg)
	return err
}

// Recv is used to deliver a message to a component
func (p *LocalFirstPort) Recv(msg akita.Msg) *akita.SendError {

	if !p.buf.CanPush() {
		p.portBusy = true
		return akita.NewSendError()
	}

	hookCtx := akita.HookCtx{
		Domain: p,
		Now:    msg.Meta().RecvTime,
		Pos:    HookPosPortMsgRecvd,
		Item:   msg,
	}
	p.InvokeHook(hookCtx)

	p.buf.Push(msg)

	// Pre-arm portBusy when this Recv just filled the buffer. The next
	// Retrieve will then unconditionally fire conn.NotifyAvailable,
	// closing the missed-wake-up window where buf transitions
	// gradually full → has-room → full again without any Recv ever
	// returning a SendError. Mirrors akita.LimitNumMsgPort.Recv.
	if !p.buf.CanPush() {
		p.portBusy = true
	}

	if p.comp != nil {
		p.comp.NotifyRecv(msg.Meta().RecvTime, p)
	}
	return nil
}

// Retrieve is used by the component to take a message from the incoming buffer
func (p *LocalFirstPort) Retrieve(now akita.VTimeInSec) akita.Msg {

	if p.buf.Size() == 0 {
		return nil
	}

	msg := p.buf.Pop()
	hookCtx := akita.HookCtx{
		Domain: p,
		Now:    now,
		Pos:    HookPosPortMsgRetrieve,
		Item:   msg,
	}
	p.InvokeHook(hookCtx)

	// Mirrors akita.LimitNumMsgPort.Retrieve's OR-gate: also fire
	// NotifyAvailable when the buf is still saturated post-pop, as a
	// safety net for any path that left portBusy unset while filling.
	if p.portBusy || !p.buf.CanPush() {
		p.portBusy = false
		p.conn.NotifyAvailable(now, p)
	}

	return msg.(akita.Msg)
}

// Peek returns the first message in the port without removing it.
func (p *LocalFirstPort) Peek() akita.Msg {

	if p.buf.Size() == 0 {
		return nil
	}

	msg := p.buf.Peek()
	return msg.(akita.Msg)
}

// NotifyAvailable is called by the connection to notify the port that the
// connection is available again
func (p *LocalFirstPort) NotifyAvailable(now akita.VTimeInSec) {
	if p.comp != nil {
		p.comp.NotifyPortFree(now, p)
	}
}

func (p *LocalFirstPort) CanSend(num int) bool {
	return p.buf.Size()+num <= p.bufCapacity
}

// NewLocalFirstPort creates a new port that works for the provided component
func NewLocalFirstPort(
	comp akita.Component,
	capacity int,
	name string,
) *LocalFirstPort {
	if comp == nil {
		panic("Port must belong to a component")
	}

	p := new(LocalFirstPort)
	p.buf = NewSmartBuffer(capacity)
	p.comp = comp
	p.bufCapacity = capacity
	p.name = name
	return p
}
