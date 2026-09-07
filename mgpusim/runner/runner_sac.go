package runner

import (
	"flag"
	"fmt"

	"gitlab.com/akita/mgpusim/sac"
	"gitlab.com/akita/util/global"
)

// SAC (Sharing-Aware Caching, ISCA'23) flags — only consumed when
// -platform-type=sac.
var (
	sacMode = flag.String("sac-mode", "dynamic",
		"SAC LLC mode: dynamic (EAB-model decision per kernel), "+
			"memory (force memory-side), sm (force SM-side)")
	sacProfileWindow = flag.Int("sac-profile-window", 2000,
		"SAC profiling window at the beginning of each kernel, in cycles")
	sacTheta = flag.Float64("sac-theta", 0.05,
		"fraction by which the SM-side EAB must exceed the memory-side "+
			"EAB to reconfigure the LLC")
	sacBIntra = flag.Float64("sac-b-intra", 10240,
		"EAB model: intra-chiplet bandwidth in GB/s (0 = derive)")
	sacBInter = flag.Float64("sac-b-inter", 1024,
		"EAB model: inter-chiplet bandwidth in GB/s (0 = derive)")
	sacBLLC = flag.Float64("sac-b-llc", 10240,
		"EAB model: raw LLC bandwidth in GB/s (0 = derive)")
	sacBMem = flag.Float64("sac-b-mem", 1024,
		"EAB model: raw memory bandwidth in GB/s (0 = derive)")
)

// configureSAC publishes the SAC CLI configuration to the global state the
// SAC platform builder reads. Called before the platform is built.
func (r *Runner) configureSAC() {
	s := global.GetInstance().SAC()
	s.ForcedMode = *sacMode
	s.ProfileWindow = *sacProfileWindow
	s.Theta = *sacTheta
	s.BIntraGBs = *sacBIntra
	s.BInterGBs = *sacBInter
	s.BLLCGBs = *sacBLLC
	s.BMemGBs = *sacBMem
}

// reportSACDecisions emits one CSV row group per kernel decision made by
// the SAC controller.
func (r *Runner) reportSACDecisions() {
	if !global.GetInstance().SAC().Enabled {
		return
	}

	for _, d := range sac.AllDecisions() {
		where := "SAC"
		prefix := fmt.Sprintf("sac_kernel%02d_", d.Kernel)
		mode := 0.0
		if d.Mode == global.SACSMSide {
			mode = 1.0
		}
		r.metricsCollector.Collect(where, prefix+"mode_sm_side", mode)
		r.metricsCollector.Collect(where, prefix+"eab_mem_gbs", d.EABMem)
		r.metricsCollector.Collect(where, prefix+"eab_sm_gbs", d.EABSM)
		r.metricsCollector.Collect(where, prefix+"r_local", d.RLocal)
		r.metricsCollector.Collect(where, prefix+"lsu_mem", d.LSUMem)
		r.metricsCollector.Collect(where, prefix+"lsu_sm", d.LSUSM)
		r.metricsCollector.Collect(where, prefix+"hit_mem", d.HitMem)
		r.metricsCollector.Collect(where, prefix+"hit_sm_pred", d.HitSM)
		r.metricsCollector.Collect(where, prefix+"profile_reqs", float64(d.TotalReqs))
	}
}
