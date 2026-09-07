// Package footprint derives the inter-chiplet sharing footprint of a benchmark
// on the host, without running a timing simulation.
//
// Motivation: the memory traces emitted by timing runs are truncated by
// -max-inst, so any shared-footprint measured from them is an underestimate.
// For every benchmark whose kernel addressing can be reproduced on the host
// (see replay.go), we walk the same address stream the GPU would and record,
// per 64B cacheline, which chiplets touch it.
//
// Output is the compact per-line aggregate that
// analyze/cacheline/shared_footprint_capacity_cdf.py consumes, not a
// row-per-access trace: a full trace of an untruncated run would be far larger
// than the 1-37GB truncated ones it is meant to replace.
//
// How the numbers relate to a timing run's memory trace:
//
//   - The set of (line, chiplet) pairs is the same. That trace is emitted at
//     the L2 directory stage, so it only sees L1 misses, but a chiplet's first
//     touch of a line always misses its L1 and every L1 fill comes from L2.
//     Footprint and per-line sharing degree therefore carry over exactly.
//   - The reuse counts are not the same, and are not meant to be. The trace
//     counts post-coalescing L2 requests and misses everything an L1 absorbs;
//     a replay counts what the kernel source issues. Reuse only orders lines
//     in the CDF, so the ranking is what matters, not the absolute counts.
//   - Addresses are virtual here and physical there. Translation is injective
//     and page-aligned, so lines map one-to-one and only the labels differ.
package footprint

import (
	"bufio"
	"encoding/json"
	"fmt"
	"math/bits"
	"os"
	"strconv"
)

// LineBytes is the cacheline size the analysis bins addresses into. Matches
// the simulated L2 block size (log2CacheLineSize = 6).
const LineBytes = 64

const log2LineBytes = 6

// emptyKey marks an unoccupied slot. A line id of 2^64-1 would mean a byte
// address of 0xFFFFFFFFFFFFFFC0, which no benchmark allocates.
const emptyKey = ^uint64(0)

// MaxChiplets is bounded by the uint64 chiplet mask. Wafer-scale meshes in
// this repo are at most 8x8.
const MaxChiplets = 64

// Recorder accumulates, per cacheline, the set of chiplets that accessed it
// and the total access count.
//
// Backed by a purpose-built open-addressing table rather than a Go map: the
// footprints here reach 10^8 lines, where a map[uint64]struct{...} costs
// several times more memory and is markedly slower.
type Recorder struct {
	numChiplets int

	keys   []uint64 // line id, emptyKey when free
	masks  []uint64 // bit c set = chiplet c touched this line
	counts []uint32 // total accesses to this line, saturating

	live     int // occupied slots
	growAt   int // resize threshold (load factor 0.5)
	accesses uint64

	buffers []Buffer

	// windows is empty unless SetWindowSizes was called. See window.go.
	windows []*bucketWindow

	// stacks is empty unless SetStackDistanceThresholds was called. See
	// stackdist.go. Indexed by chiplet.
	stacks []*chipletStack
}

// SetBuffers records the replay's buffer layout in the run summary, so a
// validation pass can restrict a comparison to specific buffers.
func (r *Recorder) SetBuffers(b []Buffer) {
	r.buffers = b
}

// NewRecorder creates a Recorder for a mesh of numChiplets chiplets.
func NewRecorder(numChiplets int) *Recorder {
	if numChiplets <= 0 || numChiplets > MaxChiplets {
		panic(fmt.Sprintf("numChiplets %d out of range [1,%d]",
			numChiplets, MaxChiplets))
	}

	r := &Recorder{numChiplets: numChiplets}
	r.alloc(1 << 20)

	return r
}

func (r *Recorder) alloc(capacity int) {
	r.keys = make([]uint64, capacity)
	for i := range r.keys {
		r.keys[i] = emptyKey
	}
	r.masks = make([]uint64, capacity)
	r.counts = make([]uint32, capacity)
	r.growAt = capacity / 2
}

