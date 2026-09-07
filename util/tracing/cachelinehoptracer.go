// cachelinehoptracer accumulates, per cache line (addr>>6) for the whole
// simulation, demand access count and real (simulated, not approximated)
// hop-distance stats, then reports top-K%-by-access-count hop distributions.
package tracing

import (
	"sort"

	"gitlab.com/akita/protocol"
)

// CachelineHopKind tags the per-resolved-access hop-distance task emitted by
// the RDMA engines / cache directory stages once a remote demand access
// (read or write) is served. Distinct from "hop-trace" so HopTracer's
// existing Detail contract ("srcChip.dstChip" string) is untouched.
const CachelineHopKind = "cacheline-hop"

// CachelineHopDetail is the Detail payload of a CachelineHopKind task.
type CachelineHopDetail struct {
	Addr uint64
	Hop  uint64
}

const cacheLineShift = 6 // 64B cache line

type cacheLineStats struct {
	demandCount   uint64
	hopSum        uint64
	hopEventCount uint64
}

// CacheLineHopTracer maps cache lines to (demand access count, hop sum, hop
// event count). Values are stored by value (no pointers) so the map stays
// GC-cheap even at millions of entries.
type CacheLineHopTracer struct {
	lines map[uint64]cacheLineStats
}

func NewCacheLineHopTracer() *CacheLineHopTracer {
	return &CacheLineHopTracer{lines: make(map[uint64]cacheLineStats)}
}

func (t *CacheLineHopTracer) StartTask(task Task) {
	switch task.Kind {
	case "memory-trace":
		t.recordDemand(task)
	case CachelineHopKind:
		t.recordHop(task)
	}
}

func (t *CacheLineHopTracer) StepTask(task Task) {
	// Do nothing
}

func (t *CacheLineHopTracer) EndTask(task Task) {
	// Do nothing
}

// recordDemand counts one demand access (type 0-5, matching the DEMAND_TYPES
// convention in analyze/cacheline/top_hot_line_hop.py) toward its cache
// line's ranking weight. Evict/overwrite rows (type 6-9, Detail =
// *VictimInfo) and PTE-walk reads are excluded.
func (t *CacheLineHopTracer) recordDemand(task Task) {
	if task.ID > 5 {
		return
	}

	req, ok := task.Detail.(protocol.AccessReq)
	if !ok {
		return
	}

	if rr, ok := req.(*protocol.ReadReq); ok && rr.IsPtw {
		return
	}

	line := req.GetAddress() >> cacheLineShift
	s := t.lines[line]
	s.demandCount++
	t.lines[line] = s
}

// recordHop attributes a real simulated hop distance to its cache line, for
// an access that was actually served remotely (RDMA/lookup-chain resolved).
func (t *CacheLineHopTracer) recordHop(task Task) {
	d, ok := task.Detail.(*CachelineHopDetail)
	if !ok {
		return
	}

	line := d.Addr >> cacheLineShift
	s := t.lines[line]
	s.hopSum += d.Hop
	s.hopEventCount++
	t.lines[line] = s
}

type cacheLineEntry struct {
	line uint64
	cacheLineStats
}

// CacheLineHopPercentile is one top-K%-by-demand-count row: hop stats are
// computed per line first (hopSum/hopEventCount), then Max/Min/Avg/Median are
// taken across those per-line averages (lines with zero hop events are
// excluded from the stat fields, though NLinesInCut still counts them).
type CacheLineHopPercentile struct {
	TopPct            int
	NLinesInCut       int
	NLinesWithHopData int
	Max               float64
	Min               float64
	Avg               float64
	Median            float64
}

// PercentileStats ranks cache lines by demand access count descending and
// returns one CacheLineHopPercentile row per cumulative top-5%/10%/.../100%
// cut (20 rows).
func (t *CacheLineHopTracer) PercentileStats() []CacheLineHopPercentile {
	sorted := make([]cacheLineEntry, 0, len(t.lines))
	for line, s := range t.lines {
		sorted = append(sorted, cacheLineEntry{line: line, cacheLineStats: s})
	}
	sort.Slice(sorted, func(i, j int) bool {
		if sorted[i].demandCount != sorted[j].demandCount {
			return sorted[i].demandCount > sorted[j].demandCount
		}
		return sorted[i].line < sorted[j].line
	})

	total := len(sorted)
	rows := make([]CacheLineHopPercentile, 0, 20)
	for pct := 5; pct <= 100; pct += 5 {
		n := total * pct / 100
		if n < 1 && total > 0 {
			n = 1
		}

		avgs := make([]float64, 0, n)
		for _, e := range sorted[:n] {
			if e.hopEventCount == 0 {
				continue
			}
			avgs = append(avgs, float64(e.hopSum)/float64(e.hopEventCount))
		}

		max, min, avg, median := hopStats(avgs)
		rows = append(rows, CacheLineHopPercentile{
			TopPct:            pct,
			NLinesInCut:       n,
			NLinesWithHopData: len(avgs),
			Max:               max,
			Min:               min,
			Avg:               avg,
			Median:            median,
		})
	}
	return rows
}

// hopStats returns (max, min, avg, median) of vals, or all zeros if empty.
func hopStats(vals []float64) (max, min, avg, median float64) {
	if len(vals) == 0 {
		return 0, 0, 0, 0
	}

	sorted := make([]float64, len(vals))
	copy(sorted, vals)
	sort.Float64s(sorted)

	sum := 0.0
	for _, v := range sorted {
		sum += v
	}

	max = sorted[len(sorted)-1]
	min = sorted[0]
	avg = sum / float64(len(sorted))

	mid := len(sorted) / 2
	if len(sorted)%2 == 0 {
		median = (sorted[mid-1] + sorted[mid]) / 2
	} else {
		median = sorted[mid]
	}

	return max, min, avg, median
}
