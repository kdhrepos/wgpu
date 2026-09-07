package writebacksac

import (
	"log"
)

// LookupBuffer holds cachePipelineItems ordered by transaction message ID
// (ascending). The directory stage picks the first item whose target block
// is ready (not locked, no active readers), preserving same-line ordering
// while allowing later-arrived independent transactions to bypass a stalled
// head. Implements util.Buffer.
type LookupBuffer struct {
	capacity int
	elements []cachePipelineItem
}

// NewLookupBuffer creates an empty LookupBuffer with the given capacity.
func NewLookupBuffer(capacity int) *LookupBuffer {
	return &LookupBuffer{capacity: capacity}
}

func (b *LookupBuffer) CanPush() bool {
	return len(b.elements) < b.capacity
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

// transPriorityKey returns (priority, id) for sort ordering. priority 0 is
// applied to transactions whose destination is the current chiplet
// (toRemote=false) so they sort ahead of forward-to-remote transactions
// (priority 1). Within the same priority, transactions sort by id ascending
// to preserve same-line ordering.
func transPriorityKey(t *transaction) (int, uint64) {
	p := 1
	if !t.toRemote {
		p = 0
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
