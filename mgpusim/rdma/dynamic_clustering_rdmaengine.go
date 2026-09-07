// Package rdma provides the implementation of an RDMA engine.
package rdma

import (
	"fmt"
	"log"
	"reflect"
	"sort"
	"strconv"
	"strings"

	"gitlab.com/akita/akita"
	"gitlab.com/akita/lmf"
	"gitlab.com/akita/protocol"
	"gitlab.com/akita/util/global"
	"gitlab.com/akita/util/tracing"
)

// DynamicClusteringRdmaEngine is an RDMA engine for dynamic cluster
// with distributed cacheline replication.
type DynamicClusteringRdmaEngine struct {
	RDMAEngineImpl

	ToL1Bottom akita.Port

	RdmaReqPortByChipletId map[int]akita.Port
	RdmaRspPortByChipletId map[int]akita.Port

	L2BottomFinder lmf.LowModuleFinder

	PCTable PCTableInterface

	// sharedHotnessSync selects the zero-overhead cross-chiplet hotness
	// sync: the requestor reads the HOME chiplet's PC classification
	// directly instead of its own piggyback-fed local view.
	sharedHotnessSync bool

	clusterMap  [][][]int
	clusterSize int

	samplingPeriod akita.VTimeInSec
	lastTime       akita.VTimeInSec

	// rspStall counts consecutive Ticks where ResponsePort has pending
	// responses but none could be routed (canRoute rejected all). Used to
	// dump the ejection-blocked state when a livelock parks responses here.
	rspStall int
}

// DeadlockSummary overrides the embedded RDMAEngineImpl version, which walks
// per-bank port slices (ToL2RDMAs/...). This engine uses the single
// ToL2RDMA/ToL2Bottom/ToDramTop ports, so read those directly plus the
// outstanding-request gate that throttles new L2 reads.
func (e *DynamicClusteringRdmaEngine) DeadlockSummary() (string, bool) {
	peek := func(p akita.Port) bool { return p != nil && p.Peek() != nil }
	toL2RDMA := peek(e.ToL2RDMA)
	toL2Bottom := peek(e.ToL2Bottom)
	toDramTop := peek(e.ToDramTop)
	pw := peek(e.PwPort)
	reqQ := peek(e.RequestPort)
	rspQ := peek(e.ResponsePort)
	outside := len(e.transactionsFromOutside)
	inside := len(e.transactionsFromInside)

	anyPending := toL2RDMA || toL2Bottom || toDramTop || pw || reqQ ||
		rspQ || outside > 0 || inside > 0 || e.outstandingReqs > 0

	line := fmt.Sprintf(
		"toL2RDMA=%v toL2Bottom=%v toDramTop=%v pw=%v req=%v rsp=%v | "+
			"outside=%d inside=%d outstanding=%d/%d",
		toL2RDMA, toL2Bottom, toDramTop, pw, reqQ, rspQ,
		outside, inside, e.outstandingReqs, maxOutstandingReqs)
	return line, anyPending
}

// Tick checks if make progress
func (e *DynamicClusteringRdmaEngine) Tick(now akita.VTimeInSec) bool {
	madeProgress := false

	madeProgress = e.processFromCtrlPort(now) || madeProgress
	if e.isDraining {
		madeProgress = e.drainRDMA(now) || madeProgress
	}
	madeProgress = e.processRspFromOutside(now) || madeProgress
	madeProgress = e.processRspFromDram(now) || madeProgress
	madeProgress = e.processRspFromL2(now) || madeProgress
	madeProgress = e.processReqFromOutside(now) || madeProgress
	madeProgress = e.processReqFromPwPort(now) || madeProgress
	madeProgress = e.processReqFromL2(now) || madeProgress
	madeProgress = e.processRetry(now) || madeProgress

	period := (now - e.lastTime) * akita.VTimeInSec(e.Freq)
	if period >= e.samplingPeriod {
		e.PCTable.Decay()
		e.lastTime = now
	}

	return madeProgress
}

func (e *DynamicClusteringRdmaEngine) processReqFromL2(
	now akita.VTimeInSec,
) bool {
	if e.pauseIncomingReqsFromL1 {
		return false
	}

	madeProgress := false
	port := e.ToL2Bottom
	for i := 0; i < e.numTransPerCycle; i++ {
		req := port.Peek()
		if req == nil {
			continue
		}

		switch req := req.(type) {
		case *protocol.WriteReq:
			if e.processReqFromL2Aux(now, req, port) {
				madeProgress = true
			}
		case *protocol.ReadReq:
			if e.processReadReqFromL2(now, req, port) {
				madeProgress = true
			}
		default:
			panic("unknown req type")
		}
	}
	return madeProgress
}

// appendVisited returns visited with self appended without aliasing the input,
// so each lookup hop carries its own copy of the chain's visit sequence.
func appendVisited(visited []int, self int) []int {
	out := make([]int, len(visited)+1)
	copy(out, visited)
	out[len(visited)] = self
	return out
}

// lookupPathStr renders a chiplet-id visit sequence as a dash-joined string.
func lookupPathStr(ids []int) string {
	parts := make([]string, len(ids))
	for i, id := range ids {
		parts[i] = strconv.Itoa(id)
	}
	return strings.Join(parts, "-")
}

func containsPort(ps []akita.Port, p akita.Port) bool {
	for _, x := range ps {
		if x == p {
			return true
		}
	}
	return false
}

