package footprint

import (
	"fmt"
	"math/bits"
	"sort"
)

// Windowed working-set size, as an alternative to the lifetime footprint the
// rest of this package computes.
//
// Lifetime footprint (Recorder.Summarize) is a union over the whole run: a
// line counts once no matter how sparsely it's revisited. For a capacity
// question ("does this fit in N MB of cache") that overstates what actually
// needs to be resident at any one time — e.g. GUPS's footprint is its whole
// table, even though only a sliver of it is "hot" at once.
//
// This computes Denning working-set size, bucketed rather than sliding: the
// access stream is cut into consecutive, non-overlapping windows of a fixed
// tick count, and each window's distinct-line count (split shared/private) is
// recorded as one sample. A sliding window (evicting the oldest access as
// each new one arrives) would be the textbook definition and is strictly more
// accurate, but costs an eviction-tracking structure touched on every access;
// bucketing costs one map write per access and no eviction bookkeeping, at
// the price of not seeing a working set that straddles a bucket boundary.
// Running several window sizes at once (100/1K/10K/100K by default) covers
// most of that gap — a transient invisible to a 100K bucket typically shows
// up whole in a 100-tick one.
//
// Window unit: one tick per touchLine call, i.e. once per (chiplet, line)
// touch — the same granularity as a demand event in a real memory trace, and
// the same convention analyze/cacheline/hot_line_eviction_analysis.py's
// per-chiplet access-count "clock" uses (cycle counts aren't comparable
// across benchmarks of different scale; access counts are). TouchN's own
// count n is a per-line reuse *weight*, not a tick multiplier — one TouchN
// call is one tick regardless of n, matching how a replay's closed-form dense
// accesses (see TouchN's doc comment) stand in for what would otherwise be
// many separate touchLine calls on the timing side.
type bucketWindow struct {
	size        int64
	tick        int64
	numChiplets int
	// line -> chiplet mask, this bucket only; reset every flush.
	lines map[uint64]uint64

	sharedMB  []float64
	privateMB []float64
	// privateMBPerChiplet[c] is chiplet c's private-line MB, one sample per
	// bucket — every bucket appends to every chiplet's series (0 if that
	// chiplet had no private lines that bucket), so all series stay aligned
	// 1:1 with sharedMB/privateMB by index.
	//
	// Memory note: this is numChiplets separate series, not one, so it scales
	// with numChiplets x bucket-count. For the smallest window size on a
	// heavy Touch-by-Touch replay (mis at real sweep scale: ~1.28e9 ticks),
	// window=100 is ~1.28e7 buckets x 36 chiplets x 8B = ~3.7 GB. Acceptable
	// on this workspace's hardware today; if it isn't on some future box, the
	// fix is a streaming quantile (e.g. P²) per chiplet instead of raw
	// samples, trading exactness for O(1) memory.
	privateMBPerChiplet [][]float64
}

func newBucketWindow(size int64, numChiplets int) *bucketWindow {
	return &bucketWindow{
		size:                size,
		numChiplets:         numChiplets,
		lines:               make(map[uint64]uint64, size),
		privateMBPerChiplet: make([][]float64, numChiplets),
	}
}

func (w *bucketWindow) touch(chiplet int, line uint64) {
	w.lines[line] |= uint64(1) << uint(chiplet)
	w.tick++
	if w.tick >= w.size {
		w.flush()
	}
}

// flush closes out the current bucket into one sample per series and starts
// the next.
func (w *bucketWindow) flush() {
	if len(w.lines) > 0 {
		privateCount := make([]int, w.numChiplets)
		var shared, private int
		for _, mask := range w.lines {
			if bits.OnesCount64(mask) >= 2 {
				shared++
				continue
			}
			private++
			privateCount[bits.TrailingZeros64(mask)]++
		}
		const mb = 1024 * 1024
		w.sharedMB = append(w.sharedMB, float64(shared)*LineBytes/mb)
		w.privateMB = append(w.privateMB, float64(private)*LineBytes/mb)
		for c, n := range privateCount {
			w.privateMBPerChiplet[c] = append(w.privateMBPerChiplet[c], float64(n)*LineBytes/mb)
		}
	}

	w.lines = make(map[uint64]uint64, w.size)
	w.tick = 0
}

// ChipletStat is a distribution summary of one chiplet's private-MB series
// across a window size's buckets.
type ChipletStat struct {
	Chiplet  int     `json:"chiplet"`
	MeanMB   float64 `json:"mean_mb"`
	MedianMB float64 `json:"median_mb"`
	P95MB    float64 `json:"p95_mb"`
	MaxMB    float64 `json:"max_mb"`
}

