package protocol

import (
	"gitlab.com/akita/akita"
	"gitlab.com/akita/util/ca"
)

// var hashedPCOverhead = 1
// var phyAddrOverhead = 6
var metadataByteOverhead = 7

type Hotness int

const (
	Streaming      Hotness = -2
	Bypass         Hotness = -1
	Cold           Hotness = 0
	Warm           Hotness = 1
	Hot            Hotness = 2
	Global         Hotness = 3
	Warm_NonShared Hotness = 4
	Hot_NonShared  Hotness = 5
)

// only used when local home and original destination are same.
type CommonDistInfo struct {
	PC      uint64
	Hotness Hotness
}

type DistInfo struct {
	PC       uint64
	Hotness  Hotness
	PAddr    uint64
	IsSecond bool
	IsWrite  bool
	// IsPtw indicates the originating L2 fetch was a page-table walk
	// (the request reaching RDMA had IsPtw=true). Used by downstream
	// L2 install paths to prefer PTE over plain data on hotness ties
	// when picking the resident line to keep.
	IsPtw bool
}

type DistributeRsp struct {
	akita.MsgMeta

	BranchNodeId int
	FirstDst     akita.Port
	SecondDst    akita.Port
	RespondTo    uint64 // The ID of the request it replies
	Data         []byte
	IsSecond     bool
	PAddr        uint64
	Info         interface{} // should be PCState

	LocalHomeList []akita.Port
}

// Meta returns the meta data attached to each message.
func (r *DistributeRsp) Meta() *akita.MsgMeta {
	return &r.MsgMeta
}

// GetRespondTo returns the ID if the request that the respond is resonding to.
func (r *DistributeRsp) GetRespondTo() uint64 {
	return r.RespondTo
}

type DistributeRspBuilder struct {
	sendTime akita.VTimeInSec
	src, dst akita.Port
	rspTo    uint64
	data     []byte
	info     interface{}

	branchNodeId int
	firstDst     akita.Port
	secondDst    akita.Port

	// for prefetch
	localHomeList []akita.Port
}

func (b DistributeRspBuilder) WithSendTime(
	t akita.VTimeInSec,
) DistributeRspBuilder {
	b.sendTime = t
	return b
}

// WithSrc sets the source of the request to build.
func (b DistributeRspBuilder) WithSrc(src akita.Port) DistributeRspBuilder {
	b.src = src
	return b
}

// WithDst sets the destination of the request to build.
func (b DistributeRspBuilder) WithDst(dst akita.Port) DistributeRspBuilder {
	b.dst = dst
	return b
}

// WithRspTo sets ID of the request that the respond to build is replying to.
func (b DistributeRspBuilder) WithRspTo(id uint64) DistributeRspBuilder {
	b.rspTo = id
	return b
}

// WithData sets the data of the request to build.
func (b DistributeRspBuilder) WithData(data []byte) DistributeRspBuilder {
	b.data = data
	return b
}

// WithInfo sets the info of the request to build.
func (b DistributeRspBuilder) WithInfo(info interface{}) DistributeRspBuilder {
	b.info = info
	return b
}

func (b DistributeRspBuilder) WithFirstDst(p akita.Port) DistributeRspBuilder {
	b.firstDst = p
	return b
}

func (b DistributeRspBuilder) WithSecondDst(p akita.Port) DistributeRspBuilder {
	b.secondDst = p
	return b
}

func (b DistributeRspBuilder) WithBranchNodeId(id int) DistributeRspBuilder {
	b.branchNodeId = id
	return b
}

func (b DistributeRspBuilder) WithLocalHomeList(list []akita.Port) DistributeRspBuilder {
	b.localHomeList = list
	return b
}

func (b DistributeRspBuilder) Build() *DistributeRsp {
	r := &DistributeRsp{}
	r.ID = akita.GetIDGenerator().Generate()
	r.Src = b.src
	r.Dst = b.dst
	r.SendTime = b.sendTime
	r.TrafficBytes = len(b.data) + accessRspByteOverhead + metadataByteOverhead
	r.RespondTo = b.rspTo
	r.Data = b.data
	r.Info = b.info
	r.BranchNodeId = b.branchNodeId
	r.FirstDst = b.firstDst
	r.SecondDst = b.secondDst
	r.LocalHomeList = b.localHomeList
	return r
}

