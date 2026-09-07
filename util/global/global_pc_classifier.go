package global

// PC classifier configuration knobs.
//
// The runtime PCTable supports two classification modes:
//
//	"threshold" (default): the original decision tree over (reuse,
//	    sharing).  Matches the oracle JSON generation script exactly.
//
//	"score":   a weighted sum of normalized (reuse, sharing, hop)
//	    metrics.  Helps during the per-PC warm-up window because hop
//	    count is meaningful even with few samples (it is nearly
//	    independent of request count — see analysis on
//	    t_pc_stats_l2miss_pte_per_owner_1.xlsx).
//
// Weights default to (0.40, 0.30, 0.30) — chosen by sweeping against
// oracle labels on the analysis xlsx: this combination preserves ~72%
// full-data tier accuracy and recovers 82/99 non-Cold PCs in a
// reuse-capped warm-up simulation.
//
// hopRef is the denominator that normalizes raw hop counts to [0,1].
// A value of 0 means "auto" — the PCTable substitutes the mesh diameter
// 2*(xDim-1) at construction.  Override only for experiments.
//
// warmupShrink controls how aggressively a high hop score lowers the
// minReqCountForClassification gate (so a high-hop PC can be tiered
// after fewer requests).  Range [0,1]; 0 disables the shrink.

// SetPCClassifierMode sets the classification mode ("threshold" or
// "score").  Unknown values are normalized to "threshold" by the
// PCTable consumer.
func (g *Global) SetPCClassifierMode(mode string) { g.pcClassifierMode = mode }

// GetPCClassifierMode returns the configured classifier mode.  An
// empty string is treated as "threshold" by the PCTable.
func (g *Global) GetPCClassifierMode() string { return g.pcClassifierMode }

func (g *Global) SetPCScoreWReuse(w float64) { g.pcScoreWReuse = w }
func (g *Global) GetPCScoreWReuse() float64  { return g.pcScoreWReuse }

func (g *Global) SetPCScoreWShare(w float64) { g.pcScoreWShare = w }
func (g *Global) GetPCScoreWShare() float64  { return g.pcScoreWShare }

func (g *Global) SetPCScoreWHop(w float64) { g.pcScoreWHop = w }
func (g *Global) GetPCScoreWHop() float64  { return g.pcScoreWHop }

func (g *Global) SetPCScoreHopRef(r float64) { g.pcScoreHopRef = r }
func (g *Global) GetPCScoreHopRef() float64  { return g.pcScoreHopRef }

func (g *Global) SetPCScoreWarmupShrink(s float64) { g.pcScoreWarmupShrink = s }
func (g *Global) GetPCScoreWarmupShrink() float64  { return g.pcScoreWarmupShrink }

// SetPCWSDecayInterval / GetPCWSDecayInterval — number of Decay() ticks
// between WSAddrs rank-decays.  A value <= 0 means "use the PCTable's
// built-in default" so direct programmatic uses of NewPCTable still work.
func (g *Global) SetPCWSDecayInterval(n int) { g.pcWSDecayInterval = n }
func (g *Global) GetPCWSDecayInterval() int  { return g.pcWSDecayInterval }
