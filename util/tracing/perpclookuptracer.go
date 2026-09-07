// PerPCLookupTracer buckets lookup-chain outcomes per originating PC for both
// platform families that use a remote-lookup scheme:
//
//   - dynamic-clustering / dynamic-dist family: tasks "lookup-hit",
//     "lookup-mshr-hit", "lookup-distribution-hit", "lookup-miss" emitted by
//     the writeback-dynamic-* L2 directorystage. Detail = *protocol.ReadReq;
//     PC = req.Info (uint64); PTW skipped (req.IsPtw).
//
//   - nearfetch family (writebackrcr / writebackrcr2): tasks
//     "data-forwarding-hit", "data-forwarding-mshr-hit",
//     "data-forwarding-miss" emitted by the L2 plus
//     "data-forwarding-reached-gh" emitted by the RcrDmaEngine. Detail =
//     *DataForwardingDetail; PC and PTW carried explicitly on the detail.
//
// PTW (page-table-walk) requests are excluded — PTW reads carry no PC.
//
// Output bucket per PC: hit, mshrHit, distHit, miss, reachedGH. The "miss"
// bucket on the dynamic-* side and "reachedGH" on the nearfetch side both
// mean "the chain failed to find a replica"; they are kept as separate
// columns so the consumer can tell the two task families apart.
package tracing

import (
	"sync"

	"gitlab.com/akita/protocol"
)

// PerPCCounts is the per-PC outcome bucket.
type PerPCCounts struct {
	Hit       uint64
	MshrHit   uint64
	DistHit   uint64 // dynamic-* lookup-distribution-hit
	Miss      uint64 // dynamic-* lookup-miss
	ReachedGH uint64 // nearfetch data-forwarding-reached-gh
}

// PerPCLookupTracer accumulates per-PC lookup outcomes across both task
// families. Safe for concurrent StartTask calls.
type PerPCLookupTracer struct {
	mu     sync.Mutex
	counts map[uint64]*PerPCCounts
}

func NewPerPCLookupTracer() *PerPCLookupTracer {
	return &PerPCLookupTracer{counts: map[uint64]*PerPCCounts{}}
}

func (t *PerPCLookupTracer) StartTask(task Task) {
	var (
		pc   uint64
		kind = task.Kind
	)

	switch kind {
	case "lookup-hit", "lookup-mshr-hit", "lookup-distribution-hit", "lookup-miss":
		r, ok := task.Detail.(*protocol.ReadReq)
		if !ok {
			return
		}
		if r.IsPtw {
			return
		}
		pc = PCFromInfo(r.Info)
	case "data-forwarding-hit", "data-forwarding-mshr-hit",
		"data-forwarding-miss", "data-forwarding-reached-gh":
		d, ok := task.Detail.(*DataForwardingDetail)
		if !ok {
			return
		}
		if d.IsPtw {
			return
		}
		pc = d.PC
	default:
		return
	}

	t.mu.Lock()
	c, ok := t.counts[pc]
	if !ok {
		c = &PerPCCounts{}
		t.counts[pc] = c
	}
	switch kind {
	case "lookup-hit", "data-forwarding-hit":
		c.Hit++
	case "lookup-mshr-hit", "data-forwarding-mshr-hit":
		c.MshrHit++
	case "lookup-distribution-hit":
		c.DistHit++
	case "lookup-miss", "data-forwarding-miss":
		c.Miss++
	case "data-forwarding-reached-gh":
		c.ReachedGH++
	}
	t.mu.Unlock()
}

func (t *PerPCLookupTracer) StepTask(task Task) {}
func (t *PerPCLookupTracer) EndTask(task Task)  {}

// Snapshot returns a copy of the per-PC counts. The returned map is
// independent of the tracer's internal state.
func (t *PerPCLookupTracer) Snapshot() map[uint64]PerPCCounts {
	t.mu.Lock()
	defer t.mu.Unlock()
	out := make(map[uint64]PerPCCounts, len(t.counts))
	for pc, c := range t.counts {
		out[pc] = *c
	}
	return out
}

// MergeSnapshots adds the per-PC counts from b into a (in place). Used when
// the tracer is attached as one instance per cache/engine and the runner
// rolls them up before CSV emit.
func MergeSnapshots(a, b map[uint64]PerPCCounts) {
	for pc, bc := range b {
		ac := a[pc]
		ac.Hit += bc.Hit
		ac.MshrHit += bc.MshrHit
		ac.DistHit += bc.DistHit
		ac.Miss += bc.Miss
		ac.ReachedGH += bc.ReachedGH
		a[pc] = ac
	}
}