type LocalHomeRsp struct {
	akita.MsgMeta

	PAddr     uint64
	RespondTo uint64 // The ID of the request it replies
	Data      []byte
	Info      interface{}
	OriginDst akita.Port
}

// Meta returns the meta data attached to each message.
func (r *LocalHomeRsp) Meta() *akita.MsgMeta {
	return &r.MsgMeta
}

// GetRespondTo returns the ID if the request that the respond is resonding to.
func (r *LocalHomeRsp) GetRespondTo() uint64 {
	return r.RespondTo
}

type LocalHomeRspBuilder struct {
	sendTime  akita.VTimeInSec
	src, dst  akita.Port
	originDst akita.Port
	rspTo     uint64
	data      []byte
	info      interface{}
	localHome akita.Port
	PAddr     uint64
}

func (b LocalHomeRspBuilder) WithSendTime(
	t akita.VTimeInSec,
) LocalHomeRspBuilder {
	b.sendTime = t
	return b
}

// WithSrc sets the source of the request to build.
func (b LocalHomeRspBuilder) WithSrc(src akita.Port) LocalHomeRspBuilder {
	b.src = src
	return b
}

// WithDst sets the destination of the request to build.
func (b LocalHomeRspBuilder) WithDst(dst akita.Port) LocalHomeRspBuilder {
	b.dst = dst
	return b
}

func (b LocalHomeRspBuilder) WithOriginDst(originDst akita.Port) LocalHomeRspBuilder {
	b.originDst = originDst
	return b
}

// WithRspTo sets ID of the request that the respond to build is replying to.
func (b LocalHomeRspBuilder) WithRspTo(id uint64) LocalHomeRspBuilder {
	b.rspTo = id
	return b
}

// WithData sets the data of the request to build.
func (b LocalHomeRspBuilder) WithData(data []byte) LocalHomeRspBuilder {
	b.data = data
	return b
}

// WithInfo sets the info of the request to build.
func (b LocalHomeRspBuilder) WithInfo(info interface{}) LocalHomeRspBuilder {
	b.info = info
	return b
}

// WithInfo sets the info of the request to build.
func (b LocalHomeRspBuilder) WithLocalHome(p akita.Port) LocalHomeRspBuilder {
	b.localHome = p
	return b
}

func (b LocalHomeRspBuilder) Build() *LocalHomeRsp {
	r := &LocalHomeRsp{}
	r.ID = akita.GetIDGenerator().Generate()
	r.Src = b.src
	r.Dst = b.dst
	r.SendTime = b.sendTime
	r.TrafficBytes = len(b.data) + accessRspByteOverhead + metadataByteOverhead
	r.RespondTo = b.rspTo
	r.Data = b.data
	r.Info = b.info

	r.OriginDst = b.originDst
	return r
}

type LookupReq struct {
	akita.MsgMeta

	Address            uint64
	VAddr              uint64
	AccessByteSize     uint64
	PID                ca.PID
	CanWaitForCoalesce bool
	Info               interface{}

	IsPtw bool

	// PeekOnly marks a forced off-path local-home probe (dynamic-clustering):
	// it never coalesces into a pending MSHR (hits only on present data, else
	// misses), so an off-path probe cannot create a circular MSHR wait.
	// Distinct from the shared CanWaitForCoalesce field.
	PeekOnly bool

	LookupDst  []akita.Port
	LookupLeft int

	// only for NearFetch
	CornerNodeId int

	// HopTraceID carries the origin RDMA hop-trace task ID across the corner
	// MSHR-coalescing split (see protocol.ReadReq.HopTraceID). Propagated along
	// the lookup chain so the corner-node fetch can reuse the requestor's task.
	HopTraceID uint64

	// Hop counts how many intermediate-chiplet L2 lookups this NearFetch
	// data-forwarding request has reached. The origin RDMA engine stamps
	// Hop=1 on the first forwarded lookup; each subsequent forward (after a
	// miss) increments it. Used so the hit tracer can record at which hop a
	// data-forwarding hit occurred. Unused on the dynamic-dist lookup path.
	Hop int

	// HopBase is the cumulative hop distance from the original requestor to the
	// start of the current leg: 0 for the origin (requestor->corner) leg, and
	// the corner's arrival hop for the corner->home continuation leg. The true
	// requestor-relative distance of a lookup is Hop+HopBase. Without it, a
	// corner-relayed hit would be mis-recorded at its short per-leg Hop (an
	// L-shaped 0->5->35 hit at chiplet 11 would read distance 1, not 6).
	HopBase int

	// Visited records the chiplet ids the lookup chain has actually traversed:
	// the requestor first, then each probed local home appends its own id
	// before forwarding. LookupPathTracer reports this real visit sequence
	// (distinct from the planned LookupDst), so a hit chain stops at the
	// hitting local home rather than running to the global home.
	Visited []int

	// Scheme is the cluster-partitioning hotness the requestor chose for this
	// lookup chain. Carried so the hitting node classifies own- vs
	// other-cluster with the SAME partition the requestor used — the hitting
	// LH's own PCTable hotness is unreliable (it may never have run this PC, so
	// GetClusterId would fall back to per-chiplet and mark everything "other").
	Scheme Hotness
}