// hash is splitmix64's finalizer. Line ids are dense and low-entropy, so the
// avalanche matters for linear probing.
func hash(x uint64) uint64 {
	x ^= x >> 30
	x *= 0xbf58476d1ce4e5b9
	x ^= x >> 27
	x *= 0x94d049bb133111eb
	x ^= x >> 31

	return x
}

// Touch records one access to addr by chiplet.
func (r *Recorder) Touch(chiplet int, addr uint64) {
	r.touchLine(chiplet, addr>>log2LineBytes, 1)
}

// TouchN records n accesses to addr by chiplet in one call.
//
// Needed by the dense benchmarks: a GEMM at the configured sizes issues ~10^13
// accesses, far too many to walk one at a time, but its access count per
// cacheline is a closed form. Replays that can compute that count directly
// must use this instead of looping.
func (r *Recorder) TouchN(chiplet int, addr, n uint64) {
	if n == 0 {
		return
	}

	r.touchLine(chiplet, addr>>log2LineBytes, n)
}

// TouchRangeN records n accesses to every cacheline in [addr, addr+size).
func (r *Recorder) TouchRangeN(chiplet int, addr, size, n uint64) {
	if size == 0 || n == 0 {
		return
	}

	first := addr >> log2LineBytes
	last := (addr + size - 1) >> log2LineBytes
	for line := first; line <= last; line++ {
		r.touchLine(chiplet, line, n)
	}
}

// TouchRange records an access of size bytes starting at addr, splitting it
// across every cacheline it spans — a request straddling a line boundary
// reaches the L2 as two accesses.
func (r *Recorder) TouchRange(chiplet int, addr, size uint64) {
	if size == 0 {
		return
	}

	first := addr >> log2LineBytes
	last := (addr + size - 1) >> log2LineBytes
	for line := first; line <= last; line++ {
		r.touchLine(chiplet, line, 1)
	}
}

// addSat adds b to a, clamping at the uint32 ceiling. Counts only order lines
// in the CDF, so saturating a pathologically hot line is harmless.
func addSat(a uint32, b uint64) uint32 {
	sum := uint64(a) + b
	if sum > uint64(^uint32(0)) {
		return ^uint32(0)
	}

	return uint32(sum)
}

func (r *Recorder) touchLine(chiplet int, line, n uint64) {
	if uint(chiplet) >= uint(r.numChiplets) {
		panic(fmt.Sprintf("chiplet %d out of range [0,%d)",
			chiplet, r.numChiplets))
	}

	for _, w := range r.windows {
		w.touch(chiplet, line)
	}
	if len(r.stacks) > 0 {
		r.stacks[chiplet].touch(line)
	}

	r.accesses += n

	mask := uint64(1) << uint(chiplet)
	slot := hash(line) & uint64(len(r.keys)-1)
	for {
		k := r.keys[slot]
		if k == line {
			r.masks[slot] |= mask
			r.counts[slot] = addSat(r.counts[slot], n)

			return
		}

		if k == emptyKey {
			r.keys[slot] = line
			r.masks[slot] = mask
			r.counts[slot] = addSat(0, n)
			r.live++
			if r.live >= r.growAt {
				r.grow()
			}

			return
		}

		slot = (slot + 1) & uint64(len(r.keys)-1)
	}
}

func (r *Recorder) grow() {
	oldKeys, oldMasks, oldCounts := r.keys, r.masks, r.counts
	r.alloc(len(oldKeys) * 2)
	r.live = 0

	for i, k := range oldKeys {
		if k == emptyKey {
			continue
		}

		slot := hash(k) & uint64(len(r.keys)-1)
		for r.keys[slot] != emptyKey {
			slot = (slot + 1) & uint64(len(r.keys)-1)
		}
		r.keys[slot] = k
		r.masks[slot] = oldMasks[i]
		r.counts[slot] = oldCounts[i]
		r.live++
	}
}

