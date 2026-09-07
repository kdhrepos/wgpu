package writeback_dynamic_dist

import (
	"log"
)

// LookupBuffer holds cachePipelineItems ordered by transaction message ID
// (ascending). The directory stage picks the first item whose target block
// is ready (not locked, no active readers), preserving same-line ordering
// while allowing later-arrived independent transactions to bypass a stalled
// head. Implements util.Buffer.
type LookupBuffer struct {
	capacity      int
	localReserve  int
	lookupReserve int
	elements      []cachePipelineItem
}

// NewLookupBuffer creates an empty LookupBuffer with the given capacity,
// local reserve, and lookup reserve. localReserve slots are kept free for
// local (toRemote=false) admission; lookupReserve slots on top of that are
// kept free for remote lookups so a real remote-fetch flood cannot starve
// lookup admission. See CanPushTransClass for the 3-class policy.
func NewLookupBuffer(capacity, localReserve, lookupReserve int) *LookupBuffer {
	return &LookupBuffer{
		capacity:      capacity,
		localReserve:  localReserve,
		lookupReserve: lookupReserve,
	}
}

func (b *LookupBuffer) CanPush() bool {
	return len(b.elements) < b.capacity
}

// CanPushTransClass admits with 3-class reserve awareness. Classes:
//   - local (!toRemote)            : whole buffer (len < capacity).
//   - remote lookup                : len < capacity-localReserve.
//   - remote non-lookup (real
//     remote fetch)                : len < capacity-localReserve-lookupReserve.
//
// A real remote-fetch flood is thus bounded, leaving lookupReserve slots for
// lookups and localReserve for local serves. (toRemote=false & lookup does
// not occur.)
func (b *LookupBuffer) CanPushTransClass(toRemote, isLookup bool) bool {
	if !toRemote {
		return len(b.elements) < b.capacity
	}
	if isLookup {
		return len(b.elements) < b.capacity-b.localReserve
	}
	return len(b.elements) < b.capacity-b.localReserve-b.lookupReserve
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

// transPriorityKey returns (priority, id) for sort ordering. Three tiers
// (lower = served first), mirroring writebackrcr minus the coalesce tier:
//
//	0  local-home serve  — !toRemote && !isLookup: reads local DRAM, generates a
//	                       response (an incoming remote miss lands here at its home)
//	1  lookup probe      — isLookup: a distribution-chain lookup. Immediately
//	                       serviceable (handleLookupMiss just forwards a
//	                       LookupMissRsp — no MSHR, no numToRemote budget), so it
//	                       sorts ABOVE a real fetch to keep the lookup chain
//	                       flowing and actually produce a home read.
//	2  real fetch        — toRemote && !isLookup: forward-to-remote miss, can
//	                       stall on the numToRemote reserve budget.
//
// Within a tier, ties break by id ascending to preserve same-line ordering.
func transPriorityKey(t *transaction) (int, uint64) {
	var p int
	switch {
	case !t.toRemote && !t.isLookup:
		p = 0 // local-home serve
	case t.isLookup:
		p = 1 // lookup probe (immediately forwarded)
	default:
		p = 2 // real remote fetch
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

// transOrderKey returns a monotonic ordering key for the transaction. With
// the default sequential ID generator, message IDs are decimal strings, so
// numeric parsing yields the true creation order. For non-numeric IDs
// (parallel xid generator) the fallback keeps insertion stable but loses
// the cross-source ordering guarantee.
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