// Meta returns the message meta.
func (r *LookupReq) Meta() *akita.MsgMeta {
	return &r.MsgMeta
}

// GetByteSize returns the number of byte that the request is accessing.
func (r *LookupReq) GetByteSize() uint64 {
	return r.AccessByteSize
}

// GetAddress returns the address that the request is accessing
func (r *LookupReq) GetAddress() uint64 {
	return r.Address
}

// GetPID returns the process ID that the request is working on.
func (r *LookupReq) GetPID() ca.PID {
	return r.PID
}

func (r *LookupReq) GetAccessInfo() interface{} {
	return r.Info
}

type LookupReqBuilder struct {
	sendTime           akita.VTimeInSec
	src, dst           akita.Port
	pid                ca.PID
	address, byteSize  uint64
	vAddr              uint64
	canWaitForCoalesce bool
	peekOnly           bool
	info               interface{}

	lookupDst []akita.Port

	// only for NearFetch
	cornerNodeId int
	hop          int
	hopBase      int

	visited []int
	scheme  Hotness
}

// WithSendTime sets the send time of the request to build.
func (b LookupReqBuilder) WithSendTime(t akita.VTimeInSec) LookupReqBuilder {
	b.sendTime = t
	return b
}

// WithSrc sets the source of the request to build.
func (b LookupReqBuilder) WithSrc(src akita.Port) LookupReqBuilder {
	b.src = src
	return b
}

// WithDst sets the destination of the request to build.
func (b LookupReqBuilder) WithDst(dst akita.Port) LookupReqBuilder {
	b.dst = dst
	return b
}

// WithPID sets the PID of the request to build.
func (b LookupReqBuilder) WithPID(pid ca.PID) LookupReqBuilder {
	b.pid = pid
	return b
}

// WithInfo sets the Info of the request to build.
func (b LookupReqBuilder) WithInfo(info interface{}) LookupReqBuilder {
	b.info = info
	return b
}

// WithAddress sets the address of the request to build.
func (b LookupReqBuilder) WithAddress(address uint64) LookupReqBuilder {
	b.address = address
	return b
}

// WithVAddr sets virtual address for translation request
func (b LookupReqBuilder) WithVAddr(vAddr uint64) LookupReqBuilder {
	b.vAddr = vAddr
	return b
}

// WithByteSize sets the byte size of the request to build.
func (b LookupReqBuilder) WithByteSize(byteSize uint64) LookupReqBuilder {
	b.byteSize = byteSize
	return b
}

// for the implementation of nearfetch
// WithHop sets the NearFetch data-forwarding hop counter of the request.
func (b LookupReqBuilder) WithHop(hop int) LookupReqBuilder {
	b.hop = hop
	return b
}

// WithHopBase sets the cumulative requestor->leg-start hop offset so a
// corner-relayed lookup records a true requestor-relative distance (Hop+HopBase).
func (b LookupReqBuilder) WithHopBase(hopBase int) LookupReqBuilder {
	b.hopBase = hopBase
	return b
}

