package emu

import (
	"gitlab.com/akita/akita"
	"gitlab.com/akita/mgpusim/coreprotocol"
)

// WGCompleteEvent is an event that marks the completion of a work-group
type WGCompleteEvent struct {
	*akita.EventBase

	Req *coreprotocol.MapWGReq
}

// NewWGCompleteEvent returns a newly constructed WGCompleteEvent
func NewWGCompleteEvent(t akita.VTimeInSec, handler akita.Handler,
	req *coreprotocol.MapWGReq,
) *WGCompleteEvent {
	e := new(WGCompleteEvent)
	e.EventBase = akita.NewEventBase(t, handler)
	e.Req = req
	return e
}
