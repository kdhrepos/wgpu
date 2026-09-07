package protocol

import (
	"gitlab.com/akita/akita"
	"gitlab.com/akita/util/ca"
)

type RCRResult int

const (
	RCRHit = iota
	RCRMiss
)

type RCReq struct {
	akita.MsgMeta

	Address            uint64
	VAddr              uint64
	AccessByteSize     uint64
	PID                ca.PID
	CanWaitForCoalesce bool
	Info               interface{}
}

// Meta returns the message meta.
func (r *RCReq) Meta() *akita.MsgMeta {
	return &r.MsgMeta
}

// GetByteSize returns the number of byte that the request is accessing.
func (r *RCReq) GetByteSize() uint64 {
	return r.AccessByteSize
}

// GetAddress returns the address that the request is accessing
func (r *RCReq) GetAddress() uint64 {
	return r.Address
}

// GetPID returns the process ID that the request is working on.
func (r *RCReq) GetPID() ca.PID {
	return r.PID
}

// GetAccessInfo returns the process ID that the request is working on.
func (r *RCReq) GetAccessInfo() interface{} {
	return r.Info
}

// A RCReqInfo is a request sent to a memory controller to write data
type RCReqInfo struct {
	ReturnAccessInfo bool
	RCRResult
}

// RCReqBuilder can build read requests.
type RCReqBuilder struct {
	sendTime           akita.VTimeInSec
	src, dst           akita.Port
	pid                ca.PID
	address, byteSize  uint64
	vAddr              uint64
	canWaitForCoalesce bool
	info               interface{}
}

// WithSendTime sets the send time of the request to build.
func (b RCReqBuilder) WithSendTime(t akita.VTimeInSec) RCReqBuilder {
	b.sendTime = t
	return b
}

// WithSrc sets the source of the request to build.
func (b RCReqBuilder) WithSrc(src akita.Port) RCReqBuilder {
	b.src = src
	return b
}

// WithDst sets the destination of the request to build.
func (b RCReqBuilder) WithDst(dst akita.Port) RCReqBuilder {
	b.dst = dst
	return b
}

// WithPID sets the PID of the request to build.
func (b RCReqBuilder) WithPID(pid ca.PID) RCReqBuilder {
	b.pid = pid
	return b
}

// WithInfo sets the Info of the request to build.
func (b RCReqBuilder) WithInfo(info interface{}) RCReqBuilder {
	b.info = info
	return b
}

// WithAddress sets the address of the request to build.
func (b RCReqBuilder) WithAddress(address uint64) RCReqBuilder {
	b.address = address
	return b
}

// WithVAddr sets virtual address for translation request
func (b RCReqBuilder) WithVAddr(vAddr uint64) RCReqBuilder {
	b.vAddr = vAddr
	return b
}

// WithByteSize sets the byte size of the request to build.
func (b RCReqBuilder) WithByteSize(byteSize uint64) RCReqBuilder {
	b.byteSize = byteSize
	return b
}

// CanWaitForCoalesce allow the request to build to wait for coalesce.
func (b RCReqBuilder) CanWaitForCoalesce() RCReqBuilder {
	b.canWaitForCoalesce = true
	return b
}

// Build creates a new RCReq
func (b RCReqBuilder) Build() *RCReq {
	r := &RCReq{}
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
	return r
}

type RCRsp struct {
	akita.MsgMeta

	RespondTo uint64 // The ID of the request it replies
	Data      []byte
	Info      interface{}
}

// Meta returns the meta data attached to each message.
func (r *RCRsp) Meta() *akita.MsgMeta {
	return &r.MsgMeta
}

type RCRspInfo struct {
	RCRResult
	Src string
}

// GetRespondTo returns the ID if the request that the respond is resonding to.
func (r *RCRsp) GetRespondTo() uint64 {
	return r.RespondTo
}

// RCRspBuilder can build data ready responds.
type RCRspBuilder struct {
	sendTime akita.VTimeInSec
	src, dst akita.Port
	rspTo    uint64
	data     []byte
	info     interface{}
}

// WithSendTime sets the send time of the request to build.
func (b RCRspBuilder) WithSendTime(
	t akita.VTimeInSec,
) RCRspBuilder {
	b.sendTime = t
	return b
}

// WithSrc sets the source of the request to build.
func (b RCRspBuilder) WithSrc(src akita.Port) RCRspBuilder {
	b.src = src
	return b
}

// WithDst sets the destination of the request to build.
func (b RCRspBuilder) WithDst(dst akita.Port) RCRspBuilder {
	b.dst = dst
	return b
}

// WithRspTo sets ID of the request that the respond to build is replying to.
func (b RCRspBuilder) WithRspTo(id uint64) RCRspBuilder {
	b.rspTo = id
	return b
}

// WithData sets the data of the request to build.
func (b RCRspBuilder) WithData(data []byte) RCRspBuilder {
	b.data = data
	return b
}

// WithInfo sets the info of the request to build.
func (b RCRspBuilder) WithInfo(info interface{}) RCRspBuilder {
	b.info = info
	return b
}

// Build creates a new RCRsp
func (b RCRspBuilder) Build() *RCRsp {
	r := &RCRsp{}
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