func (e *DynamicClusteringRdmaEngine) processReadReqFromL2(
	now akita.VTimeInSec,
	req *protocol.ReadReq,
	recvPort akita.Port,
) bool {
	e.noteFlightFirstSeen(req.Meta().ID, now)

	if e.outstandingReqs >= maxOutstandingReqs {
		if global.GetInstance().GetVerbose() {
			fmt.Println("Too many outstanding reqs", e.Name())
		}
		return false
	}
	pc, _ := req.Info.(uint64)

	addr := req.GetAddress()
	requestorId := e.GetChipletId()

	globalHome := e.RdmaReqAddressTable.Find(addr)
	homeEngine := globalHome.Component().(*DynamicClusteringRdmaEngine)
	toGlobalHome := e.GetDistance(homeEngine)
	globalHomeId := homeEngine.ChipletId

	// Request-side hotness gating via ReqFromInside (pure query, no
	// DistCount side effect; RspFromInside is reserved for the response
	// path). In zero-overhead shared-sync mode the requestor reads the
	// HOME chiplet's own live classification directly (same-process
	// pointer deref -> no simulated cross-chiplet traffic), modelling an
	// oracle broadcast of each home's hotness. Otherwise it uses its own
	// piggyback-fed local view.
	hotnessSource := e.PCTable
	if e.sharedHotnessSync {
		hotnessSource = homeEngine.PCTable
	}
	hotness := hotnessSource.ReqFromInside(pc)

	// Scheme selection — PC's hotness picks ONE cluster partitioning to
	// enumerate. Hot scheme always enabled; Warm scheme keeps the legacy
	// xDim>=6 gate. All other hotness values (Cold / Bypass / Streaming /
	// Warm@xDim<6) skip LH lookup and fall straight to GH.
	var scheme protocol.Hotness
	switch {
	case hotness == protocol.Global:
		scheme = protocol.Global
	case hotness == protocol.Hot:
		scheme = protocol.Hot
	case hotness == protocol.Warm && e.xDim >= 6:
		scheme = protocol.Warm
	}

	// Multi-cluster LH enumeration. For each cluster under the chosen
	// scheme, compute the address-hashed LH and keep it iff:
	//   (a) it is not the requestor and not the global home;
	//   (b) it lies on the requestor→GH minimal path (IsOnMinimalPath) —
	//       guarantees no per-hop hop-count inflation;
	//   (c) it is strictly closer to the requestor than the global home
	//       (legacy "closer-than-GH" gate kept from the 2-LH design).
	type lhCand struct {
		id   int
		port akita.Port
		dist int
		dx   int // |lhX - requestorX| — X-Y routing tiebreaker
	}
	seen := map[int]bool{requestorId: true, globalHomeId: true}
	cands := []lhCand{}

	if scheme != 0 {
		reqX := requestorId % e.xDim
		numClusters := e.numClustersForHotness(scheme)
		for c := 0; c < numClusters; c++ {
			lhId := e.GetLocalHomeIdForCluster(c, addr, scheme)
			if seen[lhId] {
				continue
			}
			if !IsOnMinimalPath(requestorId, globalHomeId, lhId, e.xDim) {
				continue
			}
			port := e.RdmaReqPortByChipletId[lhId]
			dist := e.GetDistance(port.Component().(RDMAEngine))
			if dist >= toGlobalHome {
				continue
			}
			seen[lhId] = true
			lhX := lhId % e.xDim
			cands = append(cands, lhCand{lhId, port, dist, Abs(lhX - reqX)})
		}
		// Closer-first ordering with X-Y routing tiebreaker. Required by the
		// LookupReq chain semantics (head of LookupDst is always "where I am
		// now"; subsequent hops advance toward GH). When two candidates have
		// the same L1 distance from R, prefer the one with larger Δx — this
		// matches the codebase's X-first dimension-order routing convention
		// (see noc/networking/routing/meshroutingtable.go:74 "X-Y routing").
		sort.Slice(cands, func(i, j int) bool {
			if cands[i].dist != cands[j].dist {
				return cands[i].dist < cands[j].dist
			}
			return cands[i].dx > cands[j].dx
		})
	}

	// Chain monotonicity pass. On a 2-D mesh, two candidates at the same
	// L1 distance from R can both lie on the R→GH geodesic yet be lateral
	// to each other (e.g. R=(0,0), GH=(3,3), P1=(1,2), P2=(2,1) — R→P1→P2
	// detours by 2 hops). Greedy filter by sorted distance: each kept
	// candidate must lie on (prev → GH) minimal path so the full chain
	// R → c0 → c1 → … → GH stays monotone.
	lookupPorts := make([]akita.Port, 0, len(cands))
	prev := requestorId
	for _, c := range cands {
		if !IsOnMinimalPath(prev, globalHomeId, c.id, e.xDim) {
			continue
		}
		lookupPorts = append(lookupPorts, c.port)
		prev = c.id
	}

	// Forced own-cluster local-home probe. The requestor's own cluster copy
	// lives at GetLocalHomeId(requestor); if it was filtered out above (off the
	// R→GH minimal path or farther than GH) prepend it as the chain head so it
	// is ALWAYS probed, even at the cost of extra hops. Marked PeekOnly so this
	// off-path probe never coalesces into a pending MSHR — that is what would
	// otherwise let two mutual off-path lookups wait on each other (circular
	// dependency). See directoryStage.readReady.
	forcedHead := false
	if scheme != 0 && global.GetInstance().GetForceLHLookup() {
		ownLHId := e.GetLocalHomeId(requestorId, addr, scheme)
		if ownLHId != requestorId && ownLHId != globalHomeId {
			ownLHPort := e.RdmaReqPortByChipletId[ownLHId]
			if !containsPort(lookupPorts, ownLHPort) {
				lookupPorts = append([]akita.Port{ownLHPort}, lookupPorts...)
				forcedHead = true
			}
		}
	}

	// if the list of lookup port is not empty, send the requests
	shouldLookup := len(lookupPorts) > 0

	var cloned protocol.AccessReq
	if shouldLookup {
		lb := protocol.LookupReqBuilder{}.
			WithSendTime(now).
			WithSrc(e.RequestPort).
			WithDst(lookupPorts[0]).
			WithLookupDst(lookupPorts).
			WithPID(req.GetPID()).
			WithAddress(req.GetAddress()).
			WithByteSize(req.GetByteSize()).
			WithInfo(req.Info).
			WithHop(1).                      // first lookup trial; incremented on each chain forward
			WithVisited([]int{requestorId}). // chain starts at the requestor
			WithScheme(scheme).              // partition for own/other classification
			WithPeekOnly(forcedHead)         // forced off-path head = peek-only (no MSHR coalesce)
		cloned = lb.Build()

		cloned.(*protocol.LookupReq).IsPtw = req.IsPtw
	} else {
		cloned = e.cloneReq(req)
		cloned.Meta().SendTime = now
		cloned.Meta().Src = e.RequestPort
		cloned.Meta().Dst = globalHome
		cloned.(*protocol.ReadReq).IsPtw = req.IsPtw
	}

	networkMsg := &protocol.NetworkMsg{
		Msg:     cloned,
		SrcPort: e.RequestPort,
	}

	err := e.RequestPort.Send(networkMsg)
	if err == nil {
		e.outstandingReqs++

		recvPort.(*akita.LimitNumMsgPort).Retrieve(now)

		// Measure the "no lookup possible" case. A request is a lookup
		// *candidate* (eligible) when LH enumeration ran (scheme != 0) and the
		// global home is in a *different* cluster, so a lookup would have been
		// worthwhile; GH-in-own-cluster is excluded by the user's request.
		// Among candidates, "skipped" = no local home lay on the
		// requestor->GH minimal path, so no lookup was sent. Counted here
		// (after a successful send) so a retried request is not
		// double-counted. eligible is the denominator, skipped the numerator.
		if scheme != 0 &&
			e.GetClusterId(requestorId, scheme) !=
				e.GetClusterId(globalHomeId, scheme) {
			tracing.StartTask(0, 0, now, e,
				"lookup-outcome-eligible", "",
				&tracing.LookupOutcome{IsPtw: req.IsPtw})
			if !shouldLookup {
				tracing.StartTask(0, 0, now, e,
					"lookup-outcome-skipped", "",
					&tracing.LookupOutcome{IsPtw: req.IsPtw})
			}
		}

		tracing.TraceReqReceive(req, now, e)
		tracing.TraceReqInitiate(cloned, now, e,
			tracing.MsgIDAtReceiver(req, e))

		emitRdmaFlightStartAt(cloned,
			e.consumeFlightFirstSeen(req.Meta().ID, now), e)

		trans := transaction{
			fromInside: req,
			toOutside:  cloned,
		}
		e.transactionsFromInside = append(e.transactionsFromInside, trans)

		srcName := req.Meta().Src.Name()
		dstName := globalHome.Name()
		srcChip := strings.Split(srcName, ".")[1]
		dstChip := strings.Split(dstName, ".")[1]

		if req.IsPtw {
			// tracing.StartTask(
			// cloned.Meta().ID,
			// "",
			// now,
			// e,
			// "pte-caching-remote-ptw-trace",
			// strconv.FormatUint(pc, 10),
			// srcChip+"."+dstChip)
		} else {
			tracing.StartTask(
				cloned.Meta().ID,
				0,
				now,
				e,
				"hop-trace",
				strconv.FormatUint(pc, 10),
				srcChip+"."+dstChip)
		}

		return true
	}

	return false
}

func (e *DynamicClusteringRdmaEngine) processRspFromL2(
	now akita.VTimeInSec,
) bool {
	madeProgress := false
	port := e.ToL2RDMA.(*akita.LimitNumMsgPort)
	for i := 0; i < e.numTransPerCycle; i++ {
		// Sink-bound responses (DataReadyRsp/WriteDoneRsp) leave on
		// ResponsePort and finalize transactions. They must not be
		// head-of-line blocked by a LookupMissRsp stalled on a full
		// RequestPort (request-net re-injection). Drain them first.
		if m, idx, ok := port.PeekReady(isSinkBoundRspFromL2); ok {
			done := false
			switch r := m.(type) {
			case *protocol.DataReadyRsp:
				done = e.processDataReadyRspFromL2(now, r, port, idx)
			case *protocol.WriteDoneRsp:
				done = e.processWriteDoneRspFromL2(now, r, port, idx)
			}
			if done {
				madeProgress = true
				continue
			}
		}

		if m, idx, ok := port.PeekReady(isLookupMissRsp); ok {
			if e.processLookupMissRspFromL2(
				now, m.(*protocol.LookupMissRsp), port, idx) {
				madeProgress = true
				continue
			}
		}

		if head := port.Peek(); head != nil &&
			!isSinkBoundRspFromL2(head) && !isLookupMissRsp(head) {
			panic("unknown rsp type from L2")
		}
		break
	}
	return madeProgress
}

func isSinkBoundRspFromL2(m akita.Msg) bool {
	switch m.(type) {
	case *protocol.DataReadyRsp, *protocol.WriteDoneRsp:
		return true
	}
	return false
}

func isLookupMissRsp(m akita.Msg) bool {
	_, ok := m.(*protocol.LookupMissRsp)
	return ok
}

