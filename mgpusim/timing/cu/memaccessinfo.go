package cu

import (
	"gitlab.com/akita/akita"
	"gitlab.com/akita/mgpusim/insts"
	"gitlab.com/akita/mgpusim/timing/wavefront"
	"gitlab.com/akita/protocol"
)

type vectorMemAccessLaneInfo struct {
	laneID                int
	reg                   *insts.Reg
	regCount              int
	addrOffsetInCacheLine uint64
}

// VectorMemAccessInfo defines access info
type VectorMemAccessInfo struct {
	ID        uint64
	Read      *protocol.ReadReq
	Write     *protocol.WriteReq
	Wavefront *wavefront.Wavefront
	Inst      *wavefront.Inst
	PC        uint64
	IssueTime akita.VTimeInSec
	laneInfo  []vectorMemAccessLaneInfo
}

// TaskID returns the ID of the VectorMemAccess transaction
func (i VectorMemAccessInfo) TaskID() uint64 {
	return i.ID
}

// InstFetchReqInfo defines request info
type InstFetchReqInfo struct {
	Req       *protocol.ReadReq
	Wavefront *wavefront.Wavefront
	Address   uint64
}

// ScalarMemAccessInfo defines request info
type ScalarMemAccessInfo struct {
	Req       *protocol.ReadReq
	Wavefront *wavefront.Wavefront
	DstSGPR   *insts.Reg
	Inst      *wavefront.Inst
	IssueTime akita.VTimeInSec
}
