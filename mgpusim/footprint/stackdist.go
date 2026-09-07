package footprint

// Per-chiplet exact stack (reuse) distance — Mattson/Olken's algorithm via a
// Fenwick (binary indexed) tree over each chiplet's own local access
// sequence.
//
// Complements the bucketed windowed-WSS in window.go rather than replacing
// it: that gives a coarse, cheap distributional snapshot at a few fixed
// granularities (100/1K/10K/100K ticks). This gives the EXACT answer to
// "what hit rate would a private cache of size C get", for any C, in one
// pass — the stack distance of an access IS the minimum LRU cache size (in
// lines) that would have turned it into a hit.
//
// Scope: chiplet-LOCAL only — each chiplet's own touches, independent of
// every other chiplet. Not a shared/cluster-pool stack: answering "would an
// N-chiplet cluster pool capture this" needs cluster membership, which this
// package doesn't model (see analyze/common/pc_hotness.py's cluster_dims),
// and is future work if wanted.
//
// Tick unit: one per touchLine call, same convention as window.go (a
// TouchN's own count n is a reuse *weight*, not multiple ticks — see
// window.go's doc comment for why).
//
// Cost: O(log U_c) time per touch, where U_c is chiplet c's own tick count
// so far — meaningfully more expensive than the O(1) amortized hash-table
// touch the rest of this package uses. Opt-in via SetStackDistanceThresholds
// for exactly that reason.
//
// Memory: one Fenwick tree per chiplet, sized to that chiplet's own tick
// count (doubling growth, same pattern as Recorder.grow()) — O(ticks), not
// O(unique lines). For a benchmark that never batches through TouchN (e.g.
// mis, which only ever calls Touch) at real sweep scale, this can reach
// single-digit GB across all chiplets combined. A compaction pass that
// periodically re-indexes live lines into a smaller tree would bound this to
// O(unique lines) instead; deliberately not built here — get an exact,
// verifiable implementation first, optimize memory only if it's actually the
// bottleneck in practice.

import "sort"

type chipletStack struct {
	clock    int64
	lastTime map[uint64]int64 // line -> tick of its most recent touch
	tree     []int32          // Fenwick tree, 1-based; tree[0] unused

	// thresholds are line counts (not bytes); hits[i] counts accesses whose
	// stack distance is <= thresholds[i], out of every access including cold
	// misses — the standard MRC convention (a cold miss is a miss at any
	// capacity, so it belongs in the denominator, never the numerator).
	thresholds []int64
	hits       []int64
	accesses   int64
	coldMisses int64
}

func newChipletStack(thresholds []int64) *chipletStack {
	return &chipletStack{
		lastTime:   make(map[uint64]int64),
		tree:       make([]int32, 1<<16),
		thresholds: thresholds,
		hits:       make([]int64, len(thresholds)),
	}
}

// add applies delta at 1-based Fenwick position i.
func (s *chipletStack) add(i int64, delta int32) {
	for ; i < int64(len(s.tree)); i += i & (-i) {
		s.tree[i] += delta
	}
}

// sum returns the prefix sum over positions [1, i].
func (s *chipletStack) sum(i int64) int64 {
	var total int64
	for ; i > 0; i -= i & (-i) {
		total += int64(s.tree[i])
	}

	return total
}

// grow reallocates the tree to fit at least `need` and replays every
// currently-live line's marker into it. Only the most recent touch of each
// line is ever live (an earlier marker is removed — see touch), so this is
// exactly one add() per lastTime entry, not one per historical access.
func (s *chipletStack) grow(need int64) {
	newCap := int64(len(s.tree))
	for newCap <= need {
		newCap *= 2
	}

	s.tree = make([]int32, newCap)
	for _, t := range s.lastTime {
		s.add(t+1, 1)
	}
}

// touch records one access to line by this chiplet and buckets it into the
// hit/miss counters for every configured threshold.
func (s *chipletStack) touch(line uint64) {
	s.accesses++
	pos := s.clock + 1
	if pos >= int64(len(s.tree)) {
		s.grow(pos)
	}

	if prevTick, ok := s.lastTime[line]; ok {
		prevPos := prevTick + 1
		// Distinct lines touched strictly after line's last touch and before
		// now = (everything currently live) minus (live at-or-before prevPos).
		// line's own marker is still at prevPos at this point (not yet moved),
		// so it falls in the "at-or-before" term and is correctly excluded.
		dist := s.sum(int64(len(s.tree)-1)) - s.sum(prevPos)

		for i, th := range s.thresholds {
			if dist <= th {
				s.hits[i]++
			}
		}

		s.add(prevPos, -1)
	} else {
		s.coldMisses++
	}

	s.add(pos, 1)
	s.lastTime[line] = s.clock
	s.clock++
}

// ChipletStackDistanceStat is one chiplet's exact stack-distance summary.
type ChipletStackDistanceStat struct {
	Chiplet    int   `json:"chiplet"`
	Accesses   int64 `json:"accesses"`
	ColdMisses int64 `json:"cold_misses"`
	// HitsAtThreshold[i] / HitRateAtThreshold[i] line up 1:1 with the
	// thresholds passed to SetStackDistanceThresholds, in the same order.
	Thresholds      []int64   `json:"thresholds_lines"`
	HitsAtThreshold []int64   `json:"hits_at_threshold"`
	HitRate         []float64 `json:"hit_rate_at_threshold"`
}

