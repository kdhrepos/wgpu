package rdma

import (
	"math/bits"
	"sort"

	"gitlab.com/akita/protocol"
	"gitlab.com/akita/util/global"
)

// ClassifierMode selects the classification algorithm used by classify().
type ClassifierMode int

const (
	// ClassifierThreshold is the original decision tree on
	// (reuse, sharing).  Mirrors the oracle JSON generation script.
	ClassifierThreshold ClassifierMode = iota
	// ClassifierScore uses a weighted sum of normalized
	// (reuse, sharing, hop) with thresholds on the resulting score.
	// Helps during warm-up because hop count is meaningful even with
	// few samples.
	ClassifierScore
)

// PCCounter tracks per-PC statistics for online hotness classification.
//
// Two parallel representations are kept side-by-side:
//
//   - Production path (fixed memory): two HyperLogLog sketches.
//     UniqueAddrs estimates the distinct cacheline-address cardinality, and
//     UniqueAddrChip estimates the distinct (addr, chipletId) tuple
//     cardinality.  Their ratio is the per-line average chiplet sharing
//     used by classify().
//
//   - Ground truth (validation only): GTAddrs maps each address to a
//     bitmap of the requestor chiplets that have asked for it.  Always
//     populated so the reporter can emit HLL-vs-truth comparisons.  Not
//     part of the hardware footprint — production hardware would carry
//     the HLLs only.
type PCCounter struct {
	pc uint64

	// Production-path sketches.
	UniqueAddrs    *HLLUnit // HLL_1: distinct addr estimator (lifetime).
	UniqueAddrChip *HLLUnit // HLL_2: distinct (addr, chiplet) estimator.

	// WSAddrs is a distinct-addr HLL that is periodically rank-decayed
	// (DecayRanks) so it estimates the *windowed* working set rather than
	// the lifetime cardinality. Only the footprint clamp consumes it;
	// reuse/sharing keep using the lifetime UniqueAddrs so tier intensity
	// is unaffected. See PCTable.Decay / classify.
	WSAddrs *HLLUnit

	// CachedHotness is the classification decided at the last periodic
	// reclassify (PCTable.Decay). Runtime queries (ReqFromInside /
	// RspFromInside) read this instead of calling classify() per request —
	// HLL Estimate() every request would be prohibitively expensive in HW.
	CachedHotness protocol.Hotness

	// Ground truth — addr -> chiplet bitmap.  popcount of a value is the
	// real number of distinct chiplets that have touched that address.
	// len(GTAddrs) is the real unique-address count.
	GTAddrs map[uint64]uint64

	// ReqCount is the total number of external requests observed for
	// this PC at this owner.
	ReqCount uint64

	// HopCount is the cumulative hop distance of observed requests, kept
	// for parity with OraclePCCounter.
	HopCount uint64

	// lastHotnessRank is the hotnessRank() of the most recently observed
	// classification for this PC, used to detect a hotness increase.
	// Starts at 0 (Cold rank).
	lastHotnessRank int

	// FirstIncreaseTime is the simulation time (now) at which this PC's
	// classified hotness first increased above its starting (Cold) tier.
	// HasIncreased guards it so only the first increase is stamped.
	FirstIncreaseTime float64
	HasIncreased      bool
}

// PCCounterValidation is the per-PC HLL-vs-ground-truth snapshot used by
// the reporter to dump accuracy data alongside the regular stats columns.
type PCCounterValidation struct {
	PC       uint64
	ReqCount uint64
	HopCount uint64

	// HLL estimates.
	HLLUnique       uint64
	HLLTuples       uint64
	HLLSharingAvg   float64
	HLLAvgCacheline float64 // ReqCount / HLLUnique (HLL-estimated reuse)

	// Ground truth.
	GTUnique     uint64
	GTTuples     uint64
	GTSharingAvg float64
	GTSharingMin int
	GTSharingMax int
	GTSharingP50 int
	GTSharingP90 int
	GTSharingP99 int

	// Hotness as classified from this counter (local view — does not
	// include the piggyback RemoteHotness merge).  Reporters typically
	// surface this as a numeric column.
	Hotness protocol.Hotness
}

// remoteHotnessEntry is a single piggyback cache slot — the most recent
// home-classified hotness seen for a PC, plus its LRU tick.
type remoteHotnessEntry struct {
	Hotness        protocol.Hotness
	LastAccessTick uint64
}

