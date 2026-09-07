package protocol

import (
	"gitlab.com/akita/akita"
	"gitlab.com/akita/util/ca"
)

type MshrCoalesceReq struct {
	akita.MsgMeta

	Address            uint64
	VAddr              uint64
	AccessByteSize     uint64
	PID                ca.PID
	CanWaitForCoalesce bool
	Info               interface{}
}

// Meta returns the message meta.
func (r *MshrCoalesceReq) Meta() *akita.MsgMeta {
	return &r.MsgMeta
}

// GetByteSize returns the number of byte that the request is accessing.
func (r *MshrCoalesceReq) GetByteSize() uint64 {
	return r.AccessByteSize
}

// GetAddress returns the address that the request is accessing
func (r *MshrCoalesceReq) GetAddress() uint64 {
	return r.Address
}

// GetPID returns the process ID that the request is working on.
func (r *MshrCoalesceReq) GetPID() ca.PID {
	return r.PID
}

// GetAccessInfo returns the process ID that the request is working on.
// func (r *MshrCoalesceReq) GetAccessInfo() interface{} {
// 	return r.Info
// }

// MshrCoalesceReqBuilder can build read requests.
type MshrCoalesceReqBuilder struct {
	sendTime           akita.VTimeInSec
	src, dst           akita.Port
	pid                ca.PID
	address, byteSize  uint64
	vAddr              uint64
	canWaitForCoalesce bool
	info               interface{}
}

// WithSendTime sets the send time of the request to build.
func (b MshrCoalesceReqBuilder) WithSendTime(t akita.VTimeInSec) MshrCoalesceReqBuilder {
	b.sendTime = t
	return b
}

// WithSrc sets the source of the request to build.
func (b MshrCoalesceReqBuilder) WithSrc(src akita.Port) MshrCoalesceReqBuilder {
	b.src = src
	return b
}

// WithDst sets the destination of the request to build.
func (b MshrCoalesceReqBuilder) WithDst(dst akita.Port) MshrCoalesceReqBuilder {
	b.dst = dst
	return b
}

// WithPID sets the PID of the request to build.
func (b MshrCoalesceReqBuilder) WithPID(pid ca.PID) MshrCoalesceReqBuilder {
	b.pid = pid
	return b
}

// WithInfo sets the Info of the request to build.
func (b MshrCoalesceReqBuilder) WithInfo(info interface{}) MshrCoalesceReqBuilder {
	b.info = info
	return b
}

// WithAddress sets the address of the request to build.
func (b MshrCoalesceReqBuilder) WithAddress(address uint64) MshrCoalesceReqBuilder {
	b.address = address
	return b
}

// WithVAddr sets virtual address for translation request
func (b MshrCoalesceReqBuilder) WithVAddr(vAddr uint64) MshrCoalesceReqBuilder {
	b.vAddr = vAddr
	return b
}

// WithByteSize sets the byte size of the request to build.
func (b MshrCoalesceReqBuilder) WithByteSize(byteSize uint64) MshrCoalesceReqBuilder {
	b.byteSize = byteSize
	return b
}

// CanWaitForCoalesce allow the request to build to wait for coalesce.
func (b MshrCoalesceReqBuilder) CanWaitForCoalesce() MshrCoalesceReqBuilder {
	b.canWaitForCoalesce = true
	return b
}

// Build creates a new MshrCoalesceReq
func (b MshrCoalesceReqBuilder) Build() *MshrCoalesceReq {
	r := &MshrCoalesceReq{}
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