// WindowStat is a distribution summary over one window size's bucket series.
type WindowStat struct {
	WindowSize int64 `json:"window_size"`
	// NumWindows is the count of *full* buckets the stats below are over. A
	// trailing partial bucket (fewer than WindowSize ticks left at the end of
	// the run) is dropped rather than flushed: its distinct-line count isn't
	// comparable to a full bucket's and would bias the low end of every
	// percentile below.
	NumWindows int `json:"num_windows"`

	SharedMeanMB   float64 `json:"shared_mean_mb"`
	SharedMedianMB float64 `json:"shared_median_mb"`
	SharedP95MB    float64 `json:"shared_p95_mb"`
	SharedMaxMB    float64 `json:"shared_max_mb"`

	// Private*MB is the cross-chiplet aggregate (every chiplet's private
	// lines summed before taking the distribution). PrivateByChiplet below is
	// the same data split per chiplet — the aggregate alone can hide one
	// chiplet holding most of the private working set while the total looks
	// modest (see PrivateLinesPerChiplet's doc comment on the lifetime side).
	PrivateMeanMB   float64 `json:"private_mean_mb"`
	PrivateMedianMB float64 `json:"private_median_mb"`
	PrivateP95MB    float64 `json:"private_p95_mb"`
	PrivateMaxMB    float64 `json:"private_max_mb"`

	// PrivateByChiplet[c] is chiplet c's own private-MB distribution over
	// this window size's buckets. No shared-by-chiplet equivalent: a shared
	// line by definition has no single owning chiplet, so there's no
	// non-arbitrary way to attribute it to one.
	PrivateByChiplet []ChipletStat `json:"private_by_chiplet"`
}

func percentile(sorted []float64, p float64) float64 {
	if len(sorted) == 0 {
		return 0
	}
	idx := int(p * float64(len(sorted)-1))

	return sorted[idx]
}

func mean(vals []float64) float64 {
	if len(vals) == 0 {
		return 0
	}
	var sum float64
	for _, v := range vals {
		sum += v
	}

	return sum / float64(len(vals))
}

// distStats is mean/median/p95/max over one sample series. Shared by the
// aggregate and per-chiplet stats below so there's one sort+percentile path.
func distStats(vals []float64) (meanV, medianV, p95V, maxV float64) {
	sorted := append([]float64(nil), vals...)
	sort.Float64s(sorted)

	return mean(vals), percentile(sorted, 0.50), percentile(sorted, 0.95), percentile(sorted, 1.00)
}

// finalize summarizes the closed buckets so far. The trailing partial bucket
// (w.tick > 0 but < w.size) is intentionally left unflushed — see NumWindows.
func (w *bucketWindow) finalize() WindowStat {
	sMean, sMedian, sP95, sMax := distStats(w.sharedMB)
	pMean, pMedian, pP95, pMax := distStats(w.privateMB)

	byChiplet := make([]ChipletStat, w.numChiplets)
	for c := 0; c < w.numChiplets; c++ {
		cMean, cMedian, cP95, cMax := distStats(w.privateMBPerChiplet[c])
		byChiplet[c] = ChipletStat{
			Chiplet: c, MeanMB: cMean, MedianMB: cMedian, P95MB: cP95, MaxMB: cMax,
		}
	}

	return WindowStat{
		WindowSize: w.size,
		NumWindows: len(w.sharedMB),

		SharedMeanMB:   sMean,
		SharedMedianMB: sMedian,
		SharedP95MB:    sP95,
		SharedMaxMB:    sMax,

		PrivateMeanMB:   pMean,
		PrivateMedianMB: pMedian,
		PrivateP95MB:    pP95,
		PrivateMaxMB:    pMax,

		PrivateByChiplet: byChiplet,
	}
}

// SetWindowSizes enables windowed-WSS tracking for the given tick counts.
// Must be called before any Touch/TouchN/TouchRange/TouchRangeN. Sizes <= 0
// are rejected by panicking — a silently-ignored bad size would just look
// like "no windowed stats" in the output with no indication why.
func (r *Recorder) SetWindowSizes(sizes []int64) {
	r.windows = make([]*bucketWindow, 0, len(sizes))
	for _, s := range sizes {
		if s <= 0 {
			panic(fmt.Sprintf("window size must be > 0, got %d", s))
		}
		r.windows = append(r.windows, newBucketWindow(s, r.numChiplets))
	}
}

// WindowStats finalizes and returns one WindowStat per size passed to
// SetWindowSizes, in the same order. Empty if SetWindowSizes was never
// called.
func (r *Recorder) WindowStats() []WindowStat {
	stats := make([]WindowStat, len(r.windows))
	for i, w := range r.windows {
		stats[i] = w.finalize()
	}

	return stats
}