func (e *DynamicClusteringRdmaEngine) processLookupMissRspFromL2(
	now akita.VTimeInSec,
	rsp *protocol.LookupMissRsp,
	recvPort akita.Port,
	idx int,
) bool {
	transactionIndex := e.findTransactionByRspToID(
		rsp.GetRespondTo(), e.transactionsFromOutside)

	if transactionIndex == -1 {
		panic("cannot find transaction for lookup rsp")
	}

	trans := e.transactionsFromOutside[transactionIndex]

	originalReq, ok := trans.fromOutside.(*protocol.LookupReq)
	if !ok {
		panic("processLookupMissRspFromL2")
	}

	// Strip-head chain advancement. Convention: originalReq.LookupDst[0] is
	// always "the LH we are at right now". On miss, drop the head and
	// forward to the next hop (if any), carrying the remaining list so the
	// next hop can continue stripping. Generalizes the legacy 2-port
	// hard-coded `LookupDst[1]` index to arbitrary chain length.
	remaining := originalReq.LookupDst[1:]

	var req protocol.AccessReq
	var nextDst akita.Port
	if len(remaining) > 0 {
		nextDst = remaining[0]

		req = protocol.LookupReqBuilder{}.
			WithSendTime(now).
			WithSrc(originalReq.Meta().Src).
			WithDst(nextDst).
			WithLookupDst(remaining).
			WithPID(originalReq.PID).
			WithAddress(originalReq.Address).
			WithByteSize(originalReq.AccessByteSize).
			WithInfo(originalReq.Info).   // PC to distinguish between instruction fetch and data access
			WithHop(originalReq.Hop + 1). // advance the lookup-trial counter
			// This LH missed: append it to the visit sequence before forwarding.
			WithVisited(appendVisited(originalReq.Visited, e.GetChipletId())).
			WithScheme(originalReq.Scheme). // carry partition unchanged down the chain
			Build()
		req.(*protocol.LookupReq).IsPtw = originalReq.IsPtw
	} else {
		// Last hop — fall back to the global home owning this address.
		nextDst = e.RdmaReqAddressTable.Find(originalReq.Address)

		req = protocol.ReadReqBuilder{}.
			WithSendTime(now).
			WithSrc(originalReq.Meta().Src).
			WithDst(nextDst).
			WithPID(originalReq.PID).
			WithAddress(originalReq.Address).
			WithByteSize(originalReq.AccessByteSize).
			WithInfo(originalReq.Info). // PC to distinguish between instruction fetch and data access
			Build()
		req.(*protocol.ReadReq).IsPtw = originalReq.IsPtw
	}

	req.Meta().ID = originalReq.Meta().ID

	networkMsg := &protocol.NetworkMsg{
		Msg:     req,
		SrcPort: e.RequestPort,
	}

	err := e.RequestPort.Send(networkMsg)
	if err == nil {
		recvPort.(*akita.LimitNumMsgPort).RetrieveAt(now, idx)

		if len(remaining) == 0 {
			// Lookup chain exhausted: every probed local home missed and the
			// request now falls back to the global home.
			tracing.StartTask(0, 0, now, e,
				"lookup-outcome-reached-gh", "",
				&tracing.LookupOutcome{IsPtw: originalReq.IsPtw})

			// Visit-sequence: requestor + every probed local home (this last
			// one included), with the global home flagged separately as "-g<id>"
			// since the lookup missed everywhere and fell back to it.
			ghId := nextDst.Component().(*DynamicClusteringRdmaEngine).ChipletId
			visited := appendVisited(originalReq.Visited, e.GetChipletId())
			tracing.StartTask(0, 0, now, e, "lookup-path", "",
				"miss-"+lookupPathStr(visited)+"-g"+strconv.Itoa(ghId))
		}

		// Close the tasks opened by processLookupReqFromOutside for this hop.
		// A relayed lookup miss never returns here, so without these both the
		// req_in (TraceReqReceive) and req_out (TraceReqInitiate) tasks linger
		// in every self-cleaning tracer's inflight map forever (leak scaling
		// with chain length). Mirrors the local-serve path in
		// processDataReadyRspFromL2 and the engine's own transaction drop below.
		tracing.TraceReqFinalize(trans.toInside, now, e)
		tracing.TraceReqComplete(trans.fromOutside, now, e)

		e.transactionsFromOutside =
			append(e.transactionsFromOutside[:transactionIndex],
				e.transactionsFromOutside[transactionIndex+1:]...)

		return true
	}

	return false
}

func (e *DynamicClusteringRdmaEngine) processWriteDoneRspFromL2(
	now akita.VTimeInSec,
	rsp *protocol.WriteDoneRsp,
	recvPort akita.Port,
	idx int,
) bool {
	e.noteFlightFirstSeen(rsp.Meta().ID, now)

	transactionIndex := e.findTransactionByRspToID(
		rsp.GetRespondTo(), e.transactionsFromOutside)
	trans := e.transactionsFromOutside[transactionIndex]

	rspToOutside := e.cloneRsp(rsp, trans.fromOutside.Meta().ID)
	rspToOutside.Meta().SendTime = now
	rspToOutside.Meta().Src = e.ResponsePort
	rspToOutside.Meta().Dst =
		e.RdmaRspAddressTable[trans.fromOutside.Meta().Src]

	networkMsg := &protocol.NetworkMsg{
		Msg:     rspToOutside,
		SrcPort: e.ResponsePort,
	}

	if e.ResponsePort.Send(networkMsg) != nil {
		return false
	}

	recvPort.(*akita.LimitNumMsgPort).RetrieveAt(now, idx)
	tracing.TraceReqFinalize(trans.toInside, now, e)
	tracing.TraceReqComplete(trans.fromOutside, now, e)
	emitRdmaFlightStartAt(rspToOutside,
		e.consumeFlightFirstSeen(rsp.Meta().ID, now), e)
	e.transactionsFromOutside = append(
		e.transactionsFromOutside[:transactionIndex],
		e.transactionsFromOutside[transactionIndex+1:]...)

	return true
}

