package protocol

import (
	"gitlab.com/akita/akita"
	"gitlab.com/akita/util/ca"
)

// LookupHitRsp indicates lookup hit.
type LookupHitRsp struct {
	akita.MsgMeta

	RespondTo      uint64
	PID            ca.PID
	AccessByteSize uint64
	Info           interface{}
	Data           []byte
}

func (r *LookupHitRsp) Meta() *akita.MsgMeta {
	return &r.MsgMeta
}

func (r *LookupHitRsp) GetRespondTo() uint64 {
	return r.RespondTo
}

// LookupHitRspBuilder builds LookupHitRsp.
type LookupHitRspBuilder struct {
	sendTime akita.VTimeInSec
	src      akita.Port
	dst      akita.Port
	rspTo    uint64
	pid      ca.PID
	data     []byte
	info     interface{}
}

func (b LookupHitRspBuilder) WithSendTime(t akita.VTimeInSec) LookupHitRspBuilder {
	b.sendTime = t
	return b
}

func (b LookupHitRspBuilder) WithSrc(src akita.Port) LookupHitRspBuilder {
	b.src = src
	return b
}

func (b LookupHitRspBuilder) WithDst(dst akita.Port) LookupHitRspBuilder {
	b.dst = dst
	return b
}

func (b LookupHitRspBuilder) WithRspTo(rspTo uint64) LookupHitRspBuilder {
	b.rspTo = rspTo
	return b
}

func (b LookupHitRspBuilder) WithPID(pid ca.PID) LookupHitRspBuilder {
	b.pid = pid
	return b
}

func (b LookupHitRspBuilder) WithInfo(info interface{}) LookupHitRspBuilder {
	b.info = info
	return b
}

func (b LookupHitRspBuilder) WithData(data []byte) LookupHitRspBuilder {
	b.data = data
	return b
}

func (b LookupHitRspBuilder) Build() *LookupHitRsp {
	r := &LookupHitRsp{
		MsgMeta: akita.MsgMeta{
			ID:           akita.GetIDGenerator().Generate(),
			SendTime:     b.sendTime,
			Src:          b.src,
			Dst:          b.dst,
			TrafficBytes: len(b.data) + accessRspByteOverhead,
		},
		RespondTo: b.rspTo,
		PID:       1, // force single-process PID
		Info:      b.info,
		Data:      b.data,
	}
	return r
}

// LookupMissRsp indicates lookup miss.
type LookupMissRsp struct {
	akita.MsgMeta

	RespondTo      uint64
	PID            ca.PID
	Address        uint64
	AccessByteSize uint64
	Info           interface{}
}

func (r *LookupMissRsp) GetRespondTo() uint64 {
	return r.RespondTo
}

// LookupMissRspBuilder builds LookupMissRsp.
type LookupMissRspBuilder struct {
	sendTime akita.VTimeInSec
	src      akita.Port
	dst      akita.Port
	rspTo    uint64
	pid      ca.PID
	data     []byte
	info     interface{}
}

// Meta returns the meta data attached to each message.
func (r *LookupMissRsp) Meta() *akita.MsgMeta {
	return &r.MsgMeta
}

func (b LookupMissRspBuilder) WithSendTime(t akita.VTimeInSec) LookupMissRspBuilder {
	b.sendTime = t
	return b
}

func (b LookupMissRspBuilder) WithSrc(src akita.Port) LookupMissRspBuilder {
	b.src = src
	return b
}

func (b LookupMissRspBuilder) WithDst(dst akita.Port) LookupMissRspBuilder {
	b.dst = dst
	return b
}

func (b LookupMissRspBuilder) WithRspTo(rspTo uint64) LookupMissRspBuilder {
	b.rspTo = rspTo
	return b
}

func (b LookupMissRspBuilder) WithPID(pid ca.PID) LookupMissRspBuilder {
	b.pid = pid
	return b
}

func (b LookupMissRspBuilder) WithInfo(info interface{}) LookupMissRspBuilder {
	b.info = info
	return b
}

func (b LookupMissRspBuilder) WithData(data []byte) LookupMissRspBuilder {
	b.data = data
	return b
}

func (b LookupMissRspBuilder) Build() *LookupMissRsp {
	r := &LookupMissRsp{
		MsgMeta: akita.MsgMeta{
			ID:           akita.GetIDGenerator().Generate(),
			SendTime:     b.sendTime,
			Src:          b.src,
			Dst:          b.dst,
			TrafficBytes: len(b.data) + accessRspByteOverhead,
		},
		RespondTo: b.rspTo,
		PID:       1, // force single-process PID
		Info:      b.info,
	}
	return r
}
