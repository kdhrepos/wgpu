package writeback

import (
	"container/heap"
	"log"
)

const (
	smartBufferLocalPriority   = 0
	smartBufferDefaultPriority = 1
)

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

// smartBufferLocalReserve is the minimum number of dirToBank / writeBuffer slots
// kept free for local (toRemote=false) transactions, so the local-home serve
// (the drain) is never starved by a remote-fetch flood. Matches the MSHR local
// reserve (4).
const smartBufferLocalReserve = 4

// CanPushTransReserve admits with a local reserve: a remote (toRemote=true)
// transaction may use only capacity-reserve slots, so a local (toRemote=false)
// transaction — the local-home serve that produces responses and drains the
// pipeline — always keeps room. Mirrors writebackrcr's SmartBuffer.
func (b *SmartBuffer) CanPushTransReserve(toRemote bool, reserve int) bool {
	if toRemote {
		return len(b.elements) < b.capacity-reserve
	}
	return len(b.elements) < b.capacity
}

func (b *SmartBuffer) Push(e interface{}) {
	if len(b.elements) >= b.capacity {
		log.Panic("smart buffer overflow")
	}

	priority := smartBufferDefaultPriority
	if item, ok := e.(*transaction); ok {
		if !item.toRemote {
			priority = smartBufferLocalPriority
		}
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

// ClassCounts returns how many buffered transactions are local-home
// (toRemote=false) vs remote (toRemote=true). Non-transaction entries are
// ignored. Used by DeadlockSummary to show the class mix held in the buffer.
func (b *SmartBuffer) ClassCounts() (local, remote int) {
	for _, e := range b.elements {
		t, ok := e.value.(*transaction)
		if !ok {
			continue
		}
		if t.toRemote {
			remote++
		} else {
			local++
		}
	}
	return
}