// PCTable performs online classification of PC hotness based on observed
// reuse intensity and sharing breadth.
//
// Storage is bounded to pcTableEntryLimit entries via lowest-reuse
// eviction (smallest avg accesses per cacheline) so the
// table fits in a small SRAM budget.  Per-entry memory is dominated by
// two ~88-byte HLL sketches; production hardware would not include the
// GTAddrs map (validation only).
//
// The table additionally maintains a small per-requestor "remote hotness
// cache" populated by MergeRemoteHotness from response piggyback.  This
// closes the gap between the requestor's local view (only PCs whose
// addresses are homed to this chiplet) and the global picture, by
// recording the highest hotness any home has reported.  ReqFromInside
// returns the max of the local classification and the cached remote
// hint, so a requestor that has never been the home for a PC can still
// gate lookups correctly after the first response.
type PCTable struct {
	engine RDMAEngine

	Table       map[uint64]*PCCounter
	NumChiplets int

	// xDim is the mesh side length, captured at construction.  Needed by
	// the footprint clamp to derive the per-scheme cluster width (Warm
	// width = xDim/2; Hot/Global width = 2) and thus the per-LH fan-in.
	xDim int

	TotalReqCount uint64

	// RemoteHotness is a small piggyback cache: for each PC, the
	// highest home-reported Hotness seen in inbound responses.  Bounded
	// to remoteHotnessLimit entries with its own LRU clock so it does
	// not displace the main per-PC counters.
	RemoteHotness     map[uint64]*remoteHotnessEntry
	remoteHotnessTick uint64

	// Classifier configuration — captured at construction from the
	// global singleton (set by runner CLI flags) so classify() doesn't
	// pay a global lookup on every call.  When mode is
	// ClassifierThreshold, the weight/hopRef/shrink fields are unused
	// and the decision tree runs as before.
	mode         ClassifierMode
	wReuse       float64
	wShare       float64
	wHop         float64
	hopRef       float64 // pre-resolved (auto → mesh diameter)
	warmupShrink float64

	// wsDecayCtr counts Decay() invocations; every wsDecaySampleInterval
	// ticks the windowed-WS sketches are rank-decayed. Decoupled from the
	// per-tick reclassify so the WS forget rate matches L2 residency, not
	// the (finer) sampling period.
	wsDecayCtr int

	// wsDecaySampleInterval: WS rank-decay period in Decay() ticks,
	// captured at construction from the -pc-ws-decay-interval flag (via
	// the global singleton). Falls back to defaultWSDecaySampleInterval
	// when the flag is unset (<= 0).
	wsDecaySampleInterval int
}

// Classification thresholds — mirror oracle_pc_states.json criteria.
//
// Decision tree (matches the script used to generate the data oracle JSON):
//
//	reuse < 2                                       → Cold
//	sharing < 2, 4 ≤ reuse < 15                    → Warm_NonShared
//	sharing < 2, reuse ≥ 15                        → Hot_NonShared
//	sharing ≥ 2, 4 ≤ reuse < 15                    → Warm
//	sharing ≥ 2, reuse ≥ 15  (and not Global)      → Hot
//	sharing ≥ 10, reuse ≥ 30                       → Global
//
// Streaming classification is intentionally omitted in this PCTable —
// the engine treats below-Cold traffic as Cold here.
const (
	// reuseColdThreshold: per-cacheline avg access count below this is
	// considered no-reuse (Cold).
	reuseColdThreshold uint64 = 2

	// reuseWarmThreshold: minimum avg-per-cacheline to enter Warm tier.
	reuseWarmThreshold uint64 = 4

	// reuseHotThreshold: minimum avg-per-cacheline to enter Hot tier.
	reuseHotThreshold uint64 = 15

	// reuseGlobalThreshold: minimum avg-per-cacheline to enter Global tier.
	reuseGlobalThreshold uint64 = 30

	// sharingMultiThreshold: minimum #chiplets that touched the PC to be
	// considered "shared".  Below this, the PC is classified onto the
	// NonShared track.
	sharingMultiThreshold int = 2

	// sharingGlobalThreshold: minimum sharing for Global tier.
	sharingGlobalThreshold int = 10

	// minReqCountForClassification: minimum total requests before any
	// non-Cold classification is allowed.  Necessary for online learning
	// to avoid classifying one-shot transient PCs.
	minReqCountForClassification uint64 = 100

	// pcTableEntryLimit caps the number of live PC entries.  Kernel
	// dominant-PC counts are small; this fits comfortably in SRAM.
	pcTableEntryLimit = 10

	// hllBValue is the index-bit width passed to NewHLLUnit.  b=6 means
	// 64 registers per HLL (~88 bytes) with ~13% standard error — well
	// under the gaps between reuse-threshold tiers.
	hllBValue uint32 = 6

	// remoteHotnessLimit caps the piggyback cache.  Sized to match
	// pcTableEntryLimit since the working-set of "interesting PCs" is
	// the same on both the home and requestor sides.
	remoteHotnessLimit = pcTableEntryLimit

	// defaultWSDecaySampleInterval: number of Decay() ticks (each =
	// samplingPeriod cycles, default 10k) between WSAddrs rank-decays. One
	// DecayRanks() halves the estimate, so the WS time-constant τ ≈ interval
	// × samplingPeriod / ln2. 3 × 10k / 0.69 ≈ 43k cycles. Used when the
	// -pc-ws-decay-interval flag is unset (<= 0); otherwise the flag wins.
	defaultWSDecaySampleInterval = 3
)

