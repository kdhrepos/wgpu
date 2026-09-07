package writebackrcr

import (
	"container/heap"
	"log"
)

// Priority encoding for SmartBuffer ordering (lower value pops first). Same
// 4-tier scheme as the lookup buffer's transPriorityKey, applied here so the
// directory->bank stage drains in the same chain-advancing order:
//
//	0 local-home serve     — !toRemote && !isLookup (reads local DRAM, responds)
//	1 transit lookup       — isLookup && !isCornerNode (probe / lookup-hit forward)
//	2 real fetch / hit     — toRemote && !coalesce-delegation (corner serves home)
//	3 coalesce delegation  — defers the fetch to a corner, waits on a remote rsp
//
// The coalesce tier is keyed on the ACTION (isCoalesceDelegation), not on the
// address-derived trans.isCoalesce: a coalesce-class read that HIT locally lands
// here as bankReadHit (a real response, not a delegation) and must keep the
// real-fetch tier, not sink to the bottom.
const smartBufferDefaultPriority = 2

// isCoalesceDelegation reports whether the action hands the fetch off to the
// corner/home node and waits on a remote response (no local progress).
func isCoalesceDelegation(a action) bool {
	switch a {
	case bankEvictAndMshrCoalesce,
		writeBufferMshrCoalesce,
		writeBufferEvictAndMshrCoalesce:
		return true
	}
	return false
}

// smartBufferPriority encodes the 4-tier priority for a transaction.
// Non-transaction entries (e.g. control msgs) fall back to default.
func smartBufferPriority(t *transaction) int {
	switch {
	case !t.toRemote && !t.isLookup:
		return 0 // local-home serve
	case t.isLookup && !t.isCornerNode:
		return 1 // transit lookup (probe / lookup-hit forward)
	case !isCoalesceDelegation(t.action):
		return 2 // real fetch / hit (this chiplet is the corner for home)
	default:
		return 3 // coalesce delegation
	}
}

type smartBufferEntry struct {
	value    interface{}
	priority int
	seq      uint64
}

type smartBufferHeap []smartBufferEntry

func (h smartBufferHeap) Len() int {
	return len(h)
}

func (h smartBufferHeap) Less(i, j int) bool {
	if h[i].priority != h[j].priority {
		return h[i].priority < h[j].priority
	}

	// Secondary key: transaction msg ID ascending so lower-id (= earlier
	// generated) transactions pop first. Falls back to insertion order
	// (seq) for non-transaction entries or transactions with non-numeric
	// IDs (transOrderKey returns 0 — ties broken by seq).
	idI := smartBufferEntryOrderKey(h[i])
	idJ := smartBufferEntryOrderKey(h[j])
	if idI != idJ {
		return idI < idJ
	}
	return h[i].seq < h[j].seq
}

func smartBufferEntryOrderKey(e smartBufferEntry) uint64 {
	if t, ok := e.value.(*transaction); ok {
		return transOrderKey(t)
	}
	return 0
}

func (h smartBufferHeap) Swap(i, j int) {
	h[i], h[j] = h[j], h[i]
}

func (h *smartBufferHeap) Push(x interface{}) {
	*h = append(*h, x.(smartBufferEntry))
}

func (h *smartBufferHeap) Pop() interface{} {
	old := *h
	n := len(old)
	entry := old[n-1]
	*h = old[:n-1]
	return entry
}

// NewSmartBuffer creates a default buffer object.
func NewSmartBuffer(capacity int) *SmartBuffer {
	b := &SmartBuffer{
		capacity: capacity,
		elements: make(smartBufferHeap, 0, capacity),
	}
	heap.Init(&b.elements)
	return b
}

type SmartBuffer struct {
	capacity int
	elements smartBufferHeap
	nextSeq  uint64
}

func (b *SmartBuffer) CanPush() bool {
	return len(b.elements) < b.capacity
}

// writeBufferLocalReserve is the number of writeBufferBuffer slots kept free for
// local (toRemote=false) transactions. The local serve / response path
// (reachedGlobal-home reads and corner-serve fetches landing at their home read
// LOCAL DRAM, generate responses, and depend on nothing remote) is the
// cycle-breaker of the NearFetch all-to-all wait. Without a reserve, a
// remote-bound (toRemote=true) coalesce/lookup flood fills the buffer to
// capacity and starves the serve path even though SmartBuffer already
// prioritizes it — priority orders, it does not reserve.
const writeBufferLocalReserve = 16

// dirToBankLocalReserve is the number of dirToBank slots kept free for local
// (toRemote=false) transactions. dirToBank is the home-node serve lane: a
// corner-serve / local-home read lands here, reads LOCAL DRAM, and generates
// the DataReady that unblocks a remote requester's outstanding fetch — the
// single cycle-breaker of the NearFetch all-to-all wait. Round-3 reserved only
// writeBufferBuffer, so the choke simply moved one stage up: toRemote=true
// forward-fetches filled dirToBank to capacity and a serve read could not enter
// to produce its response. Reserving here completes the serve lane end-to-end
// (lookupBufForRemote -> dirToBank -> rdmaSender), so responses always drain.
const dirToBankLocalReserve = 16

