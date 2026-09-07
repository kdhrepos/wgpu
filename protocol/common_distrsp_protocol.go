package protocol

import "gitlab.com/akita/akita"

type CommonDistRsp struct {
	akita.MsgMeta

	RespondTo uint64 // The ID of the request it replies
	Data      []byte
	PAddr     uint64
	Info      interface{} // should be PCState

	LocalHomeList []akita.Port
}

// Meta returns the meta data attached to each message.
func (r *CommonDistRsp) Meta() *akita.MsgMeta {
	return &r.MsgMeta
}

// GetRespondTo returns the ID if the request that the respond is resonding to.
func (r *CommonDistRsp) GetRespondTo() uint64 {
	return r.RespondTo
}

type CommonDistRspBuilder struct {
	sendTime akita.VTimeInSec
	src, dst akita.Port
	rspTo    uint64
	data     []byte
	info     interface{}

	localHomeList []akita.Port
}

func (b CommonDistRspBuilder) WithSendTime(
	t akita.VTimeInSec,
) CommonDistRspBuilder {
	b.sendTime = t
	return b
}

// WithSrc sets the source of the request to build.
func (b CommonDistRspBuilder) WithSrc(src akita.Port) CommonDistRspBuilder {
	b.src = src
	return b
}

// WithDst sets the destination of the request to build.
func (b CommonDistRspBuilder) WithDst(dst akita.Port) CommonDistRspBuilder {
	b.dst = dst
	return b
}

// WithRspTo sets ID of the request that the respond to build is replying to.
func (b CommonDistRspBuilder) WithRspTo(id uint64) CommonDistRspBuilder {
	b.rspTo = id
	return b
}

// WithData sets the data of the request to build.
func (b CommonDistRspBuilder) WithData(data []byte) CommonDistRspBuilder {
	b.data = data
	return b
}

// WithInfo sets the info of the request to build.
func (b CommonDistRspBuilder) WithInfo(info interface{}) CommonDistRspBuilder {
	b.info = info
	return b
}

func (b CommonDistRspBuilder) WithLocalHomeList(list []akita.Port) CommonDistRspBuilder {
	b.localHomeList = list
	return b
}

func (b CommonDistRspBuilder) Build() *CommonDistRsp {
	r := &CommonDistRsp{}
	r.ID = akita.GetIDGenerator().Generate()
	r.Src = b.src
	r.Dst = b.dst
	r.SendTime = b.sendTime
	r.TrafficBytes = len(b.data) + accessRspByteOverhead + metadataByteOverhead
	r.RespondTo = b.rspTo
	r.Data = b.data
	r.Info = b.info
	r.LocalHomeList = b.localHomeList
	return r
}