// hotnessRank ranks the Hotness enum on its *semantic* axis so the
// monotonic-raise merge does not get fooled by the enum's numeric
// ordering (Warm_NonShared=4, Hot=2 — the latter is semantically
// stronger).  Order: Cold < Warm_NonShared < Hot_NonShared < Warm < Hot
// < Global.  Shared-track tiers (which trigger LH lookup) dominate the
// NonShared tiers; within each track, higher reuse dominates.  Streaming
// and Bypass are not produced by this PCTable and are treated as Cold
// for ranking purposes.
func hotnessRank(h protocol.Hotness) int {
	switch h {
	case protocol.Global:
		return 5
	case protocol.Hot:
		return 4
	case protocol.Warm:
		return 3
	case protocol.Hot_NonShared:
		return 2
	case protocol.Warm_NonShared:
		return 1
	default:
		return 0
	}
}

// NewPCTable constructs a PCTable for the given RDMA engine.
//
// xDim and yDim are taken as explicit arguments (mirroring
// NewOraclePCTable) because this constructor is called inside the
// engine's own constructor — before the platform builder calls
// SetXDim/SetYDim — so engine.GetXDim()/GetYDim() would still return 0
// at this point.  Passing them in keeps NumChiplets correct from the
// first ReqFromOutside call.
//
// Classifier configuration is read from the process-wide global
// singleton (populated by the runner CLI flags) and cached on the
// table — classify() does not consult global on the hot path.
func NewPCTable(engine RDMAEngine, xDim, yDim int) *PCTable {
	numChiplets := xDim * yDim

	g := global.GetInstance()
	mode := ClassifierThreshold
	if g.GetPCClassifierMode() == "score" {
		mode = ClassifierScore
	}

	hopRef := g.GetPCScoreHopRef()
	if hopRef <= 0 {
		// auto: mesh diameter. xDim==yDim is the wafer-scale convention
		// (square mesh); if asymmetric, prefer the longer axis to
		// avoid hopN clipping at 1.0 prematurely.
		dim := xDim
		if yDim > dim {
			dim = yDim
		}
		hopRef = float64((dim - 1) + (dim - 1))
	}
	if hopRef < 1 {
		hopRef = 1 // avoid div-by-zero in single-chiplet mode
	}

	wsDecayInterval := g.GetPCWSDecayInterval()
	if wsDecayInterval <= 0 {
		wsDecayInterval = defaultWSDecaySampleInterval
	}

	return &PCTable{
		engine:        engine,
		Table:         make(map[uint64]*PCCounter),
		NumChiplets:   numChiplets,
		xDim:          xDim,
		RemoteHotness: make(map[uint64]*remoteHotnessEntry),

		mode:                  mode,
		wReuse:                g.GetPCScoreWReuse(),
		wShare:                g.GetPCScoreWShare(),
		wHop:                  g.GetPCScoreWHop(),
		hopRef:                hopRef,
		warmupShrink:          g.GetPCScoreWarmupShrink(),
		wsDecaySampleInterval: wsDecayInterval,
	}
}

// newCounter allocates a fresh PCCounter with both HLL sketches and the
// ground-truth map pre-initialized.
func (t *PCTable) newCounter(pc uint64) *PCCounter {
	return &PCCounter{
		pc:             pc,
		UniqueAddrs:    NewHLLUnit(hllBValue),
		UniqueAddrChip: NewHLLUnit(hllBValue),
		WSAddrs:        NewHLLUnit(hllBValue),
		GTAddrs:        make(map[uint64]uint64),
	}
}

// evictLowestReuse removes the counter with the smallest HLL-estimated
// average accesses per cacheline (ReqCount / UniqueAddrs.Estimate()).
// Lowest-reuse PCs are the least valuable to keep classifying, so they
// are the victim.  Called only when the table is full and a new PC needs
// admission.
func (t *PCTable) evictLowestReuse() {
	var victimPC uint64
	var minAvg float64
	first := true
	for pc, c := range t.Table {
		unique := c.UniqueAddrs.Estimate()
		var avg float64
		if unique > 0 {
			avg = float64(c.ReqCount) / float64(unique)
		}
		if first || avg < minAvg {
			victimPC = pc
			minAvg = avg
			first = false
		}
	}
	if !first {
		delete(t.Table, victimPC)
	}
}