func (e *DynamicClusteringRdmaEngine) processDataReadyRspFromL2(
	now akita.VTimeInSec,
	rsp *protocol.DataReadyRsp,
	recvPort akita.Port,
	idx int,
) bool {
	e.noteFlightFirstSeen(rsp.Meta().ID, now)

	transactionIndex := e.findTransactionByRspToID(
		rsp.GetRespondTo(), e.transactionsFromOutside)
	if transactionIndex == -1 {
		panic("cannot find transaction for data ready rsp")
	}
	trans := e.transactionsFromOutside[transactionIndex]
	_, wasLookup := trans.fromOutside.(*protocol.LookupReq) // check if original req was lookup.

	var rspToOutside protocol.AccessRsp
	var distInfo *protocol.DistInfo
	var hotness protocol.Hotness

	pc, _ := rsp.Info.(uint64) // for instruction fetch, the info is nil.
	pAddr := trans.fromOutside.(protocol.AccessReq).GetAddress()
	hotness = e.PCTable.RspFromInside(pc, pAddr)

	// if wasLookup &&
	// 	hotness != protocol.Global {
	// 	hotness = protocol.Bypass
	// }

	// setup distribution info — propagate IsPtw from the originating
	// request (ReadReq or LookupReq) so the destination L2's install
	// path can prefer PTEs over plain data on hotness ties.
	isPtw := false
	switch r := trans.fromOutside.(type) {
	case *protocol.ReadReq:
		isPtw = r.IsPtw
	case *protocol.LookupReq:
		isPtw = r.IsPtw
	}
	distInfo = &protocol.DistInfo{}
	distInfo.PAddr = pAddr
	distInfo.PC = pc
	distInfo.Hotness = hotness
	distInfo.IsSecond = false
	distInfo.IsPtw = isPtw

	// requestor
	requestorId := e.RdmaRspAddressTable[trans.fromOutside.Meta().Src].
		Component().(*DynamicClusteringRdmaEngine).GetChipletId()

	// Real cumulative hop distance for this resolved access: e is wherever
	// the read actually resolved (the true global home, or the hitting
	// local home on a lookup-chain hit — see "lookup-outcome-hit" below,
	// which relies on the same "chain runs on the minimal path" property).
	if !isPtw {
		tracing.StartTask(0, 0, now, e, tracing.CachelineHopKind, "",
			&tracing.CachelineHopDetail{
				Addr: pAddr,
				Hop:  uint64(GetDistanceById(requestorId, e.GetChipletId(), e.xDim)),
			})
	}

	requestorClusterId := e.GetClusterId(requestorId, hotness)
	requestorPort := e.RdmaRspAddressTable[trans.fromOutside.Meta().Src]

	// home node
	globalHomeId := e.GetChipletId()
	// globalHomePort := e.RdmaRspPortByChipletId[globalHomeId]
	globalHomeClusterId := e.GetClusterId(globalHomeId, hotness)

	// local home node — address-hashed within the requestor's cluster
	localHomeId := e.GetLocalHomeId(requestorId, pAddr, hotness)
	localHomePort := e.RdmaRspPortByChipletId[localHomeId]

	// distance from home node to requestor and local home
	toRequestor := e.GetDistance(requestorPort.Component().(*DynamicClusteringRdmaEngine))
	toLocalHome := e.GetDistance(localHomePort.Component().(*DynamicClusteringRdmaEngine))

	shouldDistribute := (globalHomeClusterId != requestorClusterId) && // if src and dst are in the same cluster, do not distribute
		(localHomePort != requestorPort) && // original requestor and cluster home node should not be same
		!wasLookup && // if the request was from same cluster, do not distribute.
		(hotness <= protocol.Global && hotness > protocol.Cold) // if it's cold, do not distribute

	isCommonDst := (globalHomeClusterId != requestorClusterId) &&
		(localHomePort == requestorPort) &&
		!wasLookup && // if the request was from same cluster, do not distribute.
		(hotness <= protocol.Global && hotness > protocol.Cold)

	if shouldDistribute {
		branchNodeId := GetBranchNodeId(e.xDim, globalHomeId, localHomeId, requestorId)
		// DistributeRsp rides the (idle) Req network so its fan-out output
		// (DataReadyRsp on the Rsp network) never shares a VC with it. The
		// branch's Req endpoint intercepts and re-emits on the Rsp network.
		branchNodePort := e.RdmaReqPortByChipletId[branchNodeId]

		// distribution with PC, pAddr
		rspToOutside = protocol.DistributeRspBuilder{}.
			WithSendTime(now).
			WithSrc(e.RequestPort).
			WithBranchNodeId(branchNodeId).
			WithDst(branchNodePort).
			WithFirstDst(localHomePort).
			WithSecondDst(requestorPort).
			WithRspTo(trans.fromOutside.Meta().ID).
			WithData(rsp.Data).
			WithInfo(distInfo).
			Build()

		if toRequestor <= toLocalHome {
			rspToOutside.(*protocol.DistributeRsp).FirstDst = requestorPort
			rspToOutside.(*protocol.DistributeRsp).SecondDst = localHomePort
		}
	} else if isCommonDst {
		// logic when local home is same for origin dst.
		// send normal data ready rsp but need to denote as distribution response.
		rspToOutside = protocol.CommonDistRspBuilder{}.
			WithSendTime(now).
			WithSrc(e.ResponsePort).
			WithDst(requestorPort).
			WithRspTo(trans.fromOutside.Meta().ID).
			WithData(rsp.Data).
			WithInfo(distInfo).
			Build()
	} else {
		// no distribution
		rspToOutside = protocol.DataReadyRspBuilder{}.
			WithSendTime(now).
			WithSrc(e.ResponsePort).
			WithDst(requestorPort).
			WithRspTo(trans.fromOutside.Meta().ID).
			WithData(rsp.Data).
			WithInfo(distInfo).
			Build()
	}

	// DistributeRsp injects on the Req network (see above); all other
	// responses stay on the Rsp network.
	sendPort := e.ResponsePort
	if shouldDistribute {
		sendPort = e.RequestPort
	}
	networkMsg := &protocol.NetworkMsg{
		Msg:     rspToOutside,
		SrcPort: sendPort,
	}

	err := sendPort.Send(networkMsg)
	if err == nil {
		recvPort.(*akita.LimitNumMsgPort).RetrieveAt(now, idx)

		tracing.TraceReqFinalize(trans.toInside, now, e)
		tracing.TraceReqComplete(trans.fromOutside, now, e)
		emitRdmaFlightStartAt(rspToOutside,
			e.consumeFlightFirstSeen(rsp.Meta().ID, now), e)

		e.transactionsFromOutside =
			append(e.transactionsFromOutside[:transactionIndex],
				e.transactionsFromOutside[transactionIndex+1:]...)

		tracing.StartTask(0, 0, now, e,
			"hotness-trace",
			"", distInfo.Hotness)

		if wasLookup {
			// Lookup hit at this local home. Record at which probe it hit
			// (Hop) and whether the hitting LH sits in the requestor's own
			// cluster. globalHomeClusterId is this node's cluster, and a
			// lookup hit always lands on a local home (never the requestor),
			// so this cleanly splits own- vs other-cluster hits.
			lookupReq := trans.fromOutside.(*protocol.LookupReq)
			tracing.StartTask(0, 0, now, e,
				"lookup-outcome-hit", "",
				&tracing.LookupOutcome{
					// Real mesh-hop distance from the requestor to the hitting
					// LH (e). The chain runs on the minimal path, so this equals
					// the cumulative hops actually traversed — unlike Hop, which
					// only counted probe order (+1 per LH regardless of spacing).
					Distance: GetDistanceById(requestorId, e.GetChipletId(), e.xDim),
					// Classify own- vs other-cluster with the requestor's OWN
					// partition (carried on the LookupReq), not this hitting
					// node's local PCTable hotness — that node may never have run
					// this PC, which would collapse every hit to "other".
					SameCluster: e.GetClusterId(requestorId, lookupReq.Scheme) ==
						e.GetClusterId(globalHomeId, lookupReq.Scheme),
					IsPtw: lookupReq.IsPtw,
				})

			// Visit-sequence ending at the hitting local home (this node). The
			// address's global home is appended as "-g<id>" so the path shows
			// where the data is homed even on a hit; the "hit-" prefix (vs
			// "miss-") signals the lookup succeeded before reaching it. e itself
			// is the hitting LH, not the GH, so look the GH up from the address.
			visited := appendVisited(lookupReq.Visited, e.GetChipletId())
			ghId := e.RdmaReqAddressTable.Find(pAddr).
				Component().(*DynamicClusteringRdmaEngine).ChipletId
			tracing.StartTask(0, 0, now, e, "lookup-path", "",
				"hit-"+lookupPathStr(visited)+"-g"+strconv.Itoa(ghId))
		}

		var dst int
		if shouldDistribute {
			dst = localHomeId
		} else if isCommonDst {
			dst = requestorId
		}

		if shouldDistribute || isCommonDst {
			tracing.StartTask(0, 0, now, e,
				"distribution-per-pc",
				"", pc)
			tracing.StartTask(0, 0, now, e,
				"distribution-send-count",
				"", e.ChipletId)
			tracing.StartTask(0, 0, now, e,
				"distribution",
				"", dst)

			tracing.StartTask(0, 0, now, e,
				"distribution-trace",
				"",
				&tracing.DistInfo{
					Addr:         pAddr,
					PC:           pc,
					Home:         uint64(e.GetChipletId()),
					Hotness:      hotness,
					ToDistribute: dst,
					ToResponse:   requestorId,
					DistType:     tracing.TYPE_DIST, // normal distribution
				})

			e.PCTable.Distribute(pc)
		}

		return true
	} else {
		if global.GetInstance().GetVerbose() {
			fmt.Println("Error on processDataReadyRspFromL2, ",
				e.Name(),
				rspToOutside.Meta().Src.Name(),
				rspToOutside.Meta().Dst.Name(),
				rsp.Meta().ID,
			)
		}
	}

	return false
}