// Summary is the run-level aggregate written alongside the per-line CSV.
type Summary struct {
	NumChiplets    int     `json:"num_chiplets"`
	TotalAccesses  uint64  `json:"total_accesses"`
	TotalLines     int     `json:"total_lines"`
	TotalMB        float64 `json:"total_mb"`
	SharedLines    int     `json:"shared_lines"`
	SharedMB       float64 `json:"shared_mb"`
	SharedAccesses uint64  `json:"shared_accesses"`
	// LinesPerNChip[k] = lines touched by exactly k distinct chiplets.
	LinesPerNChip []int `json:"lines_per_nchip"`

	// PrivateLines/PrivateMB is LinesPerNChip[1] promoted to a first-class
	// field: lines touched by exactly one chiplet, i.e. never leave that
	// chiplet's own L2 slice. This is the complement of the shared-footprint
	// figures this package feeds — the capacity a chiplet needs regardless of
	// any cluster pooling.
	PrivateLines int     `json:"private_lines"`
	PrivateMB    float64 `json:"private_mb"`
	// PrivateLinesPerChiplet[c] / PrivateMBPerChiplet[c] = private lines whose
	// sole owner is chiplet c. Per-chiplet, not just the aggregate, because an
	// even total can still hide one chiplet holding most of the private
	// working set (e.g. a row-major partition's edge chiplet).
	PrivateLinesPerChiplet []int     `json:"private_lines_per_chiplet"`
	PrivateMBPerChiplet    []float64 `json:"private_mb_per_chiplet"`

	// SharedLinesPerChiplet[c] / SharedMBPerChiplet[c] = shared lines (>=2
	// distinct chiplets) that chiplet c is one of the touchers of. Unlike
	// PrivateLinesPerChiplet this isn't a partition — one shared line counts
	// toward every chiplet that touches it — but it answers the same
	// per-chiplet question private does: of what chiplet c itself ever
	// touches, how much of that volume is shared with someone else.
	SharedLinesPerChiplet []int     `json:"shared_lines_per_chiplet"`
	SharedMBPerChiplet    []float64 `json:"shared_mb_per_chiplet"`

	// ChipletCapacity is the cross-chiplet distribution of
	// Shared/PrivateMBPerChiplet above — the "look here first" single number
	// instead of scanning NumChiplets entries by hand.
	ChipletCapacity ChipletCapacitySummary `json:"chiplet_capacity"`

	// Windows is the windowed-WSS distribution per size passed to
	// SetWindowSizes (see window.go) — empty unless that was called. Answers
	// "what does a chiplet actually need to hold at once", as opposed to
	// TotalMB/SharedMB/PrivateMB above, which are lifetime unions and
	// overstate it for any benchmark whose reuse is sparse relative to its
	// footprint (e.g. GUPS).
	Windows []WindowStat `json:"windows,omitempty"`

	// StackDistance is the exact reuse-distance hit rate at each configured
	// capacity, per SetStackDistanceThresholds (see stackdist.go) — nil
	// unless that was called. Its Summary is cross-chiplet aggregated (read
	// that first); PerChiplet has the full per-chiplet breakdown. This is
	// chiplet-local: each chiplet's own full reference stream, shared and
	// private lines mixed together as it actually sees them — it gives the
	// exact "what hit rate would a private cache of size C get" answer for
	// whatever thresholds were asked for.
	StackDistance *StackDistanceResult `json:"stack_distance,omitempty"`

	Buffers []Buffer `json:"buffers,omitempty"`
}

// ChipletCapacitySummary is the cross-chiplet mean/median/max of
// Shared/PrivateMBPerChiplet — see Summary.ChipletCapacity.
type ChipletCapacitySummary struct {
	SharedMeanMB   float64 `json:"shared_mean_mb"`
	SharedMedianMB float64 `json:"shared_median_mb"`
	SharedMaxMB    float64 `json:"shared_max_mb"`

	PrivateMeanMB   float64 `json:"private_mean_mb"`
	PrivateMedianMB float64 `json:"private_median_mb"`
	PrivateMaxMB    float64 `json:"private_max_mb"`
}

