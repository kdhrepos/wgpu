package rob

import (
	"container/list"

	"gitlab.com/akita/akita"
)

// A Builder can build ReorderBuffers.
type Builder struct {
	engine         akita.Engine
	freq           akita.Freq
	numReqPerCycle int
	bufferSize     int
	outOfOrder     bool
	perWfSubFifo   bool
}

// MakeBuilder creates a builder with default parameters.
func MakeBuilder() Builder {
	return Builder{
		freq:           1 * akita.GHz,
		numReqPerCycle: 4,
		bufferSize:     128,
	}
}

// WithEngine sets the engine to use.
func (b Builder) WithEngine(engine akita.Engine) Builder {
	b.engine = engine
	return b
}

// WithFreq sets the frequency that the ReorderBuffer works at.
func (b Builder) WithFreq(freq akita.Freq) Builder {
	b.freq = freq
	return b
}

// WithNumReqPerCycle sets the number of request that the ReorderBuffer can
// handle in each cycle.
func (b Builder) WithNumReqPerCycle(n int) Builder {
	b.numReqPerCycle = n
	return b
}

// WithBufferSize sets the number of transactions that the buffer can handle.
func (b Builder) WithBufferSize(n int) Builder {
	b.bufferSize = n
	return b
}

// WithOutOfOrder enables out-of-order completion: responses are released to
// the top component in arrival order rather than strict FIFO order. Only safe
// when the consumer matches responses by request ID (the vector memory path of
// the CU). The scalar and instruction paths still assume in-order arrival, so
// their ROBs must keep this disabled (unless combined with WithPerWfSubFifo,
// which preserves per-WF program order).
func (b Builder) WithOutOfOrder(enabled bool) Builder {
	b.outOfOrder = enabled
	return b
}

// WithPerWfSubFifo splits the ROB into one sub-FIFO per wavefront ID. Each
// sub-FIFO retires in program order (so InFlight[0]-style scalar/inst handlers
// remain correct), but sub-FIFOs drain independently — a slow head in WF-A no
// longer blocks WF-B's ready response. Eliminates the cross-WF HoL component
// without enabling the cross-WF reordering that breaks scalar/inst handlers.
// Orthogonal to WithOutOfOrder: when both are set, each sub-FIFO retires its
// own oldest ready entry (intra-WF OOO).
func (b Builder) WithPerWfSubFifo(enabled bool) Builder {
	b.perWfSubFifo = enabled
	return b
}

// Build creates a ReorderBuffer with the given parameters.
func (b Builder) Build(name string) *ReorderBuffer {
	rb := &ReorderBuffer{}

	rb.TickingComponent = akita.NewTickingComponent(name, b.engine, b.freq, rb)

	rb.transactions = list.New()
	rb.transactions.Init()
	rb.toBottomReqIDToTransactionTable = make(map[uint64]*list.Element)

	rb.bufferSize = b.bufferSize
	rb.numReqPerCycle = b.numReqPerCycle
	rb.outOfOrder = b.outOfOrder
	rb.perWfSubFifo = b.perWfSubFifo
	if b.outOfOrder {
		rb.readyElems = make([]*list.Element, 0, b.bufferSize)
	}
	if b.perWfSubFifo {
		rb.transactionsByWf = make(map[string]*list.List)
	}

	rb.TopPort = akita.NewLimitNumMsgPort(
		rb,
		2*b.numReqPerCycle,
		name+".TopPort",
	)
	rb.BottomPort = akita.NewLimitNumMsgPort(
		rb,
		2*b.numReqPerCycle,
		name+".BottomPort",
	)
	rb.ControlPort = akita.NewLimitNumMsgPort(
		rb,
		1,
		name+".ControlPort",
	)

	return rb
}