// CanPushTrans is a reservation-aware admission test. A remote-bound
// transaction may use only capacity-writeBufferLocalReserve slots; a local
// transaction may use the whole buffer.
func (b *SmartBuffer) CanPushTrans(toRemote bool) bool {
	return b.CanPushTransReserve(toRemote, writeBufferLocalReserve)
}

// CanPushTransReserve is CanPushTrans with an explicit reserve, so different
// buffers (writeBufferBuffer vs dirToBank) can keep their own local headroom.
func (b *SmartBuffer) CanPushTransReserve(toRemote bool, reserve int) bool {
	if toRemote {
		return len(b.elements) < b.capacity-reserve
	}
	return len(b.elements) < b.capacity
}

// writeBufferCoalesceReserve keeps writeBufferBuffer slots free for non-coalesce
// remote fetches (real data fetch). coalesce and a real writeBufferFetch are
// both toRemote=true, so without this a coalesce flood fills the whole
// toRemote region (capacity-writeBufferLocalReserve) and walls off a
// writeBufferFetch waiting in dirToBank — even though dirToBank, MSHR and
// inflightFetch all have room. The real fetch must always be able to leave
// dirToBank because it is the one that actually retrieves data and unblocks
// the line.
const writeBufferCoalesceReserve = 8

// dirToBankCoalesceReserve keeps dirToBank slots free for non-coalesce remote
// fetches, mirroring writeBufferCoalesceReserve one stage up. coalesce and a
// real writeBufferFetch are both toRemote=true; without this a coalesce flood
// fills the whole toRemote region (capacity-dirToBankLocalReserve) and the
// directoryStage cannot push a real fetch into dirToBank.
const dirToBankCoalesceReserve = 8

// coalesceCount returns how many coalesce-delegation transactions are currently
// in the buffer. O(n) over a small (<=capacity) heap; called on admission only.
func (b *SmartBuffer) coalesceCount() int {
	n := 0
	for _, e := range b.elements {
		if t, ok := e.value.(*transaction); ok && isCoalesceDelegation(t.action) {
			n++
		}
	}
	return n
}

// CanPushTransClassR is the class-split admission test with explicit reserves.
// It counts BY TYPE, not by total size, so a coalesce flood is bounded by the
// number of coalesce entries regardless of how full the buffer is with other
// classes:
//   - local (toRemote=false): may use the whole buffer.
//   - remote non-coalesce (real fetch): total must leave localR free.
//   - coalesce delegation: the COUNT of coalesce entries must stay below
//     capacity-localR-coalesceR (so localR + coalesceR slots are always
//     reachable by local + real-fetch traffic), and the buffer must not be
//     full overall.
func (b *SmartBuffer) CanPushTransClassR(
	toRemote, isCoalesce bool, localR, coalesceR int,
) bool {
	if !toRemote {
		return len(b.elements) < b.capacity
	}
	if isCoalesce {
		if b.coalesceCount() >= b.capacity-localR-coalesceR {
			return false
		}
		return len(b.elements) < b.capacity
	}
	return len(b.elements) < b.capacity-localR
}

// CanPushTransClass is the writeBufferBuffer-tuned wrapper of CanPushTransClassR.
func (b *SmartBuffer) CanPushTransClass(toRemote, isCoalesce bool) bool {
	return b.CanPushTransClassR(
		toRemote, isCoalesce, writeBufferLocalReserve, writeBufferCoalesceReserve)
}

func (b *SmartBuffer) Push(e interface{}) {
	if len(b.elements) >= b.capacity {
		log.Panic("smart buffer overflow")
	}

	priority := smartBufferDefaultPriority
	if item, ok := e.(*transaction); ok {
		priority = smartBufferPriority(item)
	}

	heap.Push(&b.elements, smartBufferEntry{
		value:    e,
		priority: priority,
		seq:      b.nextSeq,
	})
	b.nextSeq++
}

func (b *SmartBuffer) Pop() interface{} {
	if len(b.elements) == 0 {
		return nil
	}

	entry := heap.Pop(&b.elements).(smartBufferEntry)
	return entry.value
}

// RemoveByValue removes the entry whose value == v (a specific transaction),
// not the heap root. writeBufferStage's fall-through commits a non-head
// transaction when the head (a coalesce blocked on the full coalesce-band)
// cannot proceed; that committed transaction must be removed by identity rather
// than by Pop, which would drop the wrong (head) entry.
func (b *SmartBuffer) RemoveByValue(v interface{}) bool {
	for i, e := range b.elements {
		if e.value == v {
			heap.Remove(&b.elements, i)
			return true
		}
	}
	return false
}

func (b *SmartBuffer) Peek() interface{} {
	if len(b.elements) == 0 {
		return nil
	}

	return b.elements[0].value
}

func (b *SmartBuffer) Capacity() int {
	return b.capacity
}

func (b *SmartBuffer) Size() int {
	return len(b.elements)
}

func (b *SmartBuffer) Clear() {
	b.elements = nil
	b.nextSeq = 0
}
