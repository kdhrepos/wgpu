package global

// SACLLCMode selects the LLC organization of the SAC platform
// (Sharing-Aware Caching, ISCA'23).
type SACLLCMode int

const (
	// SACMemorySide: each chiplet's L2 caches only data from its local
	// memory partition and serves requests from all chiplets. Requests
	// from local SMs to remote addresses pass through the local L2
	// without lookup/install.
	SACMemorySide SACLLCMode = iota
	// SACSMSide: each chiplet's L2 caches everything its local SMs
	// access (remote data is replicated). Inbound RDMA requests pass
	// through the home L2 straight to DRAM.
	SACSMSide
)

func (m SACLLCMode) String() string {
	if m == SACSMSide {
		return "sm-side"
	}
	return "memory-side"
}

// SACState holds the runtime mode and the profiling counters of the SAC
// platform. The engine is single-threaded, so no locking is required.
type SACState struct {
	Enabled   bool
	Mode      SACLLCMode
	Profiling bool

	NumChiplets        int
	NumBanksPerChiplet int

	// Configuration, set by the runner before the platform is built and
	// consumed by the SAC platform builder.
	ProfileWindow int     // cycles; 0 = default (2000)
	Theta         float64 // 0 = default (0.05)
	ForcedMode    string  // "", "dynamic", "memory", "sm"
	// EAB bandwidth constants in GB/s; 0 = derive from platform params.
	BIntraGBs float64
	BInterGBs float64
	BLLCGBs   float64
	BMemGBs   float64

	// Counters collected during the profiling window (memory-side
	// configuration active). Slice counters are indexed by global bank
	// id (chiplet*NumBanksPerChiplet + bank).
	TotalReqs    uint64
	LocalReqs    uint64
	SliceReqsMem []uint64 // slice each request hits under memory-side
	SliceReqsSM  []uint64 // slice each request would hit under SM-side
	HitsMem      uint64
	LookupsMem   uint64
	CRDHits      uint64 // predicted SM-side hits (set-sampled CRD)
	CRDReqs      uint64
}

// ResetCounters clears the per-kernel profiling counters.
func (s *SACState) ResetCounters() {
	s.TotalReqs = 0
	s.LocalReqs = 0
	s.HitsMem = 0
	s.LookupsMem = 0
	s.CRDHits = 0
	s.CRDReqs = 0
	n := s.NumChiplets * s.NumBanksPerChiplet
	if len(s.SliceReqsMem) != n {
		s.SliceReqsMem = make([]uint64, n)
		s.SliceReqsSM = make([]uint64, n)
		return
	}
	for i := range s.SliceReqsMem {
		s.SliceReqsMem[i] = 0
		s.SliceReqsSM[i] = 0
	}
}

// SAC returns the process-wide SAC state. Zero value means SAC is
// disabled and all SAC branches in shared code are skipped.
func (g *Global) SAC() *SACState {
	if g.sacState == nil {
		g.sacState = &SACState{}
	}
	return g.sacState
}