func (e *DynamicClusteringRdmaEngine) processRspFromOutside(now akita.VTimeInSec) bool {
	madeProgress := false
	port := e.ResponsePort.(*akita.LimitNumMsgPort)
	for i := 0; i < e.numTransPerCycle; i++ {
		rsp, idx, ok := port.PeekReady(e.canRouteDynClusteringRspFromOutside)
		if !ok {
			if port.Peek() != nil {
				e.rspStall++
				if e.rspStall == 1000000 {
					e.dumpRspStall(now)
				}
			}
			return madeProgress
		}
		e.rspStall = 0

		switch rsp := rsp.(type) {
		// case *protocol.DistributeRsp:
		// 	madeProgress = e.processDistributeRspFromOutside(now, rsp) || madeProgress
		case *protocol.LocalHomeRsp:
			madeProgress = e.processLocalHomeRspFromOutside(now, rsp, idx) || madeProgress
		case *protocol.TransDataReadyRsp:
			madeProgress = e.processTransRspFromOutside(now, rsp, idx) || madeProgress
		case *protocol.DataReadyRsp:
			madeProgress = e.processDataReadyRspFromOutside(now, rsp, idx) || madeProgress
		case *protocol.CommonDistRsp:
			madeProgress = e.processCommonDistRspFromOutside(now, rsp, idx) || madeProgress
		case *protocol.WriteDoneRsp:
			madeProgress = e.processWriteDoneRspFromOutside(now, rsp, idx) || madeProgress
		default:
			log.Panicf("cannot process response of type %s", reflect.TypeOf(rsp))
			return false
		}
	}
	return madeProgress
}

// dumpRspStall prints every response parked in ResponsePort with its resolved
// L2/L1 destination and that destination's CanSend, then panics. Fires when
// responses cannot eject for a long stretch (the corner-ejection livelock).
func (e *DynamicClusteringRdmaEngine) dumpRspStall(now akita.VTimeInSec) {
	port := e.ResponsePort.(*akita.LimitNumMsgPort)
	fmt.Printf("[rsp-stall] %s ResponsePort eject-blocked at %.10f; pending:\n",
		e.Name(), now)
	n := 0
	port.PeekReady(func(m akita.Msg) bool {
		if n < 30 {
			e.logOneStuckRsp(m)
			n++
		}
		return false
	})
	panic(fmt.Sprintf("[rsp-stall] %s: responses cannot eject to L2/L1", e.Name()))
}

func (e *DynamicClusteringRdmaEngine) logOneStuckRsp(m akita.Msg) {
	var dst akita.Port
	kind := reflect.TypeOf(m).String()
	switch rsp := m.(type) {
	case *protocol.DataReadyRsp:
		txIdx := e.findTransactionByRspToID(rsp.GetRespondTo(), e.transactionsFromInside)
		if txIdx == -1 {
			if di, ok := rsp.Info.(*protocol.DistInfo); ok {
				dst = e.L2BottomFinder.Find(di.PAddr)
			}
		} else {
			dst = e.transactionsFromInside[txIdx].fromInside.Meta().Src
		}
	case *protocol.CommonDistRsp:
		txIdx := e.findTransactionByRspToID(rsp.GetRespondTo(), e.transactionsFromInside)
		if txIdx != -1 {
			pAddr := e.transactionsFromInside[txIdx].fromInside.(protocol.AccessReq).GetAddress()
			dst = e.L2BottomFinder.Find(pAddr)
		}
	case *protocol.WriteDoneRsp:
		txIdx := e.findTransactionByRspToID(rsp.GetRespondTo(), e.transactionsFromInside)
		if txIdx != -1 {
			dst = e.transactionsFromInside[txIdx].fromInside.Meta().Src
		}
	case *protocol.LocalHomeRsp:
		if pAddr, ok := rsp.Info.(uint64); ok {
			dst = e.L2BottomFinder.Find(pAddr)
		}
	case *protocol.TransDataReadyRsp:
		txIdx := e.findTransactionByRspToID(rsp.GetRespondTo(), e.transactionsFromInside)
		if txIdx != -1 {
			dst = e.transactionsFromInside[txIdx].fromInside.Meta().Src
		}
	}
	dstName, canSend := "nil", false
	if dst != nil {
		dstName = dst.Name()
		if cs, ok := dst.(canSender); ok {
			canSend = cs.CanSend(1)
		}
	}
	fmt.Printf("[rsp-stall]   %s dst=%s CanSend=%v\n", kind, dstName, canSend)
}

// canRouteDynClusteringRspFromOutside reports whether the per-bank
// destination resolved for this response has buffer room. Routing depends on
// the response type (PwPort for translation, ToL1Bottom or per-bank
// ToL2Bottoms[i] for data).
func (e *DynamicClusteringRdmaEngine) canRouteDynClusteringRspFromOutside(m akita.Msg) bool {
	switch rsp := m.(type) {
	case *protocol.TransDataReadyRsp:
		txIdx := e.findTransactionByRspToID(rsp.GetRespondTo(), e.transactionsFromInside)
		dst := e.transactionsFromInside[txIdx].fromInside.Meta().Src
		return e.dstReadyVia(e.PwPort, dst)
	case *protocol.LocalHomeRsp:
		pAddr, ok := rsp.Info.(uint64)
		if !ok {
			return false
		}
		dst := e.L2BottomFinder.Find(pAddr)
		return e.dstReadyVia(e.srcPortForL2BottomDst(dst), dst)
	case *protocol.CommonDistRsp:
		txIdx := e.findTransactionByRspToID(rsp.GetRespondTo(), e.transactionsFromInside)
		pAddr := e.transactionsFromInside[txIdx].fromInside.(protocol.AccessReq).GetAddress()
		dst := e.L2BottomFinder.Find(pAddr)
		return e.dstReadyVia(e.srcPortForL2BottomDst(dst), dst)
	case *protocol.DataReadyRsp:
		txIdx := e.findTransactionByRspToID(rsp.GetRespondTo(), e.transactionsFromInside)
		var dst akita.Port
		if txIdx == -1 {
			distInfo, ok := rsp.Info.(*protocol.DistInfo)
			if !ok {
				panic("expect dist info in Dist.Info")
			}
			dst = e.L2BottomFinder.Find(distInfo.PAddr)
		} else {
			dst = e.transactionsFromInside[txIdx].fromInside.Meta().Src
		}
		return e.dstReadyVia(e.srcPortForL2BottomDst(dst), dst)
	case *protocol.WriteDoneRsp:
		txIdx := e.findTransactionByRspToID(rsp.GetRespondTo(), e.transactionsFromInside)
		dst := e.transactionsFromInside[txIdx].fromInside.Meta().Src
		src := e.srcPortForL2BottomDst(dst)
		if strings.Contains(dst.Name(), "L1") {
			src = e.ToL1Bottom
		}
		return e.dstReadyVia(src, dst)
	}
	return false
}

func (e *DynamicClusteringRdmaEngine) processWriteDoneRspFromOutside(
	now akita.VTimeInSec,
	rsp *protocol.WriteDoneRsp,
	idx int,
) bool {
	transactionIndex := e.findTransactionByRspToID(
		rsp.GetRespondTo(), e.transactionsFromInside)

	// get the orignial request
	trans := e.transactionsFromInside[transactionIndex]

	var srcPort akita.Port
	srcName := trans.fromInside.Meta().Src.Name()
	if strings.Contains(srcName, "L1") {
		srcPort = e.ToL1Bottom
	} else {
		srcPort = e.srcPortForL2BottomDst(trans.fromInside.Meta().Src)
	}

	rspToInside := e.cloneRsp(rsp, trans.fromInside.Meta().ID)
	rspToInside.Meta().SendTime = now
	rspToInside.Meta().Src = srcPort
	rspToInside.Meta().Dst = trans.fromInside.Meta().Src

	err := srcPort.Send(rspToInside)
	if err == nil {
		e.ResponsePort.(*akita.LimitNumMsgPort).RetrieveAt(now, idx)

		tracing.TraceReqFinalize(trans.toOutside, now, e)
		tracing.TraceReqComplete(trans.fromInside, now, e)

		tracing.EndTask(
			rsp.GetRespondTo(),
			now,
			rsp.Meta().Src.Component().(RDMAEngine))

		e.transactionsFromInside =
			append(e.transactionsFromInside[:transactionIndex],
				e.transactionsFromInside[transactionIndex+1:]...)

		e.outstandingReqs--

		return true
	}
	if global.GetInstance().GetVerbose() {
		fmt.Println("Error on processWriteDoneRspFromOutside, ",
			e.Name(),
			rspToInside.Meta().Src.Name(),
			rspToInside.Meta().Dst.Name(),
			rsp.Meta().ID,
		)
	}

	return false
}