// SetStackDistanceThresholds enables exact per-chiplet stack-distance
// tracking, bucketed at the given capacities (in lines, not bytes — e.g. 4MB
// = 65536 at LineBytes=64). Must be called before any Touch/TouchN/
// TouchRange/TouchRangeN. Thresholds need not be sorted; sorted ascending
// internally so StackDistanceStats reports them in a stable, readable order.
func (r *Recorder) SetStackDistanceThresholds(thresholdsLines []int64) {
	sorted := append([]int64(nil), thresholdsLines...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })

	r.stacks = make([]*chipletStack, r.numChiplets)
	for c := range r.stacks {
		r.stacks[c] = newChipletStack(sorted)
	}
}

// StackDistanceResult is SetStackDistanceThresholds' reportable output:
// Summary first (one aggregate row per threshold, across all chiplets — read
// this, not PerChiplet, unless a specific chiplet's own number is needed),
// then the full per-chiplet breakdown.
type StackDistanceResult struct {
	Summary    []ThresholdSummary         `json:"summary"`
	PerChiplet []ChipletStackDistanceStat `json:"per_chiplet"`
}

// StackDistanceStats builds the result of a SetStackDistanceThresholds pass.
// nil if that was never called on this Recorder.
func (r *Recorder) StackDistanceStats() *StackDistanceResult {
	if len(r.stacks) == 0 {
		return nil
	}

	stats := make([]ChipletStackDistanceStat, len(r.stacks))
	for c, s := range r.stacks {
		stats[c] = buildStackDistanceStat(c, s)
	}

	return &StackDistanceResult{
		Summary:    summarizeAcrossChiplets(stats),
		PerChiplet: stats,
	}
}

// ThresholdSummary aggregates one threshold's hit rate across every chiplet
// in a []ChipletStackDistanceStat — the "look here first" metric instead of
// scanning all 36 (or however many) per-chiplet entries by hand.
type ThresholdSummary struct {
	ThresholdLines int64   `json:"threshold_lines"`
	ThresholdMB    float64 `json:"threshold_mb"`
	MeanHitRate    float64 `json:"mean_hit_rate"`
	MedianHitRate  float64 `json:"median_hit_rate"`
	// Min/Max are literally the worst/best individual chiplet at this
	// threshold, named so a reader doesn't have to guess which extreme is
	// "bad" — a low hit rate means that chiplet's working set doesn't fit.
	MinHitRate        float64 `json:"min_hit_rate"`
	MinHitRateChiplet int     `json:"min_hit_rate_chiplet"`
	MaxHitRate        float64 `json:"max_hit_rate"`
	MaxHitRateChiplet int     `json:"max_hit_rate_chiplet"`
}

// summarizeAcrossChiplets builds one ThresholdSummary per threshold index,
// aggregating HitRate[i] across every entry in stats. stats must be
// non-empty and share the same Thresholds slice (true for anything built via
// buildStackDistanceStat from a common SetStackDistanceThresholds call).
func summarizeAcrossChiplets(stats []ChipletStackDistanceStat) []ThresholdSummary {
	if len(stats) == 0 {
		return nil
	}

	numThresholds := len(stats[0].Thresholds)
	out := make([]ThresholdSummary, numThresholds)
	for i := 0; i < numThresholds; i++ {
		rates := make([]float64, len(stats))
		for c, s := range stats {
			rates[c] = s.HitRate[i]
		}
		sorted := append([]float64(nil), rates...)
		sort.Float64s(sorted)

		minRate, minChip := rates[0], stats[0].Chiplet
		maxRate, maxChip := rates[0], stats[0].Chiplet
		var sum float64
		for c, r := range rates {
			sum += r
			if r < minRate {
				minRate, minChip = r, stats[c].Chiplet
			}
			if r > maxRate {
				maxRate, maxChip = r, stats[c].Chiplet
			}
		}

		lines := stats[0].Thresholds[i]
		out[i] = ThresholdSummary{
			ThresholdLines:    lines,
			ThresholdMB:       float64(lines) * LineBytes / (1024 * 1024),
			MeanHitRate:       sum / float64(len(rates)),
			MedianHitRate:     percentile(sorted, 0.5),
			MinHitRate:        minRate,
			MinHitRateChiplet: minChip,
			MaxHitRate:        maxRate,
			MaxHitRateChiplet: maxChip,
		}
	}

	return out
}

// buildStackDistanceStat converts one chipletStack's raw counters into its
// reportable form.
func buildStackDistanceStat(chiplet int, s *chipletStack) ChipletStackDistanceStat {
	rate := make([]float64, len(s.thresholds))
	for i, h := range s.hits {
		if s.accesses > 0 {
			rate[i] = float64(h) / float64(s.accesses)
		}
	}

	return ChipletStackDistanceStat{
		Chiplet:         chiplet,
		Accesses:        s.accesses,
		ColdMisses:      s.coldMisses,
		Thresholds:      s.thresholds,
		HitsAtThreshold: s.hits,
		HitRate:         rate,
	}
}
