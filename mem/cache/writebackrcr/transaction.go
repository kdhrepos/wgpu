package writebackrcr

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

	// MSHR Coalescing, NearFetch
	bankEvictAndMshrCoalesce
	bankMshrCoalesceFetched
	writeBufferMshrCoalesce
	writeBufferEvictAndMshrCoalesce
)

type transaction struct {
	action
	read              *protocol.ReadReq
	readRCR           *protocol.RCReq
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

	// fromLocal marks a transaction admitted from TopPort (an origin CU access),
	// as opposed to a remote home-serve (RDMAPort) or a lookup-chain probe
	// (LookupPort). Only fromLocal transactions emit the memory-trace task, so the
	// LocalRemoteTracer / memory trace count each CU access exactly once at the
	// requestor — matching writeback/.
	fromLocal bool

	// MSHR Coalescing for NearFetch
	// When mshr coalesced request is replied,
	// it needs to know which l1 cache was a requestor
	virtualMshrEntry *cache.MSHREntry

	mshrCoalesceReq *protocol.ReadReq

	isLookup     bool
	isCornerNode bool

	// isCoalesce is the TRUE coalesce class, stamped by topParser at admission as
	// toRemote && hasCornerNode(addr). It is the address-derived predicate the
	// directory stage will later turn into a writeBufferMshrCoalesce action (an
	// L-shaped path delegates the fetch to a corner). It is NOT isLookup &&
	// isCornerNode — that is the corner-SERVE predicate (an incoming corner-lookup
	// that this node fetches from home as a REAL fetch, hasCornerNode=false here).
	// The lookup buffer must classify by isCoalesce because at admission time the
	// action is not assigned yet (SmartBuffer/inflightFetch can use the action /
	// mshrCoalesceReq directly).
	isCoalesce bool

	// hopTraceID carries the origin RDMA hop-trace task ID (see
	// protocol.ReadReq.HopTraceID). Inherited from the incoming lookup chain and
	// stamped onto the outgoing corner-node fetch so the requestor->corner and
	// corner->home legs accumulate onto one HopTracer task.
	hopTraceID uint64

	// hop carries the NearFetch data-forwarding hop counter of the incoming
	// LookupReq (1 = first intermediate chiplet). Emitted as the Detail of
	// the data-forwarding-hit / -mshr-hit task so the RcrHitRateTracer can
	// record at which hop the hit occurred.
	hop int

	// failTraced ensures an MSHR reservation/merge fail is counted at most
	// once per request. PeekReady re-probes a stalled transaction every
	// cycle, so without this flag the count would be inflated by the stall
	// duration. Mirrors writeback's pattern so nearfetch and the baseline
	// emit comparable mshr-fail / mshr-merge-fail metrics.
	failTraced bool

	// mshrBlockedAt is the simulation time when this transaction first hit
	// a full MSHR (mshr-fail). Non-zero iff the transaction stalled waiting
	// for an MSHR slot; consumed once the slot is finally allocated to emit
	// the mshr-wait-time metric. Mirrors writeback's pattern.
	mshrBlockedAt akita.VTimeInSec

	// fetchSentTime records when the fetch request (DRAM or RDMA) was sent.
	fetchSentTime akita.VTimeInSec

	// pipelineReadyCycle: absolute cycle at which this trans becomes
	// visible to the directory stage (replaces pipelining shift register).
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
