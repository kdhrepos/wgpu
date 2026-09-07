package tracing

import "gitlab.com/akita/protocol"

// VictimInfo is the detail payload used when emitting a memory-trace row
// that describes the line being evicted/overwritten (type 8 = victim_evict,
// type 9 = victim_overwrite). Sister row to the type 6/7 row carrying the
// evictor's request; share the same (time, chiplet) so analysis can join
// them 1:1.
type VictimInfo struct {
	VictimAddr uint64
	VictimPC   uint64
}

// PCFromReq pulls the issuing PC out of an AccessReq's Info field. Callers
// in cache install paths use this to populate Block.InstallPC so a later
// evict can report which PC owned the line. Returns 0 when the request
// is nil or its Info is not a uint64 (e.g. PTW walks).
func PCFromReq(req protocol.AccessReq) uint64 {
	if req == nil {
		return 0
	}
	info := req.GetAccessInfo()
	if pc, ok := info.(uint64); ok {
		return pc
	}
	return 0
}
