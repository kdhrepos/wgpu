// Helpers that emit RDMAFlightTracer Start/End tasks at the RDMA-engine
// boundary. Paired with NetworkFlightTracer (emitted from the EndPoint
// boundary in noc/networking/switching/endpoint.go). The difference
// rdma_flight − network_flight per hop bucket isolates the RDMA engine's
// own queuing and processing overhead from the pure network transit.
package rdma

import (
	"strings"

	"gitlab.com/akita/akita"
	"gitlab.com/akita/protocol"
	"gitlab.com/akita/util/tracing"
)

// emitRdmaFlightStart fires StartTask at the moment domain (a source-side
// RDMA engine) has successfully handed the inner msg to its outbound
// port. msg must be the inner cloned msg (post-NetworkMsg unwrap) so
// Src/Dst expose the actual chiplet endpoints. Safe to call for msg
// types that are not RDMA req/rsp (it no-ops on those).
func emitRdmaFlightStart(
	msg akita.Msg, now akita.VTimeInSec, domain tracing.NamedHookable,
) {
	emitRdmaFlightStartAt(msg, now, domain)
}

// emitRdmaFlightStartAt is like emitRdmaFlightStart but takes an explicit
// startTime so the recorded interval can begin at the source-side
// first-observation moment rather than at the eventual Send-success
// moment. This captures the outbound-port admission stall the engine
// otherwise hides from the tracer.
func emitRdmaFlightStartAt(
	msg akita.Msg, startTime akita.VTimeInSec, domain tracing.NamedHookable,
) {
	kind, ok := rdmaFlightKindForMsg(msg)
	if !ok {
		return
	}
	src, dst := rdmaChipletPair(msg)
	tracing.StartTask(
		tracing.RDMAFlightTaskID(msg.Meta().ID, kind),
		0, startTime, domain, kind, "", src+"."+dst)
}

// emitRdmaFlightEnd fires EndTask at the moment domain (a dest-side RDMA
// engine) has retrieved the inner msg from its inbound port. msg here is
// the inner reassembled msg sitting in domain's RequestPort/ResponsePort.
func emitRdmaFlightEnd(
	msg akita.Msg, now akita.VTimeInSec, domain tracing.NamedHookable,
) {
	kind, ok := rdmaFlightKindForMsg(msg)
	if !ok {
		return
	}
	tracing.EndTask(
		tracing.RDMAFlightTaskID(msg.Meta().ID, kind), now, domain)
}

func rdmaFlightKindForMsg(m akita.Msg) (string, bool) {
	switch m.(type) {
	case *protocol.TransReadReq:
		return tracing.RDMAFlightKindReq, true
	case *protocol.TransDataReadyRsp:
		return tracing.RDMAFlightKindRsp, true
	case protocol.AccessReq:
		return tracing.RDMAFlightKindReq, true
	case protocol.AccessRsp:
		return tracing.RDMAFlightKindRsp, true
	}
	return "", false
}

// noteFlightFirstSeen records `now` as the first-observation time of
// srcID if no entry exists yet. Subsequent calls for the same srcID
// (e.g. when the outbound Send keeps failing and the source msg stays
// on its inbound port) are no-ops so the earliest moment is preserved.
func (e *RDMAEngineImpl) noteFlightFirstSeen(
	srcID uint64, now akita.VTimeInSec,
) {
	if e.flightFirstSeen == nil {
		e.flightFirstSeen = make(map[uint64]akita.VTimeInSec)
	}
	if _, ok := e.flightFirstSeen[srcID]; !ok {
		e.flightFirstSeen[srcID] = now
	}
}

// consumeFlightFirstSeen returns the recorded first-observation time
// for srcID and clears the entry. Falls back to `now` if no entry was
// recorded (defensive — a code path that emits without prior note).
func (e *RDMAEngineImpl) consumeFlightFirstSeen(
	srcID uint64, now akita.VTimeInSec,
) akita.VTimeInSec {
	if e.flightFirstSeen == nil {
		return now
	}
	t, ok := e.flightFirstSeen[srcID]
	if !ok {
		return now
	}
	delete(e.flightFirstSeen, srcID)
	return t
}

func rdmaChipletPair(m akita.Msg) (string, string) {
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
