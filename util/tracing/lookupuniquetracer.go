// LookupUniqueTracer counts lookup outcomes per *unique remote request*
// (identified by ReadReq.Meta().ID). Multiple lookup trials that share the
// same originating LookupReq ID collapse into one outcome:
//   - Hit: at least one trial fired lookup-hit / lookup-mshr-hit /
//     lookup-distribution-hit.
//   - Miss: every trial fired lookup-miss with no hit ever seen.
//
// Hop tracking: each ID's miss events are counted; on the first hit-class
// event for that ID, hop number = (miss count + 1). Multiple hit-class
// events for the same trial (e.g. lookup-hit and lookup-distribution-hit
// firing together) collapse into a single hit at the same hop.
//
// This is intentionally separate from LookupTracer (which counts every
// trial as an independent transaction). Do not modify LookupTracer.
package tracing

import "gitlab.com/akita/protocol"

type lookupUniqueEntry struct {
	missCount int  // trials so far that missed (only counted while status != hit)
	hit       bool // true once any hit-class event was seen for this id
	miss      bool // true once any miss event was seen for this id
}

type LookupUniqueTracer struct {
	taskFilter TaskFilter

	entries    map[uint64]*lookupUniqueEntry
	entriesPte map[uint64]*lookupUniqueEntry

	uniqueHitCount  uint64
	uniqueMissCount uint64
	hitAtHop        []uint64 // hitAtHop[h] = # IDs whose first hit was the h-th trial (1-indexed); idx 0 unused

	uniqueHitCountPte  uint64
	uniqueMissCountPte uint64
	hitAtHopPte        []uint64
}

func NewLookupUniqueTracer() *LookupUniqueTracer {
	return &LookupUniqueTracer{
		entries:     make(map[uint64]*lookupUniqueEntry),
		entriesPte:  make(map[uint64]*lookupUniqueEntry),
		hitAtHop:    []uint64{0}, // idx 0 unused; grown on demand
		hitAtHopPte: []uint64{0},
	}
}

func (t *LookupUniqueTracer) StartTask(task Task) {
	switch task.Kind {
	case "lookup-hit", "lookup-mshr-hit", "lookup-distribution-hit":
		readReq, ok := task.Detail.(*protocol.ReadReq)
		if !ok {
			return
		}
		t.markHit(readReq.Meta().ID, readReq.IsPtw)
	case "lookup-miss":
		readReq, ok := task.Detail.(*protocol.ReadReq)
		if !ok {
			return
		}
		t.markMiss(readReq.Meta().ID, readReq.IsPtw)
	}
}

func (t *LookupUniqueTracer) StepTask(task Task) {}
func (t *LookupUniqueTracer) EndTask(task Task)  {}

func (t *LookupUniqueTracer) markHit(id uint64, isPtw bool) {
	e, ok := t.entries[id]
	if !ok {
		e = &lookupUniqueEntry{}
		t.entries[id] = e
	}
	if !e.hit {
		hop := e.missCount + 1
		t.hitAtHop = growHopSlice(t.hitAtHop, hop)
		t.hitAtHop[hop]++

		if e.miss {
			t.uniqueMissCount--
		}
		t.uniqueHitCount++
		e.hit = true
	}

	if !isPtw {
		return
	}
	eP, ok := t.entriesPte[id]
	if !ok {
		eP = &lookupUniqueEntry{}
		t.entriesPte[id] = eP
	}
	if !eP.hit {
		hop := eP.missCount + 1
		t.hitAtHopPte = growHopSlice(t.hitAtHopPte, hop)
		t.hitAtHopPte[hop]++

		if eP.miss {
			t.uniqueMissCountPte--
		}
		t.uniqueHitCountPte++
		eP.hit = true
	}
}

func (t *LookupUniqueTracer) markMiss(id uint64, isPtw bool) {
	e, ok := t.entries[id]
	if !ok {
		e = &lookupUniqueEntry{}
		t.entries[id] = e
	}
	if !e.hit {
		e.missCount++
		if !e.miss {
			e.miss = true
			t.uniqueMissCount++
		}
	}

	if !isPtw {
		return
	}
	eP, ok := t.entriesPte[id]
	if !ok {
		eP = &lookupUniqueEntry{}
		t.entriesPte[id] = eP
	}
	if !eP.hit {
		eP.missCount++
		if !eP.miss {
			eP.miss = true
			t.uniqueMissCountPte++
		}
	}
}

// growHopSlice ensures s has index `hop` available, zero-filling new slots.
func growHopSlice(s []uint64, hop int) []uint64 {
	for len(s) <= hop {
		s = append(s, 0)
	}
	return s
}

// Getters

func (t *LookupUniqueTracer) GetUniqueHitCount() uint64 {
	return t.uniqueHitCount
}

func (t *LookupUniqueTracer) GetUniqueMissCount() uint64 {
	return t.uniqueMissCount
}

func (t *LookupUniqueTracer) GetUniqueTotal() uint64 {
	return t.uniqueHitCount + t.uniqueMissCount
}

// GetHitAtHopCounts returns hits-by-trial-number. Index 0 is unused;
// index h is the number of unique requests whose first hit occurred on
// the h-th trial.
func (t *LookupUniqueTracer) GetHitAtHopCounts() []uint64 {
	return t.hitAtHop
}

func (t *LookupUniqueTracer) GetUniqueHitCountPte() uint64 {
	return t.uniqueHitCountPte
}

func (t *LookupUniqueTracer) GetUniqueMissCountPte() uint64 {
	return t.uniqueMissCountPte
}

func (t *LookupUniqueTracer) GetUniqueTotalPte() uint64 {
	return t.uniqueHitCountPte + t.uniqueMissCountPte
}

func (t *LookupUniqueTracer) GetHitAtHopCountsPte() []uint64 {
	return t.hitAtHopPte
}