// WithVisited sets the already-traversed chiplet-id sequence of the lookup
// chain (requestor first; each hop appends its own id before forwarding).
func (b LookupReqBuilder) WithVisited(visited []int) LookupReqBuilder {
	b.visited = visited
	return b
}

// WithScheme sets the cluster-partitioning hotness used for this lookup chain.
func (b LookupReqBuilder) WithScheme(scheme Hotness) LookupReqBuilder {
	b.scheme = scheme
	return b
}

func (b LookupReqBuilder) WithCornerNodeId(id int) LookupReqBuilder {
	b.cornerNodeId = id
	return b
}

func (b LookupReqBuilder) WithLookupDst(lookupDst []akita.Port) LookupReqBuilder {
	b.lookupDst = lookupDst
	return b
}

// CanWaitForCoalesce allow the request to build to wait for coalesce.
func (b LookupReqBuilder) CanWaitForCoalesce() LookupReqBuilder {
	b.canWaitForCoalesce = true
	return b
}

// WithPeekOnly marks the lookup as a forced off-path probe that must never
// coalesce into a pending MSHR (dynamic-clustering deadlock avoidance).
func (b LookupReqBuilder) WithPeekOnly(v bool) LookupReqBuilder {
	b.peekOnly = v
	return b
}

func (b LookupReqBuilder) Build() *LookupReq {
	r := &LookupReq{}
	r.ID = akita.GetIDGenerator().Generate()
	r.Src = b.src
	r.Dst = b.dst
	r.SendTime = b.sendTime
	r.TrafficBytes = accessReqByteOverhead
	r.Address = b.address
	r.VAddr = b.vAddr
	r.PID = 1 // force single-process PID
	r.Info = b.info
	r.AccessByteSize = b.byteSize
	r.CanWaitForCoalesce = b.canWaitForCoalesce
	r.PeekOnly = b.peekOnly

	r.LookupDst = b.lookupDst
	r.LookupLeft = len(b.lookupDst)

	// only for NearFetch
	r.CornerNodeId = b.cornerNodeId
	r.Hop = b.hop
	r.HopBase = b.hopBase
	r.Visited = b.visited
	r.Scheme = b.scheme
	return r
}

type LookupRsp struct {
	akita.MsgMeta

	RespondTo uint64 // The ID of the request it replies
	Data      []byte
	Info      interface{}
}

// Meta returns the meta data attached to each message.
func (r *LookupRsp) Meta() *akita.MsgMeta {
	return &r.MsgMeta
}

// GetRespondTo returns the ID if the request that the respond is resonding to.
func (r *LookupRsp) GetRespondTo() uint64 {
	return r.RespondTo
}

type LookupRspBuilder struct {
	sendTime akita.VTimeInSec
	src, dst akita.Port
	rspTo    uint64
	data     []byte
	info     interface{}
}

// WithSendTime sets the send time of the request to build.
func (b LookupRspBuilder) WithSendTime(
	t akita.VTimeInSec,
) LookupRspBuilder {
	b.sendTime = t
	return b
}

// WithSrc sets the source of the request to build.
func (b LookupRspBuilder) WithSrc(src akita.Port) LookupRspBuilder {
	b.src = src
	return b
}

// WithDst sets the destination of the request to build.
func (b LookupRspBuilder) WithDst(dst akita.Port) LookupRspBuilder {
	b.dst = dst
	return b
}

// WithRspTo sets ID of the request that the respond to build is replying to.
func (b LookupRspBuilder) WithRspTo(id uint64) LookupRspBuilder {
	b.rspTo = id
	return b
}

// WithData sets the data of the request to build.
func (b LookupRspBuilder) WithData(data []byte) LookupRspBuilder {
	b.data = data
	return b
}

// WithInfo sets the info of the request to build.
func (b LookupRspBuilder) WithInfo(info interface{}) LookupRspBuilder {
	b.info = info
	return b
}

func (b LookupRspBuilder) Build() *LookupRsp {
	r := &LookupRsp{}
	r.ID = akita.GetIDGenerator().Generate()
	r.Src = b.src
	r.Dst = b.dst
	r.SendTime = b.sendTime
	r.TrafficBytes = len(b.data) + accessRspByteOverhead
	r.RespondTo = b.rspTo
	r.Data = b.data
	r.Info = b.info
	return r
}