// ensureCounter returns the counter for `pc`, allocating it on first
// use.  When the table is at capacity, the lowest-reuse entry is evicted
// before admitting the new PC.
func (t *PCTable) ensureCounter(pc uint64) *PCCounter {
	if counter, ok := t.Table[pc]; ok {
		return counter
	}
	if len(t.Table) >= pcTableEntryLimit {
		t.evictLowestReuse()
	}
	counter := t.newCounter(pc)
	t.Table[pc] = counter
	return counter
}

// classify dispatches to the configured classifier (threshold or
// score) after applying the warm-up gate.  Pure function of the
// counter's fields — no state mutation.
//
// Warm-up gate: in score mode, a high per-PC hop count shrinks the
// minReqCountForClassification gate proportionally.  Rationale: hop
// count is nearly independent of request count (correlation ≈ 0.04
// in the analysis xlsx), so it is a reliable signal even when few
// requests have been observed.
func (t *PCTable) classify(counter *PCCounter) protocol.Hotness {
	if counter == nil || counter.ReqCount == 0 {
		return protocol.Cold
	}

	t.resolveDims()

	// HLL.Estimate() never returns 0 (it floors at 1), so ReqCount is
	// the gate that prevents division by a meaningless near-zero.
	unique := counter.UniqueAddrs.Estimate()
	if unique == 0 {
		return protocol.Cold
	}

	tuples := counter.UniqueAddrChip.Estimate()
	sharing := tuples / unique
	avgPerLine := counter.ReqCount / unique

	// Effective warm-up gate.  In score mode, scale down by hop signal.
	minReq := minReqCountForClassification
	if t.mode == ClassifierScore && t.warmupShrink > 0 && t.hopRef > 1 {
		hopN := t.computeHopNorm(counter)
		shrunk := float64(minReq) * (1.0 - t.warmupShrink*hopN)
		if shrunk < 10 {
			shrunk = 10
		}
		minReq = uint64(shrunk)
	}
	if counter.ReqCount < minReq {
		return protocol.Cold
	}

	var h protocol.Hotness
	if t.mode == ClassifierScore {
		h = t.scoreClassify(counter, avgPerLine, sharing)
	} else {
		h = t.thresholdClassify(avgPerLine, sharing)
	}

	// Plan v5 — footprint clamp.  Uses the *windowed* working-set estimate
	// (WSAddrs, periodically rank-decayed) rather than the lifetime unique:
	// the clamp asks whether the currently-active footprint fits in L2, and
	// lifetime cardinality monotonically overcounts that for any PC with
	// temporal locality. reuse/sharing above still use lifetime `unique`.
	wsUnique := unique
	if counter.WSAddrs != nil {
		wsUnique = counter.WSAddrs.Estimate()
	}
	h = t.clampByFootprint(h, wsUnique)

	// Plan v5 — NonShared + high-hop promotion.  A PC classified as
	// Warm_NonShared / Hot_NonShared has high reuse but few requestor
	// chiplets, so distribution to a cluster wouldn't share data among
	// peers.  However, if the single requestor's average hop count is
	// high, even a *local* replica saves significant round-trip cost.
	// Promote such PCs onto the shared track tier so they (a) win
	// eviction races over plain NonShared lines, and (b) get the
	// distribute/lookup treatment of their shared peers.
	return t.promoteNonSharedByHop(h, counter)
}

// L2 capacity in cachelines per chiplet (4 MB / 64 B).  Used as the
// reference unit for footprint clamping. Hardcoded here mirrors the
// L2 builder's default (`writeback_dynamic_dist.Builder.byteSize =
// 256 * mem.KB` × `numMemoryBankPerChiplet = 16` ⇒ 4 MB total).
const footprintL2Lines = uint64(65536)

// fanInForTier returns the per-Local-Home fan-in factor for a shared-track
// tier: how many home chiplets' distributions converge onto a single LH's
// L2.  A home only tracks the addresses homed to *itself*, but under
// round-robin placement every home owns a similar-sized slice and a Local
// Home accumulates copies from all homes outside its own cluster, spread
// across the cluster's clusterSize LH chiplets:
//
//	fanIn = (NumChiplets − clusterSize) / clusterSize
//
// e.g. 6×6 mesh: Global (area 2) → 17, Hot (area 4) → 8, Warm (area 6) → 5.
// Returns 1 for non-shared tiers (no distribution → no LH aggregation).
// resolveDims lazily backfills NumChiplets/xDim from the engine.  The
// PCTable is constructed inside the engine constructor — before the
// platform builder calls SetXDim/SetYDim — so the xDim,yDim passed to
// NewPCTable may have been 0.  By the time any request is processed the
// engine knows its real dimensions, so we read them once on first use.
// Idempotent: once NumChiplets is positive this is a cheap no-op.
func (t *PCTable) resolveDims() {
	if t.NumChiplets > 0 || t.engine == nil {
		return
	}
	x, y := t.engine.GetXDim(), t.engine.GetYDim()
	if x > 0 && y > 0 {
		t.xDim = x
		t.NumChiplets = x * y
	}
}

