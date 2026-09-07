package switching

import (
	"gitlab.com/akita/akita"
	"gitlab.com/akita/protocol"
)

// DynamicClusteringEndPoint wraps a vanilla EndPoint with a deliver-time hook
// that intercepts DistributeRsp messages whose BranchNodeId matches this
// endpoint's chipletId. The intercepted message is fanned out into one or
// two DataReadyRsp messages pushed back into msgOutBuf, so the normal
// prepareFlits → sendFlitOut pipeline carries them out as ordinary
// wormhole streams.
//
// Net effect at the protocol layer is identical to the previous switch-
// level branching: the same two unicast DataReadyRsp streams leave the
// branch-node chiplet. Net effect at the NoC layer is that the underlying
// switch can be the plain MultiFlitChipletSwitchImpl — no per-switch
// branch state machine, no head-flit input-port holding.
type DynamicClusteringEndPoint struct {
	*EndPoint

	chipletId int

	// fanoutTarget is the endpoint whose msgOutBuf the fanned-out
	// DataReadyRsp are injected into. DistributeRsp arrives on the Req-side
	// endpoint but the resulting DataReadyRsp must leave on the Rsp network,
	// so the Req endpoint points this at its sibling Rsp endpoint. Defaults
	// to self (relay stays on the same network).
	fanoutTarget *EndPoint
}

// NewDynamicClusteringEndPoint installs the DeliverHook on the given vanilla
// EndPoint so DistributeRsp arrivals matching chipletId are intercepted
// and fanned out at deliver time.
func NewDynamicClusteringEndPoint(
	ep *EndPoint, chipletId int,
) *DynamicClusteringEndPoint {
	dep := &DynamicClusteringEndPoint{
		EndPoint:     ep,
		chipletId:    chipletId,
		fanoutTarget: ep,
	}
	ep.DeliverHook = dep.fanOutBranchRsp
	return dep
}

// SetFanoutTarget redirects the fanned-out DataReadyRsp to another endpoint's
// egress (used to move relay output onto the Rsp network).
func (dep *DynamicClusteringEndPoint) SetFanoutTarget(t *EndPoint) {
	dep.fanoutTarget = t
}

// fanOutBranchRsp is the DeliverHook. It owns DistributeRsp messages
// whose BranchNodeId == this chiplet; everything else falls through to
// the default delivery path.
//
// Returns:
//
//	(true, true)  — DistributeRsp consumed; readyMsgs may pop it.
//	(true, false) — DistributeRsp owned but msgOutBuf cannot host the
//	                resulting 1 or 2 DataReadyRsps yet; retry next tick.
//	(false, _)    — not our message type; default delivery.
func (dep *DynamicClusteringEndPoint) fanOutBranchRsp(
	msg akita.Msg, now akita.VTimeInSec,
) (handled bool, ok bool) {
	distRsp, isBranchMsg := msg.(*protocol.DistributeRsp)
	if !isBranchMsg || distRsp.BranchNodeId != dep.chipletId {
		return false, false
	}
	t := dep.fanoutTarget
	if t == nil {
		t = dep.EndPoint
	}
	need := 1
	if distRsp.SecondDst != nil && distRsp.SecondDst != distRsp.FirstDst {
		need = 2
	}
	if len(t.msgOutBuf)+need > t.msgOutBufSize {
		return true, false
	}
	t.msgOutBuf = append(t.msgOutBuf, dep.buildRsp(distRsp,
		distRsp.FirstDst, now))
	if need == 2 {
		t.msgOutBuf = append(t.msgOutBuf, dep.buildRsp(distRsp,
			distRsp.SecondDst, now))
	}
	t.TickLater(now)
	return true, true
}

// buildRsp constructs one DataReadyRsp from the original DistributeRsp
// targeted at a single destination. Src is preserved as the original
// source so downstream tracing keeps the producer identity.
func (dep *DynamicClusteringEndPoint) buildRsp(
	distRsp *protocol.DistributeRsp,
	dst akita.Port,
	now akita.VTimeInSec,
) akita.Msg {
	// Per-copy DistInfo: each fan-out copy gets its own so a downstream
	// mutation (e.g. the requestor adjusting Hotness) cannot alias the other
	// copy or the original DistributeRsp.Info.
	info := distRsp.Info
	if di, ok := info.(*protocol.DistInfo); ok && di != nil {
		diCopy := *di
		info = &diCopy
	}
	return protocol.DataReadyRspBuilder{}.
		WithSendTime(now).
		WithSrc(distRsp.Meta().Src).
		WithDst(dst).
		WithRspTo(distRsp.GetRespondTo()).
		WithData(distRsp.Data).
		WithInfo(info).
		Build()
}