func (e *DynamicClusteringRdmaEngine) processCommonDistRspFromOutside(
	now akita.VTimeInSec,
	rsp *protocol.CommonDistRsp,
	idx int,
) bool {
	var trans transaction
	var rspToInside akita.Msg
	var srcPort akita.Port

	distInfo, ok := rsp.Info.(*protocol.DistInfo)
	if !ok {
		panic("expect dist info in Dist.Info")
	}

	// Piggyback merge: fold the home's classified hotness into this
	// requestor's local PCTable so future ReqFromInside queries can
	// gate lookups even for PCs whose addresses are never homed here.
	e.PCTable.MergeRemoteHotness(distInfo.PC, distInfo.Hotness)

	transactionIndex := e.findTransactionByRspToID(
		rsp.GetRespondTo(), e.transactionsFromInside)

	// get the orignial request
	trans = e.transactionsFromInside[transactionIndex]

	// srcPort = e.ToL2Bottom // need to cache the data into local home, l2 cache.
	pAddr := trans.fromInside.(protocol.AccessReq).GetAddress()

	dstPort := e.L2BottomFinder.Find(pAddr)
	srcPort = e.srcPortForL2BottomDst(dstPort)

	// if the request is from L2, send data ready rsp to l2 cache.
	// there's no need to send the data to l1 cache.
	rspToInside = protocol.CommonDistRspBuilder{}.
		WithSendTime(now).
		WithSrc(srcPort).
		WithDst(dstPort).
		WithData(rsp.Data).
		WithInfo(rsp.Info).
		WithRspTo(trans.fromInside.Meta().ID).
		Build()

	err := srcPort.Send(rspToInside)
	if err == nil {
		e.ResponsePort.(*akita.LimitNumMsgPort).RetrieveAt(now, idx)

		tracing.TraceReqFinalize(trans.toOutside, now, e)
		tracing.TraceReqComplete(trans.fromInside, now, e)

		tracing.EndTask(
			rsp.GetRespondTo(),
			now,
			rsp.Meta().Src.Component().(RDMAEngine))

		e.transactionsFromInside =
			append(e.transactionsFromInside[:transactionIndex],
				e.transactionsFromInside[transactionIndex+1:]...)

		e.outstandingReqs--

		return true
	}
	if global.GetInstance().GetVerbose() {
		fmt.Println("Error on processCommonDistRspFromOutside, ",
			e.Name(),
			rspToInside.Meta().Src.Name(),
			rspToInside.Meta().Dst.Name(),
			rsp.Meta().ID,
		)
	}

	return false
}

func (e *DynamicClusteringRdmaEngine) processDataReadyRspFromOutside(
	now akita.VTimeInSec,
	rsp *protocol.DataReadyRsp,
	idx int,
) bool {
	transactionIndex := e.findTransactionByRspToID(
		rsp.GetRespondTo(), e.transactionsFromInside)

	var trans transaction
	var rspToInside akita.Msg
	var src akita.Port

	// Piggyback merge: every DataReadyRsp from outside carries the
	// home's classified hotness in DistInfo. Fold it into this
	// chiplet's local PCTable (raise-only) so the requestor / LH can
	// gate future lookups even for PCs whose addresses are not homed
	// here.
	if di, ok := rsp.Info.(*protocol.DistInfo); ok && di != nil {
		e.PCTable.MergeRemoteHotness(di.PC, di.Hotness)
	}

	if transactionIndex == -1 {
		distInfo, ok := rsp.Info.(*protocol.DistInfo)
		if !ok {
			panic("expect physical address in DataReadyRsp.Info")
		}
		pAddr := distInfo.PAddr
		dst := e.L2BottomFinder.Find(pAddr)
		src = e.srcPortForL2BottomDst(dst)

		rspToInside = protocol.LocalHomeRspBuilder{}.
			WithSendTime(now).
			WithSrc(src).
			WithDst(dst).
			WithData(rsp.Data).
			WithInfo(distInfo).
			WithRspTo(0).
			Build()
	} else {
		// normal data ready response without distribution
		trans = e.transactionsFromInside[transactionIndex]
		_, wasLookup := trans.toOutside.(*protocol.LookupReq) // check if original req was lookup.

		// Per-bank L2 bottom src — replaces the legacy single ToL2Bottom.
		// The hotness-Bypass/Streaming preprocessing that used to live
		// here has been superseded by the sensitivity-aware switch below
		// (Plan v4); keep only the src resolution that the new branch
		// also needs.
		src = e.srcPortForL2BottomDst(trans.fromInside.Meta().Src)
		rspToInside = e.cloneRsp(rsp, trans.fromInside.Meta().ID)
		rspToInside.Meta().SendTime = now
		rspToInside.Meta().Src = src
		rspToInside.Meta().Dst = trans.fromInside.Meta().Src

		di := rsp.Info.(*protocol.DistInfo)
		adjusted := di.Hotness

		// Lookup-fetched data has no settled hotness — install as Bypass.
		if wasLookup {
			// adjusted = protocol.Bypass
			adjusted = protocol.Cold
		}

		rspToInside.(*protocol.DataReadyRsp).Info.(*protocol.DistInfo).Hotness = adjusted
	}

	err := src.Send(rspToInside)
	if err == nil {
		e.ResponsePort.(*akita.LimitNumMsgPort).RetrieveAt(now, idx)

		if transactionIndex != -1 {
			emitRdmaFlightEnd(rsp, now, e)

			tracing.TraceReqFinalize(trans.toOutside, now, e)
			tracing.TraceReqComplete(trans.fromInside, now, e)

			tracing.EndTask(
				rsp.GetRespondTo(),
				now,
				rsp.Meta().Src.Component().(RDMAEngine))

			e.transactionsFromInside =
				append(e.transactionsFromInside[:transactionIndex],
					e.transactionsFromInside[transactionIndex+1:]...)

			e.outstandingReqs--
		}

		return true
	}
	if global.GetInstance().GetVerbose() {
		fmt.Println("Error on processDataReadyRspFromOutside, ",
			e.Name(),
			rspToInside.Meta().Src.Name(),
			rspToInside.Meta().Dst.Name(),
			rsp.Meta().ID,
		)
	}

	return false
}

func (e *DynamicClusteringRdmaEngine) processLocalHomeRspFromOutside(
	now akita.VTimeInSec,
	rsp *protocol.LocalHomeRsp,
	idx int,
) bool {
	pAddr, ok := rsp.Info.(uint64)
	if !ok {
		panic("expect physical address in LocalHomeRsp.Info")
	}

	dstPort := e.L2BottomFinder.Find(pAddr)
	srcPort := e.srcPortForL2BottomDst(dstPort)

	msg := protocol.LocalHomeRspBuilder{}.
		WithSendTime(now).
		WithSrc(srcPort).
		WithDst(dstPort).
		WithData(rsp.Data).
		WithInfo(pAddr).
		WithRspTo(0).
		Build()

	err := srcPort.Send(msg)
	if err == nil {
		e.ResponsePort.(*akita.LimitNumMsgPort).RetrieveAt(now, idx)
		return true
	}

	return false
}

func (e *DynamicClusteringRdmaEngine) processReqFromOutside(
	now akita.VTimeInSec,
) bool {
	madeProgress := false
	port := e.RequestPort.(*akita.LimitNumMsgPort)
	for i := 0; i < e.numTransPerCycle; i++ {
		req, idx, ok := port.PeekReady(e.canRouteDynClusteringReqFromOutside)
		if !ok {
			return madeProgress
		}

		switch typedReq := req.(type) {
		case *protocol.LookupReq:
			madeProgress = e.processLookupReqFromOutside(now, typedReq, idx) || madeProgress
		case *protocol.TransReadReq:
			madeProgress = e.processTransReqFromOutside(now, typedReq, idx) || madeProgress
		case *protocol.WriteReq: // write requests are only generated by eviction from llc
			madeProgress = e.processEvictReqFromOutside(now, typedReq, idx) || madeProgress
		case *protocol.ReadReq:
			madeProgress = e.processReadReqFromOutside(now, typedReq, idx) || madeProgress
		default:
			log.Panicf("cannot process request of type %s", reflect.TypeOf(req))
			return false
		}
	}
	return madeProgress
}

