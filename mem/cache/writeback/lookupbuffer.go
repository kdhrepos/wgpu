package writeback

import (
	"log"
)

// PriorityFn returns (priority, id) for a transaction. Lower priority dequeues
// first; within the same priority, lower id dequeues first.
type PriorityFn func(*transaction) (int, uint64)

// LookupBuffer holds cachePipelineItems ordered by (priority, id) as defined
// by the injected PriorityFn. The directory stage picks the first item whose
// target block is ready (not locked, no active readers), preserving same-line
// ordering while allowing later-arrived independent transactions to bypass a
// stalled head. Implements util.Buffer.
type LookupBuffer struct {
	capacity     int
	localReserve int
	elements     []cachePipelineItem
	keyFn        PriorityFn
}

// NewLookupBuffer creates an empty LookupBuffer with the given capacity,
// local-reserve, and priority-key function. keyFn must be non-nil. localReserve
// slots are kept free for local (toRemote=false) transactions so a remote flood
// can never starve local admission — see CanPushTransClass.
func NewLookupBuffer(capacity, localReserve int, keyFn PriorityFn) *LookupBuffer {
	return &LookupBuffer{
		capacity:     capacity,
		localReserve: localReserve,
		keyFn:        keyFn,
	}
}

// KeyFn returns the priority key function used to sort this buffer. Exposed so
// the directory stage can compare items across two buffers consistently.
func (b *LookupBuffer) KeyFn() PriorityFn { return b.keyFn }

func (b *LookupBuffer) CanPush() bool {
	return len(b.elements) < b.capacity
}

// CanPushTransClass admits with reserve awareness: a local (toRemote=false)
// transaction may use the whole buffer, while a remote one is capped at
// capacity-localReserve so local transactions always keep room. This lets the
// directory keep draining local work (which never blocks on a remote budget)
// even when remote transactions pile up waiting for numToRemote to free.
func (b *LookupBuffer) CanPushTransClass(toRemote bool) bool {
	if !toRemote {
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
	pNew, idNew := b.keyFn(item.trans)
	idx := len(b.elements)
	for i, e := range b.elements {
		pE, idE := b.keyFn(e.trans)
		if pE > pNew || (pE == pNew && idE > idNew) {
			idx = i
			break
		}
	}
	b.elements = append(b.elements, cachePipelineItem{})
	copy(b.elements[idx+1:], b.elements[idx:])
	b.elements[idx] = item
}

// priorityNoPTE: local (0) < remote (1). The only priority function.
func priorityNoPTE(t *transaction) (int, uint64) {
	if !t.toRemote {
		return 0, transOrderKey(t)
	}
	return 1, transOrderKey(t)
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

// ClassCounts returns how many buffered transactions are local-home
// (toRemote=false) vs remote (toRemote=true). Used by DeadlockSummary to show
// the class mix held in the buffer.
func (b *LookupBuffer) ClassCounts() (local, remote int) {
	for _, e := range b.elements {
		if e.trans.toRemote {
			remote++
		} else {
			local++
		}
	}
	return
}

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
