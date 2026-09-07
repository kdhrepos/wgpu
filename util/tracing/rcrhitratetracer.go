// RcrHitRateTracer reports hit/miss for data forwarding scheme of NearFetch.
// Every outcome is split into data vs PTE (page-table-walk) traffic via the
// DataForwardingDetail.IsPtw flag, so callers can report data-only, pte-only,
// and the data+pte total. The mshr-coalesced count is not PTE-split.
package tracing

// DataForwardingDetail is the Detail payload of the data-forwarding tasks.
// Hop is the 1-indexed intermediate-chiplet lookup at which a (mshr-)hit
// occurred (0 when not applicable). IsPtw marks page-table-walk traffic.
// PC is the originating instruction PC (0 for PTW / when unknown), used by
// PerPCLookupTracer to bucket outcomes per PC.
type DataForwardingDetail struct {
	Hop   int
	IsPtw bool
	PC    uint64
}

// PCFromInfo returns the PC carried by a request's Info field (req.Info is
// uint64 PC for non-PTW reads, 0 for PTW). Used by trace emit sites and the
// per-PC lookup tracer.
func PCFromInfo(info interface{}) uint64 {
	if pc, ok := info.(uint64); ok {
		return pc
	}
	return 0
}

// rcrCounts holds one traffic class (data or PTE).
type rcrCounts struct {
	hit       uint64
	mshrHit   uint64
	miss      uint64
	reachedGH uint64
	// hitAtHop[h] / mshrHitAtHop[h] = number of (mshr-)hits at the h-th
	// intermediate-chiplet lookup along the path (1-indexed; index 0 unused).
	hitAtHop     []uint64
	mshrHitAtHop []uint64
}

// RcrSnapshot is an immutable read-out of one traffic class.
type RcrSnapshot struct {
	Hit          uint64
	MshrHit      uint64
	Miss         uint64
	ReachedGH    uint64
	HitAtHop     []uint64
	MshrHitAtHop []uint64
}

type RcrHitRateTracer struct {
	taskFilter TaskFilter

	data rcrCounts
	pte  rcrCounts

	// mshrCoalescedReqCount is a coalescing artifact, not PTE-split.
	mshrCoalescedReqCount uint64
}

func NewRcrHitRateTracer(taskFilter TaskFilter) *RcrHitRateTracer {
	return &RcrHitRateTracer{
		taskFilter: taskFilter,
		data:       rcrCounts{hitAtHop: []uint64{0}, mshrHitAtHop: []uint64{0}},
		pte:        rcrCounts{hitAtHop: []uint64{0}, mshrHitAtHop: []uint64{0}},
	}
}

func (t *RcrHitRateTracer) StartTask(task Task) {
	if !t.taskFilter(task) {
		return
	}

	if task.Kind == "data-forwarding-mshr-coalesced" {
		t.mshrCoalescedReqCount++
		return
	}

	d, ok := task.Detail.(*DataForwardingDetail)
	if !ok {
		return
	}
	c := &t.data
	if d.IsPtw {
		c = &t.pte
	}

	switch task.Kind {
	case "data-forwarding-hit":
		c.hit++
		recordHopHit(&c.hitAtHop, d.Hop)
	case "data-forwarding-mshr-hit":
		c.mshrHit++
		recordHopHit(&c.mshrHitAtHop, d.Hop)
	case "data-forwarding-miss":
		c.miss++
	case "data-forwarding-reached-gh":
		c.reachedGH++
	default:
		panic("RCR Hit Rate Tracer")
	}
}

// recordHopHit buckets a hit by its hop number. A non-positive hop is ignored
// so the slice never under-indexes.
func recordHopHit(bucket *[]uint64, hop int) {
	if hop <= 0 {
		return
	}
	*bucket = growHopSlice(*bucket, hop)
	(*bucket)[hop]++
}

func (t *RcrHitRateTracer) StepTask(task Task) {}
func (t *RcrHitRateTracer) EndTask(task Task)  {}

// Data returns the non-PTE traffic snapshot.
func (t *RcrHitRateTracer) Data() RcrSnapshot { return t.data.snapshot() }

// Pte returns the PTE traffic snapshot.
func (t *RcrHitRateTracer) Pte() RcrSnapshot { return t.pte.snapshot() }

// GetMshrCoalescedReqCount returns the (non-PTE-split) coalesced request count.
func (t *RcrHitRateTracer) GetMshrCoalescedReqCount() uint64 {
	return t.mshrCoalescedReqCount
}

func (c *rcrCounts) snapshot() RcrSnapshot {
	return RcrSnapshot{
		Hit:          c.hit,
		MshrHit:      c.mshrHit,
		Miss:         c.miss,
		ReachedGH:    c.reachedGH,
		HitAtHop:     c.hitAtHop,
		MshrHitAtHop: c.mshrHitAtHop,
	}
}