// canRouteDynClusteringReqFromOutside reports whether the per-bank
// destination (PwPort for translation, ToL2RDMAs[i] for read/lookup,
// ToDramTops[i] for evict) has buffer room.
func (e *DynamicClusteringRdmaEngine) canRouteDynClusteringReqFromOutside(m akita.Msg) bool {
	switch req := m.(type) {
	case *protocol.LookupReq:
		dst := e.localModuleFinder.Find(req.GetAddress())
		return e.dstReadyVia(e.srcPortForL2Dst(dst), dst)
	case *protocol.TransReadReq:
		dst := e.translationModuleFinder.Find(req.GetAddress())
		return e.dstReadyVia(e.srcPortForDramDst(dst), dst)
	case *protocol.WriteReq:
		dst := e.translationModuleFinder.Find(req.GetAddress())
		return e.dstReadyVia(e.srcPortForDramDst(dst), dst)
	case *protocol.ReadReq:
		dst := e.localModuleFinder.Find(req.GetAddress())
		return e.dstReadyVia(e.srcPortForL2Dst(dst), dst)
	}
	return false
}

func (e *DynamicClusteringRdmaEngine) processReadReqFromOutside(
	now akita.VTimeInSec,
	req *protocol.ReadReq,
	idx int,
) bool {
	pc, _ := req.Info.(uint64) // for instruction fetch, the info is nil.

	dst := e.localModuleFinder.Find(req.GetAddress()) // rdma port of l2 cache
	src := e.srcPortForL2Dst(dst)

	cloned := e.cloneReq(req)
	cloned.Meta().SendTime = now
	cloned.Meta().Src = src
	cloned.Meta().Dst = dst // rdma port of local L2 cache.
	cloned.(*protocol.ReadReq).Info = req.Info
	cloned.(*protocol.ReadReq).IsPtw = req.IsPtw

	err := src.Send(cloned)
	if err == nil {
		e.RequestPort.(*akita.LimitNumMsgPort).RetrieveAt(now, idx)

		emitRdmaFlightEnd(req, now, e)

		tracing.TraceReqReceive(req, now, e)
		tracing.TraceReqInitiate(cloned, now, e,
			tracing.MsgIDAtReceiver(req, e))

		trans := transaction{
			fromOutside: req,
			toInside:    cloned,
		}
		e.transactionsFromOutside = append(e.transactionsFromOutside, trans)

		requestorId := e.RdmaRspAddressTable[req.Meta().Src].
			Component().(*DynamicClusteringRdmaEngine).
			GetChipletId()

		hop := e.GetDistance(e.RdmaRspAddressTable[req.Meta().Src].
			Component().(*DynamicClusteringRdmaEngine))

		e.PCTable.ReqFromOutside(
			pc,
			req.Address,
			requestorId,
			// 0, // FIXME: need to consider the weighted update for CMS
			hop)

		// Stamp the sim-time of this PC's first hotness increase (warm-up
		// signal). Only the runtime PCTable tracks this; oracle/simple
		// tables are skipped via the type switch. The shared table embeds
		// a *PCTable so StampFirstIncrease is promoted.
		switch pt := e.PCTable.(type) {
		case *PCTable:
			pt.StampFirstIncrease(pc, float64(now))
		case *SharedPCTable:
			pt.StampFirstIncrease(pc, float64(now))
		}

		return true
	}

	if global.GetInstance().GetVerbose() {
		fmt.Println("Error on processReadReqFromOutside, ",
			e.Name(),
			cloned.Meta().Src.Name(),
			cloned.Meta().Dst.Name(),
			req.Meta().ID,
		)
	}

	return false
}

func (e *DynamicClusteringRdmaEngine) processLookupReqFromOutside(
	now akita.VTimeInSec,
	req *protocol.LookupReq,
	idx int,
) bool {
	// _, _ := req.Info.(uint64)
	// if !ok {
	// 	panic("lookup req should include PC")
	// }

	dst := e.localModuleFinder.Find(req.GetAddress()) // rdma port of l2 cache
	src := e.srcPortForL2Dst(dst)

	cloned := protocol.LookupReqBuilder{}.
		WithSendTime(now).
		WithSrc(src).
		WithDst(dst).
		WithAddress(req.GetAddress()).
		WithByteSize(req.AccessByteSize).
		WithInfo(req.Info).
		Build()

	cloned.IsPtw = req.IsPtw

	err := src.Send(cloned)
	if err == nil {
		e.RequestPort.(*akita.LimitNumMsgPort).RetrieveAt(now, idx)

		emitRdmaFlightEnd(req, now, e)

		tracing.TraceReqReceive(req, now, e)
		tracing.TraceReqInitiate(cloned, now, e,
			tracing.MsgIDAtReceiver(req, e))

		trans := transaction{
			fromOutside: req,
			toInside:    cloned,
		}
		e.transactionsFromOutside = append(e.transactionsFromOutside, trans)

		if global.GetInstance().GetVerbose() {
			fmt.Println("Receive, ",
				e.Name(),
				req.Meta().Src.Name(),
				req.Meta().Dst.Name(),
				req.Meta().ID,
				now,
			)
		}

		return true
	}
	if global.GetInstance().GetVerbose() {
		fmt.Println("Error on processLookupReqFromOutside, ",
			e.Name(),
			cloned.Meta().Src.Name(),
			cloned.Meta().Dst.Name(),
			req.Meta().ID,
		)

	}

	return false
}

func (e *DynamicClusteringRdmaEngine) findTransactionByRspToID(
	rspTo uint64,
	transactions []transaction,
) int {
	for i, trans := range transactions {
		if trans.toOutside != nil && trans.toOutside.Meta().ID == rspTo {
			return i
		}

		if trans.toInside != nil && trans.toInside.Meta().ID == rspTo {
			return i
		}
	}

	return -1
}

func (e *DynamicClusteringRdmaEngine) Reset() {
	e.outstandingReqs = 0

	e.transactionsFromInside = []transaction{}
	e.transactionsFromOutside = []transaction{}

	e.PCTable.Reset()
}

// NewEngine creates new engine
func NewDynamicClusteringRdmaEngine(
	name string,
	engine akita.Engine,
	numTransPerCycle int,
	clusterId int,
	clusterSize int,
	xDim int,
	yDim int,
) RDMAEngine {
	e := new(DynamicClusteringRdmaEngine)
	e.TickingComponent = akita.NewTickingComponent(
		name,
		engine,
		1*akita.GHz,
		e)

	e.ToL2RDMA = akita.NewLimitNumMsgPort(e, 16*numTransPerCycle, name+".ToL2RDMA")
	e.ToL2RDMAs = []akita.Port{e.ToL2RDMA}
	e.destToL2Idx = map[akita.Port]int{}
	e.ToL1Bottom = akita.NewLimitNumMsgPort(e, 16*numTransPerCycle, name+".ToL1Bottom")
	e.ToL2Bottom = akita.NewLimitNumMsgPort(e, 16*numTransPerCycle, name+".ToL2Bottom")
	e.ToL2Bottoms = []akita.Port{e.ToL2Bottom}
	e.destToL2BottomIdx = map[akita.Port]int{}
	e.ToDramTop = akita.NewLimitNumMsgPort(e, 16*numTransPerCycle, name+".ToDramTop")
	e.ToDramTops = []akita.Port{e.ToDramTop}
	e.destToDramIdx = map[akita.Port]int{}
	e.CtrlPort = akita.NewLimitNumMsgPort(e, numTransPerCycle, name+".CtrlPort")
	e.PwPort = akita.NewLimitNumMsgPort(e, 16*numTransPerCycle, name+".PwPort")

	e.RequestPort = akita.NewLimitNumMsgPort(e, 16*numTransPerCycle, name+".RequestPort")
	e.ResponsePort = akita.NewLimitNumMsgPort(e, 16*numTransPerCycle, name+".ResponsePort")

	e.numTransPerCycle = numTransPerCycle

	e.clusterSize = clusterSize
	e.clusterMap = GetClusterMap(clusterSize)

	if global.GetInstance().GetUseSharedPCTable() {
		e.PCTable = NewSharedPCTable(e, xDim, yDim)
		e.sharedHotnessSync = true
	} else if global.GetInstance().GetUseOraclePCTable() {
		e.PCTable = NewOraclePCTable(e, xDim, yDim)
	} else {
		e.PCTable = NewPCTable(e, xDim, yDim)
	}

	e.lastTime = 0.0
	e.samplingPeriod = 10000.0

	e.retryPort = akita.NewLimitNumMsgPort(e, 100000000, name+".RetryPort")

	return e
}

