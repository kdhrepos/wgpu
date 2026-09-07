package writebackrcr

import (
	"log"
)

// LookupBuffer holds cachePipelineItems ordered by transaction message ID
// (ascending). The directory stage picks the first item whose target block
// is ready (not locked, no active readers), preserving same-line ordering
// while allowing later-arrived independent transactions to bypass a stalled
// head. Implements util.Buffer.
type LookupBuffer struct {
	capacity        int
	localReserve    int
	coalesceReserve int
	elements        []cachePipelineItem
}

// NewLookupBuffer creates an empty LookupBuffer with the given capacity and the
// two class reserves. localReserve keeps slots free for local-home work;
// coalesceReserve bounds corner-serve (mshr-coalesce) lookups by COUNT so they
// cannot wall off local-home and real-remote admission — same 3-class scheme as
// SmartBuffer (dirToBank / writeBufferBuffer).
func NewLookupBuffer(capacity, localReserve, coalesceReserve int) *LookupBuffer {
	return &LookupBuffer{
		capacity:        capacity,
		localReserve:    localReserve,
		coalesceReserve: coalesceReserve,
	}
}

func (b *LookupBuffer) CanPush() bool {
	return len(b.elements) < b.capacity
}

// coalesceCount returns the number of TRUE coalesce delegations buffered —
// trans.isCoalesce (toRemote && hasCornerNode, stamped by topParser). Type-counted
// like SmartBuffer.coalesceCount so a coalesce flood is bounded by its own count,
// not by total occupancy. NOTE: this is NOT isLookup && isCornerNode — that is the
// corner-SERVE predicate (a real fetch), which must be PROTECTED, not bounded.
func (b *LookupBuffer) coalesceCount() int {
	n := 0
	for _, e := range b.elements {
		if e.trans.isCoalesce {
			n++
		}
	}
	return n
}

// ClassCounts returns the buffer's composition by the TRUE class so the deadlock
// summary can show WHICH class fills it (lookup=N alone hides the cause):
//   home  - local-home serve (!toRemote, !isLookup): read local DRAM, generate a
//           response. The cycle-breaker; an incoming RDMA home-read lands here.
//   real  - real remote fetch (toRemote && !isCoalesce): straight-line to home —
//           own CU read OR an incoming corner-serve lookup. The RESPONSE GENERATOR
//           for remote misses; must be protected (cap-localReserve band).
//   probe - non-corner lookup (!toRemote && isLookup): forwarded chain probe.
//   coal  - coalesce delegation (toRemote && isCoalesce = hasCornerNode): the
//           L-shaped path that defers its fetch to a corner; count-bounded.
func (b *LookupBuffer) ClassCounts() (home, real, probe, coal int) {
	for _, e := range b.elements {
		t := e.trans
		switch {
		case !t.toRemote && !t.isLookup:
			home++
		case t.toRemote && !t.isCoalesce:
			real++
		case !t.toRemote && t.isLookup:
			probe++
		default:
			coal++
		}
	}
	return
}

// CanPushTransClass is the 3-class reserve admission test, mirroring
// SmartBuffer.CanPushTransClassR used by dirToBank / writeBufferBuffer. isCoalesce
// is the TRUE coalesce predicate (toRemote && hasCornerNode), NOT isLookup &&
// isCornerNode:
//   - local-home (toRemote=false): may use the whole buffer (probes included).
//   - real remote (toRemote=true, !coalesce): total must leave localReserve free.
//     This is the response-generating fetch (own straight-line read OR incoming
//     corner-serve lookup) — protected so it always has reachable slots.
//   - coalesce delegation (toRemote && hasCornerNode): its COUNT must stay below
//     capacity-localReserve-coalesceReserve, so local and real-remote always have
//     reachable slots regardless of how many delegations pile up waiting on a
//     corner. Bounding the L-shaped delegations (not the corner-serves) is the fix.
func (b *LookupBuffer) CanPushTransClass(toRemote, isCoalesce bool) bool {
	if !toRemote {
		return len(b.elements) < b.capacity
	}
	if isCoalesce {
		if b.coalesceCount() >= b.capacity-b.localReserve-b.coalesceReserve {
			return false
		}
		return len(b.elements) < b.capacity
	}
	return len(b.elements) < b.capacity-b.localReserve
}