// Summarize computes the run-level aggregate.
func (r *Recorder) Summarize() Summary {
	s := Summary{
		NumChiplets:            r.numChiplets,
		TotalAccesses:          r.accesses,
		TotalLines:             r.live,
		LinesPerNChip:          make([]int, r.numChiplets+1),
		PrivateLinesPerChiplet: make([]int, r.numChiplets),
		PrivateMBPerChiplet:    make([]float64, r.numChiplets),
		SharedLinesPerChiplet:  make([]int, r.numChiplets),
		SharedMBPerChiplet:     make([]float64, r.numChiplets),
		Buffers:                r.buffers,
	}

	for i, k := range r.keys {
		if k == emptyKey {
			continue
		}

		mask := r.masks[i]
		n := bits.OnesCount64(mask)
		s.LinesPerNChip[n]++
		switch {
		case n == 1:
			s.PrivateLinesPerChiplet[bits.TrailingZeros64(mask)]++
		case n >= 2:
			s.SharedLines++
			s.SharedAccesses += uint64(r.counts[i])
			for m := mask; m != 0; m &= m - 1 {
				s.SharedLinesPerChiplet[bits.TrailingZeros64(m)]++
			}
		}
	}

	const mb = 1024 * 1024
	s.TotalMB = float64(s.TotalLines) * LineBytes / mb
	s.SharedMB = float64(s.SharedLines) * LineBytes / mb

	s.PrivateLines = s.LinesPerNChip[1]
	s.PrivateMB = float64(s.PrivateLines) * LineBytes / mb
	for c, n := range s.PrivateLinesPerChiplet {
		s.PrivateMBPerChiplet[c] = float64(n) * LineBytes / mb
	}
	for c, n := range s.SharedLinesPerChiplet {
		s.SharedMBPerChiplet[c] = float64(n) * LineBytes / mb
	}

	sMean, sMedian, _, sMax := distStats(s.SharedMBPerChiplet)
	pMean, pMedian, _, pMax := distStats(s.PrivateMBPerChiplet)
	s.ChipletCapacity = ChipletCapacitySummary{
		SharedMeanMB:    sMean,
		SharedMedianMB:  sMedian,
		SharedMaxMB:     sMax,
		PrivateMeanMB:   pMean,
		PrivateMedianMB: pMedian,
		PrivateMaxMB:    pMax,
	}

	s.Windows = r.WindowStats()
	s.StackDistance = r.StackDistanceStats()

	return s
}

// WriteSharedCSV writes one row per shared line (>=2 distinct chiplets), the
// exact input analyze/cacheline/shared_footprint_capacity_cdf.py aggregates to.
//
// Columns: line (byte address >> 6), reuse (total accesses), nchip (distinct
// chiplets).
func (r *Recorder) WriteSharedCSV(path string) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()

	w := bufio.NewWriterSize(f, 1<<20)
	if _, err := w.WriteString("line,reuse,nchip\n"); err != nil {
		return err
	}

	buf := make([]byte, 0, 64)
	for i, k := range r.keys {
		if k == emptyKey {
			continue
		}

		n := bits.OnesCount64(r.masks[i])
		if n < 2 {
			continue
		}

		buf = buf[:0]
		buf = strconv.AppendUint(buf, k, 10)
		buf = append(buf, ',')
		buf = strconv.AppendUint(buf, uint64(r.counts[i]), 10)
		buf = append(buf, ',')
		buf = strconv.AppendInt(buf, int64(n), 10)
		buf = append(buf, '\n')
		if _, err := w.Write(buf); err != nil {
			return err
		}
	}

	return w.Flush()
}

// WriteSummaryJSON writes the run-level aggregate.
func (r *Recorder) WriteSummaryJSON(path string) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()

	enc := json.NewEncoder(f)
	enc.SetIndent("", "  ")

	return enc.Encode(r.Summarize())
}