func (e *DynamicClusteringRdmaEngine) GetClusterId(
	chipletId int,
	hotness protocol.Hotness,
) int {
	wX, wY := clusterDims(hotness, e.xDim)
	if wX == 0 {
		return chipletId // non-distributable (Cold / Bypass / …): own chiplet
	}

	clustersPerRow := e.xDim / wX
	row := chipletId / e.xDim
	col := chipletId % e.xDim

	return (row/wY)*clustersPerRow + col/wX
}

// hashAddrForLocalHome mixes the address bits to produce a well-distributed
// 64-bit value used to pick an offset inside a cluster. Simple multiplicative
// hash (golden-ratio prime) followed by an xor-shift. Deterministic, so the
// same address always maps to the same in-cluster offset.
func hashAddrForLocalHome(addr uint64) uint64 {
	h := addr * 11400714819323198485 // 2^64 / phi
	h ^= h >> 32
	return h
}

// clusterDims returns the cluster rectangle (wX columns × wY rows) used to
// distribute a PC of the given hotness on an n×n mesh (n = xDim = yDim).
// Width is fixed at 2; height carries the tier, so the area ladder is
// Global 2×1 (area 2) < Hot 2×2 (4) < Warm 2×p (2p) — hotter → smaller
// cluster → more replicas. p = smallest divisor of n above 2, so the
// cluster always tiles the mesh: 6→2×3, 8→2×4, 10→2×5, 4→2×4. Returns
// (0,0) for non-distributable hotness (Cold / Bypass / Streaming / NonShared).
func clusterDims(h protocol.Hotness, n int) (wX, wY int) {
	switch h {
	case protocol.Global:
		return 2, 1
	case protocol.Hot:
		return 2, 2
	case protocol.Warm:
		return 2, smallestDivisorAbove(n, 2)
	}
	return 0, 0
}

// smallestDivisorAbove returns the smallest divisor of n strictly greater
// than k (n itself if none exists). Sizes the Warm cluster's long axis so
// it divides the mesh on any n.
func smallestDivisorAbove(n, k int) int {
	for d := k + 1; d <= n; d++ {
		if n%d == 0 {
			return d
		}
	}
	return n
}

// numClustersForHotness returns the total number of clusters tiling the
// mesh under the given hotness scheme. e.g. 6×6: Global 2×1 → 18 clusters,
// Hot 2×2 → 9, Warm 2×3 → 6. Returns 0 for non-distributable hotness.
func (e *DynamicClusteringRdmaEngine) numClustersForHotness(
	hotness protocol.Hotness,
) int {
	wX, wY := clusterDims(hotness, e.xDim)
	if wX == 0 {
		return 0
	}
	return (e.xDim / wX) * (e.xDim / wY)
}

// GetLocalHomeIdForCluster returns the local home chiplet of the cluster
// identified by `clusterId` for the given (address, hotness). Identical
// to GetLocalHomeId but takes the cluster ID directly instead of deriving
// it from a requestor — used when enumerating LH candidates across all
// clusters of a given hotness scheme.
//
// For non-distributable hotness it falls back to the global home of `addr`.
func (e *DynamicClusteringRdmaEngine) GetLocalHomeIdForCluster(
	clusterId int,
	addr uint64,
	hotness protocol.Hotness,
) int {
	wX, wY := clusterDims(hotness, e.xDim)
	if wX == 0 {
		// No distribution for cold (or unknown) data — same chiplet that
		// owns the address acts as both global and "local" home.
		globalHome := e.RdmaReqAddressTable.Find(addr)
		return globalHome.Component().(*DynamicClusteringRdmaEngine).ChipletId
	}

	clustersPerRow := e.xDim / wX

	// cluster 시작 좌표 (직사각형: X는 wX, Y는 wY 간격으로 타일링)
	clusterStartX := (clusterId % clustersPerRow) * wX
	clusterStartY := (clusterId / clustersPerRow) * wY

	// POSITION-BASED placement: mirror the global home's intra-cluster
	// offset onto this cluster. This keeps the local home on the
	// requestor->globalHome geodesic — exactly the path the lookup chain
	// probes (IsOnMinimalPath) — so a later lookup finds the distributed
	// copy far more often than address-hash placement, which scatters the
	// local home to a path-independent slot. General over any (wX,wY) /
	// xDim (no per-size hardcoding the legacy version needed).
	globalHome := e.RdmaReqAddressTable.Find(addr)
	ghID := globalHome.Component().(*DynamicClusteringRdmaEngine).ChipletId
	offsetX := (ghID % e.xDim) % wX
	offsetY := (ghID / e.xDim) % wY

	mappedX := clusterStartX + offsetX
	mappedY := clusterStartY + offsetY

	return mappedY*e.xDim + mappedX
}

// GetLocalHomeId returns the local home chiplet within the *requestor's*
// cluster for the given (address, hotness). Thin wrapper that derives the
// requestor's cluster ID and defers placement to GetLocalHomeIdForCluster.
func (e *DynamicClusteringRdmaEngine) GetLocalHomeId(
	requestorId int,
	addr uint64,
	hotness protocol.Hotness,
) int {
	return e.GetLocalHomeIdForCluster(
		e.GetClusterId(requestorId, hotness), addr, hotness)
}

// Legacy position-based GetLocalHomeId — preserved for reference. The local
// home was picked by mirroring the global home's offset within its cluster
// onto the requestor's cluster, which kept the local home on the
// src→globalHome geodesic but coupled placement to globalHome geometry.
//
// FIXME: too complex function, also cannot be used across different cluster size and mesh size.
// func (e *DynamicClusteringRdmaEngine) GetLocalHomeId(
// 	requestorId int,
// 	globalHomeId int,
// 	hotness protocol.Hotness,
// ) int {
// 	var clusterWidth int
// 	switch hotness {
// 	case protocol.Warm:
// 		switch e.xDim {
// 		case 4:
// 			clusterWidth = 2
// 		case 6:
// 			clusterWidth = 3
// 		case 8:
// 			clusterWidth = 4
// 		default:
// 			panic("unsupported dimension")
// 		}
// 	case protocol.Hot:
// 		switch e.xDim {
// 		case 4:
// 			clusterWidth = 2
// 		case 6:
// 			clusterWidth = 2
// 		case 8:
// 			clusterWidth = 2
// 		default:
// 			panic("unsupported dimension")
// 		}
// 	case protocol.Global:
// 		switch e.xDim {
// 		case 4:
// 			clusterWidth = 2
// 		case 6:
// 			clusterWidth = 2
// 		case 8:
// 			clusterWidth = 2
// 		default:
// 			panic("unsupported dimension")
// 		}
// 	default: // no need to deal with cold data since it will not be distributed.
// 		return globalHomeId
// 	}
// 	requestorClusterId := e.GetClusterId(requestorId, hotness)

// 	// 1. global home 칩렛의 절대 좌표 계산
// 	globalHomeX := globalHomeId % e.xDim
// 	globalHomeY := globalHomeId / e.xDim

// 	// 2. global home의 클러스터 내 상대 좌표 (Local Offset) 계산
// 	globalHomeInClusterX := globalHomeX % clusterWidth
// 	globalHomeInClusterY := globalHomeY % clusterWidth

// 	// 3. requestor 클러스터의 시작 좌표 (Origin) 계산
// 	requestorClusterStartX := (requestorClusterId % (e.xDim / clusterWidth)) * clusterWidth
// 	requestorClusterStartY := (requestorClusterId / (e.xDim / clusterWidth)) * clusterWidth

// 	// 4. global home에 매칭되는 requestor 클러스터 내 절대 좌표 계산
// 	mappedX := requestorClusterStartX + globalHomeInClusterX
// 	mappedY := requestorClusterStartY + globalHomeInClusterY

// 	return mappedY*e.xDim + mappedX
// }

func (e *DynamicClusteringRdmaEngine) GetDistance(
	dstChip RDMAEngine,
) int {
	y1 := dstChip.GetY()
	x1 := dstChip.GetX()
	y2 := e.GetY()
	x2 := e.GetX()

	hop := Abs(y1-y2) + Abs(x1-x2)

	// DO NOT DELETE
	// if the localHomeNode is same as the requestor, the distance is 0,
	// but this incur bug, so return distance as 1000.
	if hop == 0 {
		return 1000
	}

	return Abs(y1-y2) + Abs(x1-x2)
}
