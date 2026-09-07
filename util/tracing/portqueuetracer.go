// PortQueueTracer measures, per port, how long messages sit in the
// port's incoming buffer between Recv (push) and Retrieve (pop). This
// isolates queueing delay at the port granularity, separate from network
// flight time or downstream processing.
//
// Attach via AttachTo on each port whose queue-wait you want measured.
// Typical targets: endpoint NetworkPort (network-side incoming flits)
// and RDMA engine ports (RequestPort, ResponsePort, ToL2*, ToDramTop*,
// PwPort, CtrlPort).
package tracing

import "gitlab.com/akita/akita"

// PortQueueStats holds aggregated queue-wait metrics for one port.
type PortQueueStats struct {
	Count    uint64
	TotalSec float64
	MaxSec   float64
}

// PortQueueTracer is an akita.Hook that records port-buffer wait time.
type PortQueueTracer struct {
	inFlight map[uint64]inFlightEntry
	perPort  map[string]*PortQueueStats
}

type inFlightEntry struct {
	portName string
	recvTime float64
}

// NewPortQueueTracer creates a PortQueueTracer with empty state.
func NewPortQueueTracer() *PortQueueTracer {
	return &PortQueueTracer{
		inFlight: make(map[uint64]inFlightEntry),
		perPort:  make(map[string]*PortQueueStats),
	}
}

// AttachTo registers the tracer as a hook on the given port.
func (t *PortQueueTracer) AttachTo(port NamedHookable) {
	port.AcceptHook(t)
}

// Func implements akita.Hook by dispatching push/pop events.
// Hook-position comparison uses the Name string rather than pointer identity
// so that port types that define their own local HookPos variables (e.g.
// idealmemcontroller.LocalFirstPort) are still recognized correctly.
func (t *PortQueueTracer) Func(ctx akita.HookCtx) {
	if ctx.Pos == nil {
		return
	}
	switch ctx.Pos.Name {
	case akita.HookPosPortMsgRecvd.Name:
		t.onRecv(ctx)
	case akita.HookPosPortMsgRetrieve.Name:
		t.onRetrieve(ctx)
	}
}

func (t *PortQueueTracer) onRecv(ctx akita.HookCtx) {
	msg, ok := ctx.Item.(akita.Msg)
	if !ok {
		return
	}
	port, ok := ctx.Domain.(akita.Named)
	if !ok {
		return
	}
	t.inFlight[msg.Meta().ID] = inFlightEntry{
		portName: port.Name(),
		recvTime: float64(ctx.Now),
	}
}

func (t *PortQueueTracer) onRetrieve(ctx akita.HookCtx) {
	msg, ok := ctx.Item.(akita.Msg)
	if !ok {
		return
	}
	entry, found := t.inFlight[msg.Meta().ID]
	if !found {
		return
	}
	delete(t.inFlight, msg.Meta().ID)
	t.recordWait(entry.portName, float64(ctx.Now)-entry.recvTime)
}

func (t *PortQueueTracer) recordWait(portName string, wait float64) {
	s := t.perPort[portName]
	if s == nil {
		s = &PortQueueStats{}
		t.perPort[portName] = s
	}
	s.Count++
	s.TotalSec += wait
	if wait > s.MaxSec {
		s.MaxSec = wait
	}
}

// GetPerPort returns a copy of per-port stats keyed by port name.
func (t *PortQueueTracer) GetPerPort() map[string]PortQueueStats {
	out := make(map[string]PortQueueStats, len(t.perPort))
	for k, v := range t.perPort {
		out[k] = *v
	}
	return out
}

// GetTotal returns aggregate count, total wait, and max wait across all
// attached ports.
func (t *PortQueueTracer) GetTotal() (uint64, float64, float64) {
	var count uint64
	var sumSec, maxSec float64
	for _, s := range t.perPort {
		count += s.Count
		sumSec += s.TotalSec
		if s.MaxSec > maxSec {
			maxSec = s.MaxSec
		}
	}
	return count, sumSec, maxSec
}
