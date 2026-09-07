package rdma

import (
	"gitlab.com/akita/protocol"
)

// SharedPCTable models a dynamic-clustering PC classifier with a
// zero-overhead cross-chiplet hotness sync.
//
// Each chiplet still classifies online, exactly like PCTable: as the Home
// of an address range it observes the external requests to its data
// (ReqFromOutside), accumulates the HLL sketches, and periodically
// reclassifies each PC (Decay -> CachedHotness). The ONLY difference is
// how a requestor learns a Home's classification.
//
//   - Plain PCTable propagates a Home's hotness back to requestors by
//     piggybacking it on responses (MergeRemoteHotness). That is real,
//     if cheap, communication.
//
//   - SharedPCTable removes that propagation entirely. A requestor reads
//     the Home chiplet's own table directly (a same-process pointer
//     dereference performed in the DynamicClusteringRdmaEngine), so the
//     instant a Home flips PC 0xA from Warm to Hot every other chiplet
//     sees "Home + 0xA = Hot" with no flit, no latency, no extra message.
//     This is the "complete zero-overhead sync" idealization: it isolates
//     the benefit of dynamic classification from the cost of broadcasting
//     it.
//
// The type embeds *PCTable so all counting / classification / reporting
// logic is reused verbatim; only MergeRemoteHotness is overridden to a
// no-op because the shared read replaces the piggyback channel.
type SharedPCTable struct {
	*PCTable
}

// NewSharedPCTable builds a SharedPCTable over a fresh online PCTable.
func NewSharedPCTable(engine RDMAEngine, xDim, yDim int) *SharedPCTable {
	return &SharedPCTable{
		PCTable: NewPCTable(engine, xDim, yDim),
	}
}

// MergeRemoteHotness is a no-op: the requestor obtains a Home's hotness by
// reading the Home's table directly (zero-overhead sync), so response
// piggyback carries no information here.
func (t *SharedPCTable) MergeRemoteHotness(pc uint64, remote protocol.Hotness) {}
