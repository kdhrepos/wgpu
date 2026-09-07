// LookupOutcomeTracer records the outcome of each L2-bypass lookup *chain*
// on the dynamic-dist(-prefetch) platform. One record per chain:
//
//   - hit: the chain found a replica at some local home. Hop = which lookup
//     trial hit (1 = first LH probed); SameCluster = the hitting LH lives in
//     the requestor's own cluster.
//   - reached-global-home: every probed LH missed and the chain fell back to
//     the global home.
//   - eligible / skipped (no-lh-on-path): a request is "eligible" when LH
//     enumeration ran (scheme != 0) and the global home is in another cluster,
//     so a lookup would have been worthwhile -- this is the denominator.
//     "skipped" is the subset of eligible where no local home lay on the
//     requestor->global-home minimal path, so no lookup was sent (the
//     numerator). Requests whose global home is in the requestor's own cluster
//     (lookup unnecessary by construction) are excluded from both.
//
// Every outcome is bucketed into data vs PTE (page-table-walk) traffic via
// the LookupReq.IsPtw flag, so callers can report data-only, pte-only, and
// the data+pte total.
//
// Recorded at the RDMA engine (not the L2) because only the engine knows the
// originating requestor's cluster, the hitting node's cluster, and the stable
// per-chain Hop counter. The per-trial LookupTracer and the ID-keyed
// LookupUniqueTracer cannot resolve these for dynamic-dist because each hop's
// local L2 query carries a fresh message ID.
package tracing

// LookupOutcome is the Detail payload of the lookup-outcome tasks.
type LookupOutcome struct {
	Distance    int  // mesh-hop distance requestor->hitting LH (real hops, not trial #)
	SameCluster bool // hitting local home is in the requestor's cluster
	IsPtw       bool // request is a page-table-walk fetch
}

// lookupOutcomeCounts holds one traffic class (data or PTE) of outcomes.
type lookupOutcomeCounts struct {
	hitAtDistance []uint64 // idx 0 unused; [d] = chains that hit at mesh distance d
	own           uint64
	other         uint64
	gh            uint64
	eligible      uint64 // GH in another cluster: a lookup was worthwhile (denominator)
	skipped       uint64 // ...of which no LH on the minimal path -> no lookup sent
}

// LookupOutcomeSnapshot is an immutable read-out of one traffic class.
type LookupOutcomeSnapshot struct {
	HitAtDistance []uint64
	Own           uint64
	Other         uint64
	GH            uint64
	Eligible      uint64
	Skipped       uint64
}

type LookupOutcomeTracer struct {
	data lookupOutcomeCounts
	pte  lookupOutcomeCounts
}

func NewLookupOutcomeTracer() *LookupOutcomeTracer {
	return &LookupOutcomeTracer{
		data: lookupOutcomeCounts{hitAtDistance: []uint64{0}},
		pte:  lookupOutcomeCounts{hitAtDistance: []uint64{0}},
	}
}

func (t *LookupOutcomeTracer) StartTask(task Task) {
	o, ok := task.Detail.(*LookupOutcome)
	if !ok {
		return
	}
	c := &t.data
	if o.IsPtw {
		c = &t.pte
	}

	switch task.Kind {
	case "lookup-outcome-hit":
		if o.Distance > 0 {
			c.hitAtDistance = growHopSlice(c.hitAtDistance, o.Distance)
			c.hitAtDistance[o.Distance]++
		}
		if o.SameCluster {
			c.own++
		} else {
			c.other++
		}
	case "lookup-outcome-reached-gh":
		c.gh++
	case "lookup-outcome-eligible":
		c.eligible++
	case "lookup-outcome-skipped":
		c.skipped++
	}
}

func (t *LookupOutcomeTracer) StepTask(task Task) {}
func (t *LookupOutcomeTracer) EndTask(task Task)  {}

// Data returns the non-PTE traffic snapshot.
func (t *LookupOutcomeTracer) Data() LookupOutcomeSnapshot {
	return t.data.snapshot()
}

// Pte returns the PTE traffic snapshot.
func (t *LookupOutcomeTracer) Pte() LookupOutcomeSnapshot {
	return t.pte.snapshot()
}

func (c *lookupOutcomeCounts) snapshot() LookupOutcomeSnapshot {
	return LookupOutcomeSnapshot{
		HitAtDistance: c.hitAtDistance,
		Own:           c.own,
		Other:         c.other,
		GH:            c.gh,
		Eligible:      c.eligible,
		Skipped:       c.skipped,
	}
}