func (t *PCTable) fanInForTier(h protocol.Hotness) uint64 {
	wX, wY := clusterDims(h, t.xDim)
	if wX <= 0 {
		return 1
	}
	clusterSize := uint64(wX * wY)
	n := uint64(t.NumChiplets)
	if clusterSize >= n {
		return 1
	}
	return (n - clusterSize) / clusterSize
}

// isSharedTier reports whether a hotness label is on the distribution
// (shared) track — the only tiers whose per-LH footprint is amplified by
// fan-in.  NonShared tiers are served by L2 bypass / lookup, not
// distribution, so they never accumulate copies at a Local Home.
func isSharedTier(h protocol.Hotness) bool {
	return h == protocol.Warm || h == protocol.Hot || h == protocol.Global
}

// clampByFootprint reduces `h` toward Cold when the PC's distributed
// working set would not fit in a single chiplet's L2.  The estimate is the
// *per-Local-Home* footprint, not the home's own slice: a Local Home
// accumulates copies from many homes, so the effective footprint is
// `unique × fanIn(tier)` (see fanInForTier).  Shared tiers are demoted one
// step at a time (Global → Hot → Warm → Cold), recomputing the fan-in on
// each step — demoting Hot → Warm enlarges the cluster (fan-in 8 → 5 on
// 6×6), lowering the per-LH footprint so the line can fit without going
// all the way to Cold.
//
// NonShared tiers are not distributed, so the relevant capacity is the
// requestor's own L2: if a single PC's working set alone exceeds L2, its
// reuse cannot be realized (capacity miss) and it is demoted to Cold.
func (t *PCTable) clampByFootprint(
	h protocol.Hotness, unique uint64,
) protocol.Hotness {
	for isSharedTier(h) {
		if unique*t.fanInForTier(h) < footprintL2Lines {
			break
		}
		h = demoteOneTier(h)
	}

	if h == protocol.Warm_NonShared || h == protocol.Hot_NonShared {
		if unique >= footprintL2Lines {
			return protocol.Cold
		}
	}

	return h
}

// demoteOneTier moves a hotness label down by exactly one enum step,
// following the canonical order Global > Hot[_NonShared] > Warm[_NonShared]
// > Cold.  Used by the footprint clamp.
func demoteOneTier(h protocol.Hotness) protocol.Hotness {
	switch h {
	case protocol.Global:
		return protocol.Hot
	case protocol.Hot:
		return protocol.Warm
	case protocol.Hot_NonShared:
		return protocol.Warm_NonShared
	case protocol.Warm:
		return protocol.Cold
	case protocol.Warm_NonShared:
		return protocol.Cold
	}
	return h
}

// nonSharedHopPromoteThreshold is the normalized hop-count threshold
// (hopN ∈ [0,1] where 1 = mesh diameter) above which NonShared PCs are
// promoted onto the shared-track tier.  0.3 corresponds to avg hop ≈
// 3.7 on a 6×6 mesh (diameter = 10), i.e. accesses that travel more
// than a third of the mesh on average.
const nonSharedHopPromoteThreshold = 0.3

// promoteNonSharedByHop pushes NonShared PCs with high average hop
// count onto the shared-track tier.  Rationale: a NonShared
// classification means "only one chiplet touches this data" — there's
// no cluster-peer to share replicas with, so distribution looks wasted.
// But when the single requestor's accesses traverse many hops, a
// closer replica (in any cluster peer's L2) still cuts round-trip
// latency.  Promoting moves the line into the regular shared track so
// it (a) outranks plain NonShared on eviction (hotnessRank: Warm_NonShared
// < Warm, Hot_NonShared < Hot — see bypasspolicy.go), and (b) is treated
// as a distribution candidate by the RDMA engine's scheme switch.
func (t *PCTable) promoteNonSharedByHop(
	h protocol.Hotness, counter *PCCounter,
) protocol.Hotness {
	if h != protocol.Warm_NonShared && h != protocol.Hot_NonShared {
		return h
	}
	hopN := t.computeHopNorm(counter)
	if hopN < nonSharedHopPromoteThreshold {
		return h
	}
	switch h {
	case protocol.Warm_NonShared:
		return protocol.Warm
	case protocol.Hot_NonShared:
		return protocol.Hot
	}
	return h
}

