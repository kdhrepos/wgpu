package writeback_dynamic_dist

import "gitlab.com/akita/protocol"

// shouldBypassByHotness decides whether a fetch returning with
// `incomingHotness` should be delivered to the requester only,
// skipping L2 install, because installing it would displace a
// resident line whose hotness ranks strictly higher — OR, on a
// hotness tie, displace a PTE with a non-PTE data line (PTEs are
// preferred on ties to avoid re-triggering page walks).
//
// Returns true when:
//
//   - incoming hotness rank < victim hotness rank, OR
//   - ranks are equal AND victim is a PTE AND incoming is not a PTE.
//
// Returns false when victim is nil/empty, when incoming dominates,
// when ranks tie among same kind (both PTE or both data), or when
// the PTE relationship is reversed (incoming PTE, victim data —
// install proceeds, PTE replaces the data).
//
// hotnessRank orders Streaming < Bypass < Cold < Warm_NonShared <
// Warm < Hot_NonShared < Hot < Global.
func shouldBypassByHotness(
	victim *Block,
	incomingHotness protocol.Hotness,
	incomingIsPte bool,
) bool {
	if victim == nil || !victim.IsValid {
		return false
	}
	iRank := hotnessRank(incomingHotness)
	vRank := hotnessRank(victim.Hotness)
	if iRank < vRank {
		return true
	}
	if iRank == vRank && victim.IsPte && !incomingIsPte {
		return true
	}
	return false
}

// isNonCacheableRsp reports whether a remote DistInfo hotness signals
// that the fetched line should NOT be cached: the requester is still
// served via MSHR, but the slot that the directory stage pre-marked
// valid must be invalidated by the write-back stage. Streaming /
// Bypass are the two response flavors that carry this meaning.
func isNonCacheableRsp(h protocol.Hotness) bool {
	return h == protocol.Streaming || h == protocol.Bypass
}

// storableRdmaHotness translates a Hotness pulled from an incoming RDMA
// response's DistInfo into the per-block hotness that should actually be
// written into the resident slot on install. Mapping:
//
//   - Warm / Hot / Global: stored as-is. These reflect the remote
//     directory's view of the line and we want the new resident to
//     start with the same level (matches LocalHomeRsp behavior).
//   - Warm_NonShared / Hot_NonShared: stored as their plain
//     counterparts. The _NonShared flavor only signals "this fetch
//     wasn't a distribution"; the stored hotness should just be the
//     base level so subsequent victim-finder decisions see the line as
//     warm/hot.
//   - Cold / Streaming / Bypass: stored as Cold. Streaming and Bypass
//     are response-flavor markers (the remote chose not to distribute),
//     not a meaningful per-block hotness, so the resident starts cold.
func storableRdmaHotness(h protocol.Hotness) protocol.Hotness {
	switch h {
	case protocol.Warm, protocol.Hot, protocol.Global:
		return h
	case protocol.Warm_NonShared:
		return protocol.Warm
	case protocol.Hot_NonShared:
		return protocol.Hot
	default:
		return protocol.Cold
	}
}

// hotnessRank assigns a strict order over Hotness for caching priority.
// *_NonShared variants are ranked just below their plain counterparts
// so that, given equal "level", a plain entry is preferred to a
// NonShared one (e.g. existing Hot beats incoming Hot_NonShared).
//
// Ordering (low → high):
//
//	Streaming < Bypass < Cold
//	< Warm_NonShared < Warm
//	< Hot_NonShared < Hot
//	< Global
func hotnessRank(h protocol.Hotness) int {
	switch h {
	case protocol.Streaming:
		return -2
	case protocol.Bypass:
		return -1
	case protocol.Cold:
		return 0
	case protocol.Warm_NonShared:
		return 1
	case protocol.Warm:
		return 2
	case protocol.Hot_NonShared:
		return 3
	case protocol.Hot:
		return 4
	case protocol.Global:
		return 5
	}
	return 0
}
