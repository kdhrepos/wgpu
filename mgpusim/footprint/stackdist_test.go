package footprint

import (
	"math/rand"
	"testing"
)

// naiveStackDistances computes exact stack distance the O(n^2) way, as an
// independent reference for the Fenwick-tree implementation to be checked
// against. -1 marks a cold miss (first-ever touch).
func naiveStackDistances(seq []uint64) []int64 {
	out := make([]int64, len(seq))
	for i, line := range seq {
		last := -1
		for j := i - 1; j >= 0; j-- {
			if seq[j] == line {
				last = j

				break
			}
		}
		if last == -1 {
			out[i] = -1

			continue
		}
		seen := map[uint64]bool{}
		for j := last + 1; j < i; j++ {
			seen[seq[j]] = true
		}
		out[i] = int64(len(seen))
	}

	return out
}

// runSequence feeds seq through a fresh chipletStack with a single very
// large threshold (so every finite distance is a "hit"), and returns, per
// access, the hit bit and whether it was a cold miss — from which the exact
// distance bucket boundary (hit iff dist <= threshold) can be checked against
// naiveStackDistances by re-running per threshold.
func hitsAtThreshold(t *testing.T, seq []uint64, threshold int64) []bool {
	t.Helper()
	s := newChipletStack([]int64{threshold})
	out := make([]bool, len(seq))
	for i, line := range seq {
		before := s.hits[0]
		s.touch(line)
		out[i] = s.hits[0] > before
	}

	return out
}

func TestStackDistance_HandVerified_NoRepeatWithinWindow(t *testing.T) {
	// A B C A B C — each of the second A/B/C has exactly 2 distinct lines
	// (the other two letters) between it and its previous occurrence.
	seq := []uint64{1, 2, 3, 1, 2, 3}

	s := newChipletStack([]int64{1, 2, 3})
	for _, line := range seq {
		s.touch(line)
	}

	if s.accesses != 6 {
		t.Fatalf("accesses = %d, want 6", s.accesses)
	}
	if s.coldMisses != 3 {
		t.Fatalf("coldMisses = %d, want 3", s.coldMisses)
	}
	// threshold=1: 0 hits (all finite distances are 2)
	// threshold=2: 3 hits
	// threshold=3: 3 hits
	want := []int64{0, 3, 3}
	for i, w := range want {
		if s.hits[i] != w {
			t.Errorf("hits[threshold=%d] = %d, want %d",
				s.thresholds[i], s.hits[i], w)
		}
	}
}

func TestStackDistance_HandVerified_ImmediateRepeat(t *testing.T) {
	// A A B A — the immediate A-A repeat has distance 0; the third A has
	// exactly one distinct line (B) since its previous touch.
	seq := []uint64{1, 1, 2, 1}

	s := newChipletStack([]int64{0, 1})
	for _, line := range seq {
		s.touch(line)
	}

	if s.accesses != 4 {
		t.Fatalf("accesses = %d, want 4", s.accesses)
	}
	if s.coldMisses != 2 {
		t.Fatalf("coldMisses = %d, want 2", s.coldMisses)
	}
	want := []int64{1, 2} // threshold=0 -> 1 hit, threshold=1 -> 2 hits
	for i, w := range want {
		if s.hits[i] != w {
			t.Errorf("hits[threshold=%d] = %d, want %d",
				s.thresholds[i], s.hits[i], w)
		}
	}
}

// TestStackDistance_BruteForceCrossCheck runs random access sequences (small
// alphabet so repeats are frequent, exercising the remove/re-add path hard)
// through both the Fenwick implementation and the O(n^2) reference, at
// several thresholds including ones that force tree growth, and requires an
// exact match on every single access — not just the aggregate counts.
func TestStackDistance_BruteForceCrossCheck(t *testing.T) {
	rng := rand.New(rand.NewSource(1))

	for trial := 0; trial < 20; trial++ {
		n := 50 + rng.Intn(500)
		alphabet := 1 + rng.Intn(30)
		seq := make([]uint64, n)
		for i := range seq {
			seq[i] = uint64(rng.Intn(alphabet))
		}

		want := naiveStackDistances(seq)

		for _, threshold := range []int64{0, 1, 2, 5, 100} {
			got := hitsAtThreshold(t, seq, threshold)
			for i := range seq {
				wantHit := want[i] >= 0 && want[i] <= threshold
				if got[i] != wantHit {
					t.Fatalf("trial %d threshold %d: access %d (line %d): "+
						"got hit=%v, want hit=%v (naive dist=%d)",
						trial, threshold, i, seq[i], got[i], wantHit, want[i])
				}
			}
		}
	}
}