// thresholdClassify is the original decision tree over (reuse, sharing).
// Mirrors the oracle_pc_states.json generation script and is the
// default classifier.  Streaming is intentionally omitted — below-Cold
// traffic is just Cold here.
func (t *PCTable) thresholdClassify(
	avgPerLine, sharing uint64,
) protocol.Hotness {
	if avgPerLine < reuseColdThreshold {
		return protocol.Cold
	}

	// NonShared track: high reuse but only one (or near-one) chiplet
	// touches the data.  Distribution would be wasted; the value of
	// L2 bypass is in freeing aggregate cluster capacity.
	if sharing < uint64(sharingMultiThreshold) {
		if avgPerLine < reuseWarmThreshold {
			return protocol.Cold
		}
		if avgPerLine < reuseHotThreshold {
			return protocol.Warm_NonShared
		}
		return protocol.Hot_NonShared
	}

	// Shared track: distribution candidates.
	if avgPerLine < reuseWarmThreshold {
		return protocol.Cold
	}
	if avgPerLine >= reuseGlobalThreshold &&
		sharing >= uint64(sharingGlobalThreshold) {
		return protocol.Global
	}
	if avgPerLine >= reuseHotThreshold {
		return protocol.Hot
	}
	return protocol.Warm
}

// Score-tier thresholds — tuned against oracle_pc_states.json labels on
// t_pc_stats_l2miss_pte_per_owner_1.xlsx.  Default weights
// (0.40, 0.30, 0.30) yield ~72% full-data tier accuracy with 82/99
// non-Cold PCs recovered in a reuse-capped warm-up simulation.
const (
	scoreColdShared    = 0.30
	scoreColdNonShared = 0.40
	scoreWarm          = 0.50
	scoreWarmNonShared = 0.65
	scoreGlobal        = 0.75
)

// scoreClassify computes a weighted score from normalized
// (reuse, sharing, hop) and maps it to a hotness tier.  Sharing still
// drives the shared/NonShared track split (same as threshold mode)
// because L2 bypass vs distribution is a structural decision that
// shouldn't depend on hop count.
func (t *PCTable) scoreClassify(
	counter *PCCounter,
	avgPerLine, sharing uint64,
) protocol.Hotness {
	reuseN := float64(avgPerLine) / float64(reuseGlobalThreshold)
	if reuseN > 1 {
		reuseN = 1
	}
	shareN := float64(sharing) / float64(sharingGlobalThreshold)
	if shareN > 1 {
		shareN = 1
	}
	hopN := t.computeHopNorm(counter)

	score := t.wReuse*reuseN + t.wShare*shareN + t.wHop*hopN

	if sharing < uint64(sharingMultiThreshold) {
		switch {
		case score < scoreColdNonShared:
			return protocol.Cold
		case score < scoreWarmNonShared:
			return protocol.Warm_NonShared
		default:
			return protocol.Hot_NonShared
		}
	}
	if score < scoreColdShared {
		return protocol.Cold
	}
	if score >= scoreGlobal && sharing >= uint64(sharingGlobalThreshold) {
		return protocol.Global
	}
	if score < scoreWarm {
		return protocol.Warm
	}
	return protocol.Hot
}

// computeHopNorm returns the per-PC average hop count normalized to
// [0,1] against hopRef.  Hop 1 (immediate neighbor) maps to 0, hop
// hopRef (mesh diameter) maps to 1.  Defensive against zero ReqCount
// (returns 0 — caller already filtered ReqCount==0 upstream).
func (t *PCTable) computeHopNorm(counter *PCCounter) float64 {
	if counter.ReqCount == 0 || t.hopRef <= 1 {
		return 0
	}
	avgHop := float64(counter.HopCount) / float64(counter.ReqCount)
	hopN := (avgHop - 1) / (t.hopRef - 1)
	if hopN < 0 {
		hopN = 0
	}
	if hopN > 1 {
		hopN = 1
	}
	return hopN
}

// ReqFromOutside is called when this chiplet (the owner) receives a remote
// request for `addr` from `requestorId`.  Updates the HLL sketches, the
// ground-truth map, and the bookkeeping counters used by classify().
func (t *PCTable) ReqFromOutside(
	pc uint64,
	addr uint64,
	requestorId int,
	hopCount int,
) {
	if pc == 0 {
		return
	}

	t.resolveDims()
	counter := t.ensureCounter(pc)

	// HLL_1: distinct addr (lifetime) + windowed WS mirror.
	counter.UniqueAddrs.Add(addr)
	counter.WSAddrs.Add(addr)

	// HLL_2: distinct (addr, chiplet) tuple.  XOR in chipletId+1 shifted
	// up so chiplet 0's "identity" doesn't collapse onto the HLL_1
	// hash space, while leaving 56 bits of addr unaltered (supports up
	// to 256 chiplets).
	if requestorId >= 0 {
		combined := addr ^ (uint64(requestorId+1) << 56)
		counter.UniqueAddrChip.Add(combined)
	} else {
		counter.UniqueAddrChip.Add(addr)
	}

	// Ground truth — record this chiplet in the per-addr bitmap.
	if requestorId >= 0 && requestorId < 64 {
		counter.GTAddrs[addr] |= 1 << uint(requestorId)
	} else {
		// Beyond 64 chiplets, fall back to recording the addr without
		// chiplet attribution.  Current wafer-scale targets are ≤ 64.
		if _, ok := counter.GTAddrs[addr]; !ok {
			counter.GTAddrs[addr] = 0
		}
	}

	counter.ReqCount++
	counter.HopCount += uint64(hopCount)
	t.TotalReqCount++
}

