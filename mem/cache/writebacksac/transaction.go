package writebacksac

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
	writeBufferPassThrough
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

	// SAC pass-through: the request skips directory lookup, MSHR and
	// install, and is forwarded 1:1 to the bottom (RDMA for memory-side
	// local requests to remote addresses, DRAM for SM-side inbound
	// requests). ptForwardedReq is the forwarded ReadReq/WriteReq used to
	// match the bottom response.
	isPassThrough  bool
	ptForwardedReq akita.Msg

	// failTraced ensures an MSHR reservation/merge fail is counted at most
	// once per request (PeekReady re-probes a stalled transaction every
	// cycle, which would otherwise inflate the count by the stall duration).
	failTraced bool

	// pipelineReadyCycle is the absolute cycle at which this transaction
	// becomes eligible for directory lookup. Mirrors the deadline-stamp
	// pattern in noc/networking/switching/multiflitswitchconnection.go so
	// the cache does not busy-tick across pipelineLatency cycles.
	pipelineReadyCycle uint64
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
