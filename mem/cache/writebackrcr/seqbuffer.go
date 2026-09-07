package writebackrcr

import "log"

// NewSeqBuffer creates a default buffer object.
func NewSeqBuffer(capacity int) *SeqBuffer {
	return &SeqBuffer{capacity: capacity}
}

type SeqBuffer struct {
	capacity int
	elements []interface{}
}

func (b *SeqBuffer) CanPush() bool {
	return len(b.elements) < b.capacity
}

func (b *SeqBuffer) Push(e interface{}) {
	if len(b.elements) >= b.capacity {
		log.Panic("smart buffer overflow")
	}

	// For transaction entries, sort by (priority, id) so that toRemote=false
	// transactions stay ahead of toRemote=true ones, and within the same
	// priority lower msg IDs (= earlier generated transactions) come first.
	// Non-transaction entries (none expected in practice) just append.
	tNew, ok := e.(*transaction)
	if !ok {
		b.elements = append(b.elements, e)
		return
	}

	pNew := 1
	if !tNew.toRemote {
		pNew = 0
	}
	idNew := transOrderKey(tNew)

	for i, elem := range b.elements {
		queued, ok := elem.(*transaction)
		if !ok {
			panic("unexpected element in seq buffer")
		}
		pE := 1
		if !queued.toRemote {
			pE = 0
		}
		idE := transOrderKey(queued)
		if pE > pNew || (pE == pNew && idE > idNew) {
			b.elements = append(b.elements, nil)
			copy(b.elements[i+1:], b.elements[i:])
			b.elements[i] = e
			return
		}
	}
	b.elements = append(b.elements, e)
}

func (b *SeqBuffer) Pop() interface{} {
	if len(b.elements) == 0 {
		return nil
	}

	e := b.elements[0]
	b.elements = b.elements[1:]
	return e
}

func (b *SeqBuffer) Peek() interface{} {
	if len(b.elements) == 0 {
		return nil
	}

	return b.elements[0]
}

func (b *SeqBuffer) Capacity() int {
	return b.capacity
}

func (b *SeqBuffer) Size() int {
	return len(b.elements)
}

func (b *SeqBuffer) Clear() {
	b.elements = nil
}