// ReqFromInside is the request-side classification query — pure read of
// the current counter state, no mutation.  Called by the requestor
// chiplet's RDMA engine to decide lookup gating.
//
// Returns the max (by hotnessRank) of:
//   - the local classification from this chiplet's PCCounter (if any),
//   - the cached remote hint from MergeRemoteHotness (if any).
//
// The remote hint covers the common case where this chiplet has never
// been the home for the PC's addresses, so the local table has no
// entry — but at least one home has already responded with its own
// classification.
func (t *PCTable) ReqFromInside(pc uint64) protocol.Hotness {
	if pc == 0 {
		return protocol.Cold
	}

	local := protocol.Cold
	if counter, ok := t.Table[pc]; ok {
		local = counter.CachedHotness // last periodic reclassify (Decay)
	}

	remote := protocol.Cold
	if entry, ok := t.RemoteHotness[pc]; ok {
		remote = entry.Hotness
	}

	if hotnessRank(remote) > hotnessRank(local) {
		return remote
	}
	return local
}

// MergeRemoteHotness folds a home-classified hotness hint received via
// response piggyback into the requestor-side cache.  The merge is
// monotonic in hotnessRank — an incoming hint never lowers a previously
// cached value.  This way the cache converges to the maximum hotness
// reported by any home that has answered the requestor, which is the
// "most informed" view for lookup-gating purposes (a single home that
// has not yet accumulated minReqCountForClassification requests would
// otherwise erase a stronger signal seen earlier).
//
// Cold hints carry no information and are dropped.  The cache is
// bounded to remoteHotnessLimit entries with its own LRU clock.
func (t *PCTable) MergeRemoteHotness(pc uint64, remote protocol.Hotness) {
	if pc == 0 {
		return
	}
	if hotnessRank(remote) == 0 {
		// Cold (or any non-shared/non-hot value we don't rank) — nothing
		// to learn.  Avoid burning an LRU slot on it.
		return
	}

	t.remoteHotnessTick++

	if entry, ok := t.RemoteHotness[pc]; ok {
		if hotnessRank(remote) > hotnessRank(entry.Hotness) {
			entry.Hotness = remote
		}
		entry.LastAccessTick = t.remoteHotnessTick
		return
	}

	if len(t.RemoteHotness) >= remoteHotnessLimit {
		t.evictLRURemoteHotness()
	}
	t.RemoteHotness[pc] = &remoteHotnessEntry{
		Hotness:        remote,
		LastAccessTick: t.remoteHotnessTick,
	}
}

// evictLRURemoteHotness removes the cache entry with the smallest
// LastAccessTick.  Called only when the cache is full and a new PC
// needs to be admitted.
func (t *PCTable) evictLRURemoteHotness() {
	var oldestPC uint64
	var oldestTick uint64
	first := true
	for pc, e := range t.RemoteHotness {
		if first || e.LastAccessTick < oldestTick {
			oldestPC = pc
			oldestTick = e.LastAccessTick
			first = false
		}
	}
	if !first {
		delete(t.RemoteHotness, oldestPC)
	}
}

// RspFromInside is the response-side classification query.  It returns the
// cached classify() result directly.
//
// Behavior:
//   - For an unknown PC (never seen by ReqFromOutside), returns Cold
//     without allocating a counter.
//   - Otherwise returns the tier from the last periodic reclassify.
func (t *PCTable) RspFromInside(
	pc uint64,
	addr uint64,
) protocol.Hotness {
	if pc == 0 {
		return protocol.Cold
	}
	counter, ok := t.Table[pc]
	if !ok {
		return protocol.Cold
	}

	// Cached tier from the last periodic reclassify (Decay) — no per-request
	// HLL Estimate.
	return counter.CachedHotness
}

