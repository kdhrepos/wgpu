package tracing

import (
	"strconv"
	"sync"
)

// Task IDs are derived from a base ID (a message / event / transaction ID, all
// produced by akita.GetIDGenerator() and therefore monotonic and small) by
// packing a task "kind" and an optional domain ID into the high bits. This
// replaces the old string-suffix scheme (base+"_req_out", base+"@domain", ...)
// that allocated millions of short-lived strings and dominated GC.
//
// Bit layout (64 bits total):
//
//	[0..45]  base ID   (46 bits, ~7e13 — far above any realistic msg/event count)
//	[46..49] kind      (4 bits, up to 15 task kinds)
//	[50..63] domain ID (14 bits, up to 16384 distinct traced domains)
//
// Only kindReqIn uses the domain field; the others leave it 0. Distinct kinds
// guarantee that the several task types sharing one base ID never collide.
const (
	taskBaseBits    = 46
	taskKindBits    = 4
	taskBaseMask    = (uint64(1) << taskBaseBits) - 1
	taskKindMask    = (uint64(1) << taskKindBits) - 1
	taskKindShift   = taskBaseBits
	taskDomainShift = taskBaseBits + taskKindBits
)

const (
	kindReqOut   uint64 = 1
	kindReqIn          uint64 = 2
	kindTransReq       uint64 = 3
	kindPipeline       uint64 = 4
	kindAddrTransStats uint64 = 5
	kindFetch          uint64 = 6
	kindTLBMshrWait    uint64 = 7
	kindSimdExec       uint64 = 8
	kindL2TLBStats     uint64 = 9
	kindStall          uint64 = 10
	kindMMUMemLatency  uint64 = 11
	kindNetFlightReq   uint64 = 12
	kindNetFlightRsp   uint64 = 13
	kindRDMAFlightReq  uint64 = 14
	kindRDMAFlightRsp  uint64 = 15
)

func encodeTaskID(base, kind, domain uint64) uint64 {
	return (base & taskBaseMask) |
		((kind & taskKindMask) << taskKindShift) |
		(domain << taskDomainShift)
}

// ReqOutTaskID returns the task ID of the "req_out" task for the given base ID.
func ReqOutTaskID(base uint64) uint64 { return encodeTaskID(base, kindReqOut, 0) }

// TransReqTaskID returns the task ID of the network "trace-trans-req" task.
func TransReqTaskID(base uint64) uint64 { return encodeTaskID(base, kindTransReq, 0) }

// PipelineTaskID returns the task ID of the "pipeline" task for the given base.
func PipelineTaskID(base uint64) uint64 { return encodeTaskID(base, kindPipeline, 0) }

// AddrTransStatsTaskID returns the task ID of the address-translator stats task.
func AddrTransStatsTaskID(base uint64) uint64 { return encodeTaskID(base, kindAddrTransStats, 0) }

// FetchTaskID returns the task ID of the cache "fetch" task for the given base.
func FetchTaskID(base uint64) uint64 { return encodeTaskID(base, kindFetch, 0) }

// TLBMshrWaitTaskID returns the task ID of the TLB MSHR-wait task.
func TLBMshrWaitTaskID(base uint64) uint64 { return encodeTaskID(base, kindTLBMshrWait, 0) }

// SimdExecTaskID returns the task ID of the SIMD-exec task for the given base.
func SimdExecTaskID(base uint64) uint64 { return encodeTaskID(base, kindSimdExec, 0) }

// L2TLBStatsTaskID returns the task ID of the L2 TLB stats task.
func L2TLBStatsTaskID(base uint64) uint64 { return encodeTaskID(base, kindL2TLBStats, 0) }

// StallTaskID returns the task ID of a per-component "stall" task, keyed by the
// component name (which has no numeric base ID of its own).
func StallTaskID(name string) uint64 { return encodeTaskID(domainID(name), kindStall, 0) }

// MMUMemLatencyTaskID returns the task ID of the MMU memory-latency task.
func MMUMemLatencyTaskID(base uint64) uint64 { return encodeTaskID(base, kindMMUMemLatency, 0) }

// DecimalTaskID converts a decimal-encoded generator ID kept as a string
// (wavefront/workgroup UIDs, driver command IDs, the simulation ID — coarse
// identifiers retained as strings for logging and map keys) into the uint64
// task ID used by the tracing API. The parsed value occupies the low base bits
// with a zero "kind", so it never collides with the encoded task kinds above.
// Parsing allocates nothing.
func DecimalTaskID(s string) uint64 {
	n, _ := strconv.ParseUint(s, 10, 64)
	return n
}

// NetworkFlightTaskID returns the task ID of a NetworkFlightTracer flight task.
// The request and response directions get distinct kinds so the two never
// collide on a single message ID.
func NetworkFlightTaskID(base uint64, kind string) uint64 {
	if kind == NetworkFlightKindRsp {
		return encodeTaskID(base, kindNetFlightRsp, 0)
	}
	return encodeTaskID(base, kindNetFlightReq, 0)
}

// RDMAFlightTaskID returns the task ID of an RDMAFlightTracer flight task. It
// uses kinds distinct from NetworkFlightTaskID so the two flight tracers never
// collide on a single message ID.
func RDMAFlightTaskID(base uint64, kind string) uint64 {
	if kind == RDMAFlightKindRsp {
		return encodeTaskID(base, kindRDMAFlightRsp, 0)
	}
	return encodeTaskID(base, kindRDMAFlightReq, 0)
}

// Domain ID registry. Each traced domain (component) gets a small, stable,
// per-run integer ID assigned on first use. The mapping only has to be
// self-consistent within a single simulation run, which a first-come counter
// trivially satisfies.
var (
	domainIDMu     sync.Mutex
	domainIDByName = make(map[string]uint64)
	nextDomainID   uint64
)

func domainID(name string) uint64 {
	domainIDMu.Lock()
	id, ok := domainIDByName[name]
	if !ok {
		id = nextDomainID
		nextDomainID++
		domainIDByName[name] = id
	}
	domainIDMu.Unlock()
	return id
}