// TestStackDistance_ForcesTreeGrowth uses a small initial capacity's worth
// of accesses plus more, on a single line touched sparsely, to make sure
// grow()'s live-marker replay doesn't corrupt state across a resize.
func TestStackDistance_ForcesTreeGrowth(t *testing.T) {
	const n = 1 << 18 // several multiples of the 1<<16 initial tree size
	seq := make([]uint64, n)
	for i := range seq {
		// Two lines, alternating with a long non-repeating filler in between,
		// so growth happens while a real reuse is still "in flight".
		if i%1000 == 0 {
			seq[i] = 999999
		} else {
			seq[i] = uint64(i) // unique filler, never repeats
		}
	}

	s := newChipletStack([]int64{500, 1000})
	for _, line := range seq {
		s.touch(line)
	}

	// line 999999 repeats every 1000 accesses with exactly 999 distinct
	// fillers in between each time (the very first occurrence is a cold
	// miss; every later one has distance 999). n is not itself a multiple of
	// 1000, so this must round up, not truncate — n/1000 undercounts by one.
	wantOccurrences := (n + 999) / 1000
	wantFinite := wantOccurrences - 1 // first occurrence is the cold miss

	if s.hits[0] != 0 { // threshold=500 < 999, none of the repeats should hit
		t.Errorf("hits[threshold=500] = %d, want 0", s.hits[0])
	}
	if s.hits[1] != int64(wantFinite) { // threshold=1000 > 999, all repeats hit
		t.Errorf("hits[threshold=1000] = %d, want %d", s.hits[1], wantFinite)
	}
}

// TestSummarizeAcrossChiplets checks the cross-chiplet aggregate (mean,
// median, min/max + which chiplet) against hand-computed values for a small,
// known set of per-chiplet hit rates at two thresholds.
func TestSummarizeAcrossChiplets(t *testing.T) {
	thresholds := []int64{100, 200}
	stats := []ChipletStackDistanceStat{
		{Chiplet: 0, Thresholds: thresholds, HitRate: []float64{0.5, 0.9}},
		{Chiplet: 1, Thresholds: thresholds, HitRate: []float64{0.2, 0.6}},
		{Chiplet: 2, Thresholds: thresholds, HitRate: []float64{0.8, 0.3}},
	}

	got := summarizeAcrossChiplets(stats)
	if len(got) != 2 {
		t.Fatalf("len(summary) = %d, want 2", len(got))
	}

	const eps = 1e-9

	// threshold[0]=100: rates [0.5, 0.2, 0.8] -> mean 0.5, median 0.5,
	// min 0.2 @ chiplet1, max 0.8 @ chiplet2.
	t0 := got[0]
	if t0.ThresholdLines != 100 {
		t.Errorf("t0.ThresholdLines = %d, want 100", t0.ThresholdLines)
	}
	if diff := t0.MeanHitRate - 0.5; diff > eps || diff < -eps {
		t.Errorf("t0.MeanHitRate = %v, want 0.5", t0.MeanHitRate)
	}
	if diff := t0.MedianHitRate - 0.5; diff > eps || diff < -eps {
		t.Errorf("t0.MedianHitRate = %v, want 0.5", t0.MedianHitRate)
	}
	if t0.MinHitRate != 0.2 || t0.MinHitRateChiplet != 1 {
		t.Errorf("t0 min = %v @ chiplet%d, want 0.2 @ chiplet1",
			t0.MinHitRate, t0.MinHitRateChiplet)
	}
	if t0.MaxHitRate != 0.8 || t0.MaxHitRateChiplet != 2 {
		t.Errorf("t0 max = %v @ chiplet%d, want 0.8 @ chiplet2",
			t0.MaxHitRate, t0.MaxHitRateChiplet)
	}

	// threshold[1]=200: rates [0.9, 0.6, 0.3] -> mean 0.6, median 0.6,
	// min 0.3 @ chiplet2, max 0.9 @ chiplet0.
	t1 := got[1]
	if diff := t1.MeanHitRate - 0.6; diff > eps || diff < -eps {
		t.Errorf("t1.MeanHitRate = %v, want 0.6", t1.MeanHitRate)
	}
	if diff := t1.MedianHitRate - 0.6; diff > eps || diff < -eps {
		t.Errorf("t1.MedianHitRate = %v, want 0.6", t1.MedianHitRate)
	}
	if t1.MinHitRate != 0.3 || t1.MinHitRateChiplet != 2 {
		t.Errorf("t1 min = %v @ chiplet%d, want 0.3 @ chiplet2",
			t1.MinHitRate, t1.MinHitRateChiplet)
	}
	if t1.MaxHitRate != 0.9 || t1.MaxHitRateChiplet != 0 {
		t.Errorf("t1 max = %v @ chiplet%d, want 0.9 @ chiplet0",
			t1.MaxHitRate, t1.MaxHitRateChiplet)
	}
}