// StampFirstIncrease records the simulation time `now` at which this PC's
// classified hotness first rises above its previous tier (by hotnessRank).
// Called by the engine right after ReqFromOutside updates the counter, so
// the classification reflects the just-observed request. Only the first
// increase is stamped (HasIncreased guard); subsequent rises are ignored.
// No-op for an unknown PC.
func (t *PCTable) StampFirstIncrease(pc uint64, now float64) {
	counter, ok := t.Table[pc]
	if !ok {
		return
	}
	r := hotnessRank(counter.CachedHotness)
	if r > counter.lastHotnessRank && !counter.HasIncreased {
		counter.FirstIncreaseTime = now
		counter.HasIncreased = true
	}
	counter.lastHotnessRank = r
}

// Distribute satisfies PCTableInterface.  The production table no longer
// keeps a per-PC distribution counter (the response-side cap was removed),
// so this is a no-op.
func (t *PCTable) Distribute(pc uint64) {}

// Decay is called periodically (samplingPeriod) by the engine. It is the
// single per-window maintenance hook and does two things per tick:
//  1. every wsDecaySampleInterval ticks, rank-decays each WSAddrs sketch so
//     the footprint clamp sees a windowed working set, and
//  2. recomputes CachedHotness for every counter so runtime queries never
//     have to run classify() (HLL Estimate) on the per-request path.
func (t *PCTable) Decay() {
	t.wsDecayCtr++
	decayWS := false
	if t.wsDecayCtr >= t.wsDecaySampleInterval {
		t.wsDecayCtr = 0
		decayWS = true
	}

	for _, counter := range t.Table {
		if decayWS && counter.WSAddrs != nil {
			counter.WSAddrs.DecayRanks()
		}

		// Reclassify AFTER the WS decay so the cached tier reflects the
		// freshly-shrunk footprint.
		counter.CachedHotness = t.classify(counter)
	}
}

// Reset clears all per-PC state, including the piggyback remote-hotness
// cache.  Used by the engine on Reset.
func (t *PCTable) Reset() {
	t.Table = make(map[uint64]*PCCounter)
	t.TotalReqCount = 0
	t.RemoteHotness = make(map[uint64]*remoteHotnessEntry)
	t.remoteHotnessTick = 0
	t.wsDecayCtr = 0
}

// GetPCTable returns the underlying counter map for inspection / metrics.
func (t *PCTable) GetPCTable() interface{} {
	return t.Table
}

// HotnessOfPC returns the local classification for a PC (no piggyback
// merge).  Used by the reporter to dump per-PC hotness as a metric
// column; semantically distinct from ReqFromInside which folds in the
// remote-hotness cache.  Returns Cold for unknown PCs.
func (t *PCTable) HotnessOfPC(pc uint64) protocol.Hotness {
	counter, ok := t.Table[pc]
	if !ok {
		return protocol.Cold
	}
	return t.classify(counter)
}

// GetCounterValidation produces per-PC HLL-vs-ground-truth comparison
// records for the reporter.  Pure read of state — safe to call after
// simulation end.
func (t *PCTable) GetCounterValidation() map[uint64]*PCCounterValidation {
	out := make(map[uint64]*PCCounterValidation, len(t.Table))
	for pc, c := range t.Table {
		v := &PCCounterValidation{
			PC:        pc,
			ReqCount:  c.ReqCount,
			HopCount:  c.HopCount,
			HLLUnique: c.UniqueAddrs.Estimate(),
			HLLTuples: c.UniqueAddrChip.Estimate(),
			Hotness:   t.classify(c),
		}
		if v.HLLUnique > 0 {
			v.HLLSharingAvg = float64(v.HLLTuples) / float64(v.HLLUnique)
			v.HLLAvgCacheline = float64(c.ReqCount) / float64(v.HLLUnique)
		}

		// Ground truth — popcount distribution over all addresses.
		gtUnique := uint64(len(c.GTAddrs))
		v.GTUnique = gtUnique
		if gtUnique > 0 {
			pops := make([]int, 0, gtUnique)
			var sum uint64
			for _, bm := range c.GTAddrs {
				p := bits.OnesCount64(bm)
				pops = append(pops, p)
				sum += uint64(p)
			}
			v.GTTuples = sum
			v.GTSharingAvg = float64(sum) / float64(gtUnique)

			sort.Ints(pops)
			v.GTSharingMin = pops[0]
			v.GTSharingMax = pops[len(pops)-1]
			v.GTSharingP50 = pops[pIdx(len(pops), 0.50)]
			v.GTSharingP90 = pops[pIdx(len(pops), 0.90)]
			v.GTSharingP99 = pops[pIdx(len(pops), 0.99)]
		}
		out[pc] = v
	}
	return out
}

// pIdx returns the index into a sorted slice of length n that
// corresponds to percentile p (0.0–1.0), clamped to a valid range.
func pIdx(n int, p float64) int {
	if n <= 0 {
		return 0
	}
	idx := int(float64(n) * p)
	if idx >= n {
		idx = n - 1
	}
	if idx < 0 {
		idx = 0
	}
	return idx
}
