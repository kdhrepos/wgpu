package util

import "log"

// A Buffer is a fifo queue for anything
type Buffer interface {
	CanPush() bool
	Push(e interface{})
	Pop() interface{}
	Peek() interface{}
	Capacity() int
	Size() int

	// Remove all elements in the buffer
	Clear()
}

// NewBuffer creates a default buffer object.
func NewBuffer(capacity int) Buffer {
	return &BufferImpl{capacity: capacity}
}

type BufferImpl struct {
	capacity int
	elements []interface{}
}

func (b *BufferImpl) CanPush() bool {
	return len(b.elements) < b.capacity
}

func (b *BufferImpl) Push(e interface{}) {
	if len(b.elements) >= b.capacity {
		log.Panic("buffer overflow")
	}
	b.elements = append(b.elements, e)
}

func (b *BufferImpl) Pop() interface{} {
	if len(b.elements) == 0 {
		return nil
	}

	e := b.elements[0]
	b.elements = b.elements[1:]
	return e
}

func (b *BufferImpl) Peek() interface{} {
	if len(b.elements) == 0 {
		return nil
	}

	return b.elements[0]
}

func (b *BufferImpl) Capacity() int {
	return b.capacity
}

func (b *BufferImpl) Size() int {
	return len(b.elements)
}

func (b *BufferImpl) Clear() {
	b.elements = nil
}

func (b *BufferImpl) GetBuffer() []interface{} {
	return b.elements
}

func (b *BufferImpl) SetBuffer(buf []interface{}) {
	b.elements = buf
}

func (b *BufferImpl) HeadToTail() {
	if len(b.elements) == 0 {
		return
	}

	head := b.elements[0]
	b.elements = b.elements[1:]
	b.elements = append(b.elements, head)
}
