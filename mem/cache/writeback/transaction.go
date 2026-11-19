package writeback

import (
	"gitlab.com/akita/akita"
	"gitlab.com/akita/mem/cache"
	"gitlab.com/akita/protocol"
	"gitlab.com/akita/util/ca"
)

type action int

const (
	actionInvalid action = iota
	bankReadHit
	bankWriteHit
	bankEvict
	bankEvictAndWrite
	bankEvictAndFetch
	bankWriteFetched
	writeBufferFetch
	writeBufferEvictAndFetch
	writeBufferEvictAndWrite
	writeBufferFlush
)

type transaction struct {
	action
	read              *protocol.ReadReq
	write             *protocol.WriteReq
	flush             *cache.FlushReq
	block             *cache.Block
	victim            *cache.Block
	fetchPID          ca.PID
	fetchAddress      uint64
	fetchedData       []byte
	fetchReadReq      *protocol.ReadReq
	evictingPID       ca.PID
	evictingAddr      uint64
	evictingData      []byte
	evictingDirtyMask []bool
	evictionWriteReq  *protocol.WriteReq
	evictionDone      *protocol.WriteDoneRsp
	mshrEntry         *cache.MSHREntry

	toRemote bool

	fromLocal bool

	// pipelineReadyCycle is the cycle at which this transaction becomes
	// visible to the directory stage (admission cycle + pipelineLatency).
	// Replaces the shift-register: the directory gates each item by this
	// deadline, modelling the tag-lookup latency as an event.
	pipelineReadyCycle uint64

	// failTraced ensures an MSHR reservation/merge fail is counted at most
	// once per request (PeekReady re-probes a stalled transaction every
	// cycle, which would otherwise inflate the count by the stall duration).
	failTraced bool

	// mshrBlockedAt is the simulation time when this transaction first hit
	// a full MSHR (mshr-fail). Non-zero iff the transaction stalled waiting
	// for an MSHR slot; cleared implicitly once the transaction advances.
	mshrBlockedAt akita.VTimeInSec
}

func (t transaction) accessReq() protocol.AccessReq {
	if t.read != nil {
		return t.read
	}
	if t.write != nil {
		return t.write
	}
	return nil
}

func (t transaction) req() akita.Msg {
	if t.accessReq() != nil {
		return t.accessReq()
	}
	if t.flush != nil {
		return t.flush
	}
	return nil
}