func (b *LookupBuffer) Push(e interface{}) {
	if len(b.elements) >= b.capacity {
		log.Panic("lookup buffer overflow")
	}
	item, ok := e.(cachePipelineItem)
	if !ok {
		log.Panic("LookupBuffer accepts only cachePipelineItem")
	}
	b.insertSorted(item)
}

func (b *LookupBuffer) insertSorted(item cachePipelineItem) {
	pNew, idNew := transPriorityKey(item.trans)
	idx := len(b.elements)
	for i, e := range b.elements {
		pE, idE := transPriorityKey(e.trans)
		if pE > pNew || (pE == pNew && idE > idNew) {
			idx = i
			break
		}
	}
	b.elements = append(b.elements, cachePipelineItem{})
	copy(b.elements[idx+1:], b.elements[idx:])
	b.elements[idx] = item
}

// transPriorityKey returns (priority, id) for sort ordering. Four explicit tiers
// (lower = served first). A transit lookup probe sits ABOVE real fetches because
// it is immediately serviceable (handleLookupMiss just forwards — no MSHR, no
// remote budget) and advances the NearFetch chain toward the home, whereas a real
// fetch can stall on MSHR/remote budget. Serving probes first keeps the chain
// flowing so a home read is actually produced:
//
//	0  local-home serve  — !toRemote && !isLookup: reads local DRAM, generates a
//	                       response (an incoming remote miss lands here at its home)
//	1  transit lookup    — isLookup && !isCornerNode: forwarded chain probe, no fetch
//	2  real fetch        — toRemote && !isCoalesce: this chiplet is the corner for
//	                       the home (incoming corner-serve lookup OR own straight-
//	                       line read). Drains a coalesce elsewhere.
//	3  coalesce delegate — toRemote && isCoalesce (hasCornerNode): defers to a corner
//
// Within a tier, ties break by id ascending (earlier-generated first).
func transPriorityKey(t *transaction) (int, uint64) {
	var p int
	switch {
	case !t.toRemote && !t.isLookup:
		p = 0 // local-home serve
	case t.isLookup && !t.isCornerNode:
		p = 1 // transit lookup probe (immediately forwarded)
	case t.toRemote && !t.isCoalesce:
		p = 2 // real fetch (this chiplet is the corner for home)
	default:
		p = 3 // coalesce delegation
	}
	return p, transOrderKey(t)
}

func (b *LookupBuffer) Pop() interface{} {
	if len(b.elements) == 0 {
		return nil
	}
	e := b.elements[0]
	b.elements = b.elements[1:]
	return e
}

func (b *LookupBuffer) Peek() interface{} {
	if len(b.elements) == 0 {
		return nil
	}
	return b.elements[0]
}

func (b *LookupBuffer) Capacity() int { return b.capacity }
func (b *LookupBuffer) Size() int     { return len(b.elements) }
func (b *LookupBuffer) Clear()        { b.elements = nil }

// PeekReady returns the first item whose transaction satisfies ready, plus
// its index. ok=false if none is ready.
func (b *LookupBuffer) PeekReady(
	ready func(*transaction) bool,
) (item cachePipelineItem, idx int, ok bool) {
	for i, e := range b.elements {
		if ready(e.trans) {
			return e, i, true
		}
	}
	return cachePipelineItem{}, -1, false
}

// RemoveAt removes the element at idx. No-op if idx is out of range.
func (b *LookupBuffer) RemoveAt(idx int) {
	if idx < 0 || idx >= len(b.elements) {
		return
	}
	b.elements = append(b.elements[:idx], b.elements[idx+1:]...)
}

// transOrderKey returns a monotonic ordering key for the transaction. Message
// IDs are produced by the sequential ID generator, so the numeric ID is itself
// the true creation order.
func transOrderKey(t *transaction) uint64 {
	switch {
	case t.read != nil:
		return t.read.Meta().ID
	case t.write != nil:
		return t.write.Meta().ID
	case t.flush != nil:
		return t.flush.Meta().ID
	}
	return 0
}
