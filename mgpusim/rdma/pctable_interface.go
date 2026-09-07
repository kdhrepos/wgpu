package rdma

import (
	"gitlab.com/akita/protocol"
)

type PCTableInterface interface {
	ReqFromOutside(pc uint64, addr uint64, requestorId int, hopCount int)
	ReqFromInside(pc uint64) protocol.Hotness
	RspFromInside(pc uint64, addr uint64) protocol.Hotness

	// Distribute notifies the table that the engine has actually issued
	// a distribution for the given PC.  Implementations use this to
	// advance per-PC distribution counters / throttling state.
	Distribute(pc uint64)

	// MergeRemoteHotness is called on the requestor side when a response
	// arrives carrying the home-classified hotness for a PC.  The
	// implementation merges this hint into its local view monotonically
	// (raise-only), so that the requestor's subsequent ReqFromInside
	// queries return at least the hotness reported by any home that has
	// responded.  Implementations that already have global knowledge
	// (e.g. Oracle) may treat this as a no-op.
	MergeRemoteHotness(pc uint64, remote protocol.Hotness)

	Decay()
	Reset()

	GetPCTable() interface{}
}
