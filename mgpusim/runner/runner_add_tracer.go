// Package runner defines how default benchmark samples are executed.
package runner

import (
	"strings"

	"gitlab.com/akita/akita"
	"gitlab.com/akita/mem/cache"
	"gitlab.com/akita/mem/idealmemcontroller"
	"gitlab.com/akita/mem/vm/addresstranslator"
	"gitlab.com/akita/mem/vm/mmu"
	"gitlab.com/akita/mem/vm/tlb"
	"gitlab.com/akita/mgpusim/rdma"
	"gitlab.com/akita/mgpusim/timing/caches"
	"gitlab.com/akita/mgpusim/timing/caches/rob"
	cupkg "gitlab.com/akita/mgpusim/timing/cu"
	"gitlab.com/akita/noc/networking/chipnetwork"
	"gitlab.com/akita/util/tracing"
)

// enableWarpStallTracing turns on per-CU warp stall and memory latency
// counters when -report-warp-stall is set. The counters are dumped at the
// end of simulation by reportWarpStall().
func (r *Runner) enableWarpStallTracing() {
	if !r.ReportWarpStall {
		return
	}
	for _, gpu := range r.GPUDriver.GPUs {
		for _, comp := range gpu.CUs {
			if c, ok := comp.(*cupkg.ComputeUnit); ok {
				c.EnableStallTracing = true
			}
		}
	}
}

func (r *Runner) addLocalRemoteTracer() {
	r.LocalRemoteTracer = tracing.NewLocalRemoteTracer()

	for _, gpu := range r.GPUDriver.GPUs {
		for _, cache := range gpu.L2Caches {
			tracing.CollectTrace(cache, r.LocalRemoteTracer)
		}
	}
}

func (r *Runner) addPteCachingTracer() {
	r.PteCachingTracer = tracing.NewPteCachingTracer(*xDim, *yDim)

	for _, gpu := range r.GPUDriver.GPUs {
		for _, cache := range gpu.L2Caches {
			tracing.CollectTrace(cache, r.PteCachingTracer)
		}
		for _, engine := range gpu.ChipRDMAEngines {
			tracing.CollectTrace(engine, r.PteCachingTracer)
		}
		for _, net := range gpu.InterChipletNetwork {
			for _, swArr := range net.GetChipSwitches() {
				for _, sw := range swArr {
					tracing.CollectTrace(sw, r.PteCachingTracer)
				}
			}
		}
	}
}

func (r *Runner) addPteAccessTracer(name string) {
	if !r.TracePteAccess {
		return
	}

	r.PteAccessTracer = tracing.NewPteAccessTracer(
		name,
		*xDim,
		*yDim)

	for _, gpu := range r.GPUDriver.GPUs {
		for _, mmu := range gpu.MMUs {
			tracing.CollectTrace(mmu, r.PteAccessTracer)
		}
	}
}

func (r *Runner) addNumFlitTracer() {
	r.NumFlitTracer = tracing.NewNumFlitTracer(
		func(t tracing.Task) bool {
			return t.Kind == "num-flits"
		})

	// msg→flit splitting (and the "num-flits" task) now fires on the
	// source EndPoint via tracing.StartTask(ep, ...), not the switches.
	// A hook on a switch never sees it, so attach to every EndPoint.
	// NumFlitTracer dedups by msg ID.
	for _, gpu := range r.GPUDriver.GPUs {
		for _, net := range gpu.InterChipletNetwork {
			conn := net.GetSwitchConnector()
			if conn == nil {
				continue
			}
			for _, ep := range conn.GetEndPoints() {
				tracing.CollectTrace(ep, r.NumFlitTracer)
			}
		}
	}
}

func (r *Runner) addFlushTracer() {
	r.FlushTracer = tracing.NewFlushTracer(
		func(t tracing.Task) bool {
			return t.Kind == "flushing"
		},
		*xDim,
		*yDim)
	tracing.CollectTrace(r.GPUDriver, r.FlushTracer)
}

func (r *Runner) addMemoryAccessTracer(name string) {
	if !r.TraceMemoryAccess {
		return
	}
	tracer := tracing.NewMemoryAccessTracer(name, *xDim, *yDim)
	// tracer := tracing.NewRdmaTracer(name)

	r.MemoryAccessTracer = tracer

	for _, gpu := range r.GPUDriver.GPUs {
		// for _, e := range gpu.ChipRDMAEngines {
		// 	tracing.CollectTrace(e, tracer)
		// }
		for _, cache := range gpu.L2Caches {
			tracing.CollectTrace(cache, tracer)
		}
	}
}

func (r *Runner) addCacheLineHopTracer() {
	if !r.TraceCacheLineHopStats {
		return
	}
	tracer := tracing.NewCacheLineHopTracer()

	r.CacheLineHopTracer = tracer

	for _, gpu := range r.GPUDriver.GPUs {
		for _, e := range gpu.ChipRDMAEngines {
			tracing.CollectTrace(e, tracer)
		}
		for _, cache := range gpu.L2Caches {
			tracing.CollectTrace(cache, tracer)
		}
	}
}

func (r *Runner) addWorkgroupDimensionTracer() {
	r.WorkgroupDimensionTracer = tracing.NewWorkgroupDimensionTracer(
		func(task tracing.Task) bool {
			return task.Kind == "workgroup-dimension"
		})

	for _, gpu := range r.GPUDriver.GPUs {
		for _, dispatcher := range gpu.CommandProcessor.Dispatchers {
			tracing.CollectTrace(dispatcher, r.WorkgroupDimensionTracer)
		}
	}
}

func (r *Runner) addKernelTracer() {
	r.KernelTracer = tracing.NewKernelTracer(
		func(task tracing.Task) bool {
			return task.Kind == "kernel-trace"
		})

	tracing.CollectTrace(r.GPUDriver, r.KernelTracer)
}

func (r *Runner) addMemoryUsageTracer() {
	tracer := tracing.NewMemoryUsageTracer(
		func(task tracing.Task) bool {
			return task.Kind == "mem-alloc"
		})

	r.MemoryUsageTracer = tracer
	tracing.CollectTrace(r.GPUDriver, tracer)
}

func (r *Runner) addMaxInstStopper() {
	if *maxInstCount == 0 {
		return
	}

	r.maxInstStopper = newInstStopper(*maxInstCount, r, *logDir)

	for _, gpu := range r.GPUDriver.GPUs {
		for _, cu := range gpu.CUs {
			tracing.CollectTrace(cu.(tracing.NamedHookable), r.maxInstStopper)
		}
	}
}

func (r *Runner) addKernelTimeTracer() {
	r.KernelTimeCounter = tracing.NewBusyTimeTracer(
		func(task tracing.Task) bool {
			return task.What == "*driver.LaunchKernelCommand"
		})
	tracing.CollectTrace(r.GPUDriver, r.KernelTimeCounter)

	for _, gpu := range r.GPUDriver.GPUs {
		gpuKernelTimeCounter := tracing.NewBusyTimeTracer(
			func(task tracing.Task) bool {
				return task.What == "*protocol.LaunchKernelReq"
			})
		r.PerGPUKernelTimeCounter = append(
			r.PerGPUKernelTimeCounter, gpuKernelTimeCounter)
		tracing.CollectTrace(gpu.CommandProcessor, gpuKernelTimeCounter)
	}
}

func (r *Runner) addInstCountTracer() {
	if !r.ReportInstCount {
		return
	}

	for _, gpu := range r.GPUDriver.GPUs {
		for _, cu := range gpu.CUs {
			tracer := newInstTracer(r, *logDir)
			r.InstCountTracers = append(r.InstCountTracers,
				instCountTracer{
					tracer: tracer,
					cu:     cu,
				})
			tracing.CollectTrace(cu.(tracing.NamedHookable), tracer)
		}
	}
}

func (r *Runner) addCacheLatencyTracer() {
	if !r.ReportCacheLatency {
		return
	}

	for _, gpu := range r.GPUDriver.GPUs {
		for _, cache := range gpu.L1ICaches {
			tracer := tracing.NewAverageTimeTracer(
				func(task tracing.Task) bool {
					return task.Kind == "req_in"
				})
			r.CacheLatencyTracers = append(r.CacheLatencyTracers,
				cacheLatencyTracer{tracer: tracer, cache: cache})
			tracing.CollectTrace(cache, tracer)
		}

		for _, cache := range gpu.L1SCaches {
			tracer := tracing.NewAverageTimeTracer(
				func(task tracing.Task) bool {
					return task.Kind == "req_in"
				})
			r.CacheLatencyTracers = append(r.CacheLatencyTracers,
				cacheLatencyTracer{tracer: tracer, cache: cache})
			tracing.CollectTrace(cache, tracer)
		}

		for _, cache := range gpu.L1VCaches {
			tracer := tracing.NewAverageTimeTracer(
				func(task tracing.Task) bool {
					return task.Kind == "req_in"
				})
			r.CacheLatencyTracers = append(r.CacheLatencyTracers,
				cacheLatencyTracer{tracer: tracer, cache: cache})
			tracing.CollectTrace(cache, tracer)
		}

		for _, cache := range gpu.L2Caches {
			tracer := tracing.NewAverageTimeTracer(
				func(task tracing.Task) bool {
					return task.Kind == "req_in"
				})
			r.CacheLatencyTracers = append(r.CacheLatencyTracers,
				cacheLatencyTracer{tracer: tracer, cache: cache})
			tracing.CollectTrace(cache, tracer)
		}

		// for _, cache := range gpu.L2Caches {
		// 	pipelineForLocal := cache.GetPipeline()
		// 	tracer := tracing.NewAverageTimeTracer(
		// 		func(task tracing.Task) bool {
		// 			return task.Kind == "pipeline"
		// 		})
		// 	r.L2PipelineLatencyTracers = append(r.L2PipelineLatencyTracers,
		// 		L2PipelineLatencyTracer{tracer: tracer, pipeline: pipeline})
		// 	tracing.CollectTrace(pipeline, tracer)
		// }

	}
}

func (r *Runner) addTLBLatencyTracer() {
	if !r.ReportTLBLatency {
		return
	}

	for _, gpu := range r.GPUDriver.GPUs {
		for _, tlb := range gpu.L1ITLBs {
			tracer := tracing.NewAverageTimeTracer(
				func(task tracing.Task) bool {
					return task.Kind == "req_in"
				})
			r.TLBLatencyTracers = append(r.TLBLatencyTracers,
				TLBLatencyTracer{tracer: tracer, tlb: tlb})
			tracing.CollectTrace(tlb, tracer)
		}

		for _, tlb := range gpu.L1STLBs {
			tracer := tracing.NewAverageTimeTracer(
				func(task tracing.Task) bool {
					return task.Kind == "req_in"
				})
			r.TLBLatencyTracers = append(r.TLBLatencyTracers,
				TLBLatencyTracer{tracer: tracer, tlb: tlb})
			tracing.CollectTrace(tlb, tracer)
		}

		for _, tlb := range gpu.L1VTLBs {
			tracer := tracing.NewAverageTimeTracer(
				func(task tracing.Task) bool {
					return task.Kind == "req_in"
				})
			r.TLBLatencyTracers = append(r.TLBLatencyTracers,
				TLBLatencyTracer{tracer: tracer, tlb: tlb})
			tracing.CollectTrace(tlb, tracer)
		}

		for _, tlb := range gpu.L2TLBs {
			tracer := tracing.NewAverageTimeTracer(
				func(task tracing.Task) bool {
					return task.Kind == "req_in"
				})
			r.TLBLatencyTracers = append(r.TLBLatencyTracers,
				TLBLatencyTracer{tracer: tracer, tlb: tlb})
			tracing.CollectTrace(tlb, tracer)
		}

		// for _, tlb := range gpu.L2TLBs {
		// 	pipeline := tlb.GetPipeline()
		// 	tracer := tracing.NewAverageTimeTracer(
		// 		func(task tracing.Task) bool {
		// 			return task.Kind == "pipeline"
		// 		})
		// 	r.TLBPipelineLatencyTracers = append(r.TLBPipelineLatencyTracers,
		// 		TLBPipelineLatencyTracer{tracer: tracer, pipeline: pipeline})
		// 	tracing.CollectTrace(pipeline, tracer)
		// }

		numL1VTLBs := len(gpu.L1VTLBs) // + len(gpu.L1STLBs) + len(gpu.L1ITLBs)
		allL1TLBs := make([]*tlb.TLB, numL1VTLBs)
		_ = copy(allL1TLBs, gpu.L1VTLBs)
		allL1TLBs = append(allL1TLBs, gpu.L1STLBs...)
		allL1TLBs = append(allL1TLBs, gpu.L1ITLBs...)
		for _, tlb := range allL1TLBs {
			tracer := tracing.NewAverageTimeTracer(
				func(task tracing.Task) bool {
					return task.Kind == "req_out"
				})
			r.DownTLBLatencyTracers = append(r.DownTLBLatencyTracers,
				TLBLatencyTracer{tracer: tracer, tlb: tlb})
			tracing.CollectTrace(tlb, tracer)
		}

		for _, tlb := range gpu.L2TLBs {
			tracer := tracing.NewAverageTimeTracer(
				func(task tracing.Task) bool {
					return task.Kind == "req_out"
				})
			r.DownTLBLatencyTracers = append(r.DownTLBLatencyTracers,
				TLBLatencyTracer{tracer: tracer, tlb: tlb})
			tracing.CollectTrace(tlb, tracer)
		}

	}
}

func (r *Runner) addTLBConditionalTracer() {
	if !r.ReportTLBConditionalStats {
		return
	}

	for _, gpu := range r.GPUDriver.GPUs {
		numL1VTLBs := len(gpu.L1VTLBs) // + len(gpu.L1STLBs) + len(gpu.L1ITLBs)
		allL1TLBs := make([]*tlb.TLB, numL1VTLBs)
		_ = copy(allL1TLBs, gpu.L1VTLBs)
		allL1TLBs = append(allL1TLBs, gpu.L1STLBs...)
		allL1TLBs = append(allL1TLBs, gpu.L1ITLBs...)
		for _, tlb := range allL1TLBs {
			tracer := tracing.NewConditionalAverageTimeTracer(
				func(task tracing.Task) bool {
					return task.Kind == "L2TLB_stats" // || task.Kind == "local_tlb"
				})
			r.TLBConditionalLatencyTracers = append(r.TLBConditionalLatencyTracers,
				TLBConditionalLatencyTracer{tracer: tracer, tlb: tlb})
			tracing.CollectTrace(tlb, tracer)
		}
	}
}

func (r *Runner) addTLBCoalesceTracer() {
	if !r.ReportTLBCoalesce {
		return
	}

	for _, gpu := range r.GPUDriver.GPUs {
		for _, tlb := range gpu.L2TLBs {
			tracer := tracing.NewAverageCountTracer(
				func(task tracing.Task) bool {
					return task.Kind == "buflen"
				})
			r.L2TLBBufLenTracers = append(r.L2TLBBufLenTracers, tracer)
			tracing.CollectTrace(tlb, tracer)
		}
		for _, tlb := range gpu.L2TLBs {
			tracer := tracing.NewAverageCountTracer(
				func(task tracing.Task) bool {
					return task.Kind == "bufleng0"
				})
			r.L2TLBBufLenG0Tracers = append(r.L2TLBBufLenG0Tracers, tracer)
			tracing.CollectTrace(tlb, tracer)
		}
		for _, tlb := range gpu.L2TLBs {
			tracer := tracing.NewAverageCountTracer(
				func(task tracing.Task) bool {
					return task.Kind == "coalesceaddr"
				})
			r.L2TLBCoalesceAddrTracers = append(r.L2TLBCoalesceAddrTracers, tracer)
			tracing.CollectTrace(tlb, tracer)
		}
		for _, tlb := range gpu.L2TLBs {
			tracer := tracing.NewAverageCountTracer(
				func(task tracing.Task) bool {
					return task.Kind == "coalesce"
				})
			r.L2TLBCoalesceTracers = append(r.L2TLBCoalesceTracers, tracer)
			tracing.CollectTrace(tlb, tracer)
		}
	}
}

func (r *Runner) addL2TLBMSHRLenTracer() {
	if !r.ReportL2TLBMSHRLen {
		return
	}

	for _, gpu := range r.GPUDriver.GPUs {
		for _, tlb := range gpu.L2TLBs {
			tracer := tracing.NewAverageCountTracer(
				func(task tracing.Task) bool {
					return task.Kind == "MSHRlen"
				})
			r.L2TLBMSHRLenTracers = append(r.L2TLBMSHRLenTracers, tracer)
			tracing.CollectTrace(tlb, tracer)
		}
		for _, tlb := range gpu.L2TLBs {
			tracer := tracing.NewAverageCountTracer(
				func(task tracing.Task) bool {
					return task.Kind == "MSHRlen_g0"
				})
			r.L2TLBMSHRLenG0Tracers = append(r.L2TLBMSHRLenG0Tracers, tracer)
			tracing.CollectTrace(tlb, tracer)
		}
		for _, tlb := range gpu.L2TLBs {
			tracer := tracing.NewAverageCountTracer(
				func(task tracing.Task) bool {
					return task.Kind == "MSHRuniq"
				})
			r.L2TLBMSHRUniqLenTracers = append(r.L2TLBMSHRUniqLenTracers, tracer)
			tracing.CollectTrace(tlb, tracer)
		}
		for _, tlb := range gpu.L2TLBs {
			tracer := tracing.NewAverageCountTracer(
				func(task tracing.Task) bool {
					return task.Kind == "MSHRuniq_g0"
				})
			r.L2TLBMSHRUniqLenG0Tracers = append(r.L2TLBMSHRUniqLenG0Tracers, tracer)
			tracing.CollectTrace(tlb, tracer)
		}
	}
}

func getCoalescingTracer(prefix string, statName string) *tracing.AverageCountTracer {
	return tracing.NewAverageCountTracer(func(task tracing.Task) bool {
		return strings.Contains(task.Kind, prefix+"-"+statName)
	})
}

func (r *Runner) addAddressTranslatorLatencyTracer() {
	if !r.ReportAddressTranslatorLatency {
		return
	}
	for _, gpu := range r.GPUDriver.GPUs {
		for _, l1vAddrTrans := range gpu.L1VAddrTranslator {
			tracer := tracing.NewConditionalAverageTimeTracer(
				func(task tracing.Task) bool {
					return task.Kind == "addr-translator-stats" // || task.Kind == "local_tlb"
				})
			tracing.CollectTrace(l1vAddrTrans, tracer)
			r.AddressTranslatorLatencyTracers = append(r.AddressTranslatorLatencyTracers,
				AddressTranslatorLatencyTracer{tracer: tracer, addressTranslator: l1vAddrTrans})
		}
	}
	for _, gpu := range r.GPUDriver.GPUs {
		for _, l1sAddrTrans := range gpu.L1SAddrTranslator {
			tracer := tracing.NewConditionalAverageTimeTracer(
				func(task tracing.Task) bool {
					return task.Kind == "addr-translator-stats" // || task.Kind == "local_tlb"
				})

			tracing.CollectTrace(l1sAddrTrans, tracer)
			r.AddressTranslatorLatencyTracers = append(r.AddressTranslatorLatencyTracers,
				AddressTranslatorLatencyTracer{tracer: tracer, addressTranslator: l1sAddrTrans})
		}
	}
	for _, gpu := range r.GPUDriver.GPUs {
		for _, l1iAddrTrans := range gpu.L1IAddrTranslator {
			tracer := tracing.NewConditionalAverageTimeTracer(
				func(task tracing.Task) bool {
					return task.Kind == "addr-translator-stats" // || task.Kind == "local_tlb"
				})
			tracing.CollectTrace(l1iAddrTrans, tracer)
			r.AddressTranslatorLatencyTracers = append(r.AddressTranslatorLatencyTracers,
				AddressTranslatorLatencyTracer{tracer: tracer, addressTranslator: l1iAddrTrans})
		}
	}
}

func (r *Runner) addPageWalkLatencyTracer() {
	if !r.ReportPageWalkLatency {
		return
	}
	for _, gpu := range r.GPUDriver.GPUs {
		for _, mmu := range gpu.MMUs {
			tracer := tracing.NewAverageTimeTracer(
				func(task tracing.Task) bool {
					return task.Kind == "req_in"
				})
			r.PageWalkLatencyTracers = append(r.PageWalkLatencyTracers,
				PageWalkLatencyTracer{tracer: tracer, mmu: mmu})
			tracing.CollectTrace(mmu, tracer)
		}
	}
}

func (r *Runner) addMMUConditionalTracer() {
	if !r.ReportMMUConditionalStats {
		return
	}
	for _, gpu := range r.GPUDriver.GPUs {
		for _, mmu := range gpu.MMUs {
			tracer := tracing.NewConditionalAverageTimeTracer(
				func(task tracing.Task) bool {
					return task.Kind == "MMU_mem_latency"
				})
			r.MMUMemoryLatencyTracers = append(r.MMUMemoryLatencyTracers,
				MMUMemoryLatencyTracer{tracer: tracer, mmu: mmu})
			tracing.CollectTrace(mmu, tracer)
		}
	}
}

func (r *Runner) addActiveWalkerTracer() {
	if !r.ReportActiveWalkerCount {
		return
	}

	for _, gpu := range r.GPUDriver.GPUs {
		for _, mmu := range gpu.MMUs {
			tracer := tracing.NewAverageCountTracer(
				func(task tracing.Task) bool {
					return task.Kind == "num_active_walkers"
				})
			r.ActivePageWalkerTracers = append(r.ActivePageWalkerTracers,
				ActivePageWalkerTracer{tracer: tracer, mmu: mmu})
			tracing.CollectTrace(mmu, tracer)
		}
	}

}

func (r *Runner) addDRAMLatencyTracer() {
	if !r.ReportDRAMLatency {
		return
	}

	for _, gpu := range r.GPUDriver.GPUs {
		for _, dram := range gpu.MemoryControllers {
			tracer := tracing.NewAverageTimeTracer(
				func(task tracing.Task) bool {
					return task.Kind == "req_in"
				})
			r.DRAMLatencyTracers = append(r.DRAMLatencyTracers,
				DRAMLatencyTracer{tracer: tracer, dram: dram})
			tracing.CollectTrace(dram, tracer)
		}

	}
}

func (r *Runner) addPWCHitRateTracer() {
	if !r.ReportPWCHitRate {
		return
	}

	for _, gpu := range r.GPUDriver.GPUs {
		for _, mmu := range gpu.MMUs {
			tracer := tracing.NewStepCountTracer(
				func(task tracing.Task) bool { return task.ID != 0 })
			r.PWCHitRateTracers = append(r.PWCHitRateTracers,
				PWCHitRateTracer{tracer: tracer, mmu: mmu})
			tracing.CollectTrace(mmu, tracer)
		}
	}
}

func (r *Runner) addCacheHitRateTracer() {
	if !r.ReportCacheHitRate {
		return
	}

	for _, gpu := range r.GPUDriver.GPUs {
		for _, cache := range gpu.L1VCaches {
			tracer := tracing.NewStepCountTracer(
				//	func(task tracing.Task) bool { return true })
				func(task tracing.Task) bool {
					return task.Kind == "l1_transaction"
				})
			r.CacheHitRateTracers = append(r.CacheHitRateTracers,
				cacheHitRateTracer{tracer: tracer, cache: cache})
			tracing.CollectTrace(cache, tracer)
		}

		for _, cache := range gpu.L1SCaches {
			tracer := tracing.NewStepCountTracer(
				//func(task tracing.Task) bool { return true })
				func(task tracing.Task) bool {
					return task.Kind == "l1_transaction"
				})
			r.CacheHitRateTracers = append(r.CacheHitRateTracers,
				cacheHitRateTracer{tracer: tracer, cache: cache})
			tracing.CollectTrace(cache, tracer)
		}

		for _, cache := range gpu.L1ICaches {
			tracer := tracing.NewStepCountTracer(
				//func(task tracing.Task) bool { return true })
				func(task tracing.Task) bool {
					return task.Kind == "l1_transaction"
				})
			r.CacheHitRateTracers = append(r.CacheHitRateTracers,
				cacheHitRateTracer{tracer: tracer, cache: cache})
			tracing.CollectTrace(cache, tracer)
		}

		for _, cache := range gpu.L2Caches {
			tracer := tracing.NewStepCountTracer(
				func(task tracing.Task) bool {
					return task.Kind == "req_in"
				})
			r.CacheHitRateTracers = append(r.CacheHitRateTracers,
				cacheHitRateTracer{tracer: tracer, cache: cache})
			tracing.CollectTrace(cache, tracer)
		}
	}
}

func (r *Runner) addTLBHitRateTracer() {
	if !r.ReportTLBHitRate {
		return
	}

	for _, gpu := range r.GPUDriver.GPUs {
		for _, tlb := range gpu.L1VTLBs {
			tracer := tracing.NewStepCountTracer(
				func(task tracing.Task) bool { /*return true*/
					return task.Kind == "req_in"
				})
			r.TLBHitRateTracers = append(r.TLBHitRateTracers,
				TLBHitRateTracer{tracer: tracer, tlb: tlb})
			tracing.CollectTrace(tlb, tracer)
		}

		for _, tlb := range gpu.L1STLBs {
			tracer := tracing.NewStepCountTracer(
				func(task tracing.Task) bool { /*return true })*/
					return task.Kind == "req_in"
				})
			r.TLBHitRateTracers = append(r.TLBHitRateTracers,
				TLBHitRateTracer{tracer: tracer, tlb: tlb})
			tracing.CollectTrace(tlb, tracer)
		}

		for _, tlb := range gpu.L1ITLBs {
			tracer := tracing.NewStepCountTracer(
				func(task tracing.Task) bool { /*return true })*/
					return task.Kind == "req_in"
				})
			r.TLBHitRateTracers = append(r.TLBHitRateTracers,
				TLBHitRateTracer{tracer: tracer, tlb: tlb})
			tracing.CollectTrace(tlb, tracer)
		}

		for _, tlb := range gpu.L2TLBs {
			tracer := tracing.NewStepCountTracer(
				func(task tracing.Task) bool { return true })
			r.TLBHitRateTracers = append(r.TLBHitRateTracers,
				TLBHitRateTracer{tracer: tracer, tlb: tlb})
			tracing.CollectTrace(tlb, tracer)
		}

	}
}

func (r *Runner) addRDMAEngineTracer() {
	if !r.ReportRDMATransactionCount {
		return
	}

	for _, gpu := range r.GPUDriver.GPUs {
		t := rdmaTransactionCountTracer{}
		t.rdmaEngine = gpu.RDMAEngine
		t.incomingTracer = tracing.NewAverageTimeTracer(
			func(task tracing.Task) bool {
				if task.Kind != "req_in" {
					return false
				}

				isFromOutside := strings.Contains(
					task.Detail.(akita.Msg).Meta().Src.Name(), "RDMA")
				if !isFromOutside {
					return false
				}

				return true
			})
		t.outgoingTracer = tracing.NewAverageTimeTracer(
			func(task tracing.Task) bool {
				if task.Kind != "req_in" {
					return false
				}

				isFromOutside := strings.Contains(
					task.Detail.(akita.Msg).Meta().Src.Name(), "RDMA")
				if isFromOutside {
					return false
				}

				return true
			})

		tracing.CollectTrace(t.rdmaEngine, t.incomingTracer)
		tracing.CollectTrace(t.rdmaEngine, t.outgoingTracer)

		r.RDMATransactionCounters = append(r.RDMATransactionCounters, t)
	}
}

func (r *Runner) addCDMAEngineTracer() {
	if !r.ReportCDMATransactionCount {
		return
	}

	for _, gpu := range r.GPUDriver.GPUs {
		for _, cdmaEngine := range gpu.ChipRDMAEngines {
			t := rdmaTransactionCountTracer{}
			t.rdmaEngine = cdmaEngine
			t.incomingTracer = tracing.NewAverageTimeTracer(
				func(task tracing.Task) bool {
					if task.Kind != "req_in" {
						return false
					}

					isFromOutside := strings.Contains(
						task.Detail.(akita.Msg).Meta().Src.Name(), "RDMA")
					if !isFromOutside {
						return false
					}

					return true
				})
			t.outgoingTracer = tracing.NewAverageTimeTracer(
				func(task tracing.Task) bool {
					if task.Kind != "req_in" {
						return false
					}

					isFromOutside := strings.Contains(
						task.Detail.(akita.Msg).Meta().Src.Name(), "RDMA")
					if isFromOutside {
						return false
					}

					return true
				})

			tracing.CollectTrace(t.rdmaEngine, t.incomingTracer)
			tracing.CollectTrace(t.rdmaEngine, t.outgoingTracer)

			r.CDMATransactionCounters = append(r.CDMATransactionCounters, t)

			tracer := tracing.NewStepCountTracer(
				func(task tracing.Task) bool { /*return true*/
					return task.Kind == "req_in"
				})
			r.CDMAAccessTracers = append(r.CDMAAccessTracers,
				CDMAAccessTracer{tracer: tracer, cdmaEngine: cdmaEngine})
			tracing.CollectTrace(cdmaEngine, tracer)

		}
	}

	for _, gpu := range r.GPUDriver.GPUs {
		for _, cdmaEngine := range gpu.PageRDMAEngines {
			t := rdmaTransactionCountTracer{}
			t.rdmaEngine = cdmaEngine
			t.incomingTracer = tracing.NewAverageTimeTracer(
				func(task tracing.Task) bool {
					if task.Kind != "req_in" {
						return false
					}

					isFromOutside := strings.Contains(
						task.Detail.(akita.Msg).Meta().Src.Name(), "PageRDMA")
					if !isFromOutside {
						return false
					}

					return true
				})
			t.outgoingTracer = tracing.NewAverageTimeTracer(
				func(task tracing.Task) bool {
					if task.Kind != "req_in" {
						return false
					}

					isFromOutside := strings.Contains(
						task.Detail.(akita.Msg).Meta().Src.Name(), "PageRDMA")
					if isFromOutside {
						return false
					}

					return true
				})

			tracing.CollectTrace(t.rdmaEngine, t.incomingTracer)
			tracing.CollectTrace(t.rdmaEngine, t.outgoingTracer)

			r.PageTransactionCounters = append(r.PageTransactionCounters, t)

			tracer := tracing.NewStepCountTracer(
				func(task tracing.Task) bool { /*return true*/
					return task.Kind == "req_in"
				})
			r.PageAccessTracers = append(r.PageAccessTracers,
				CDMAAccessTracer{tracer: tracer, cdmaEngine: cdmaEngine})
			tracing.CollectTrace(cdmaEngine, tracer)

		}
	}
}

func (r *Runner) addDRAMTracer() {
	if !r.ReportDRAMTransactionCount {
		return
	}

	for _, gpu := range r.GPUDriver.GPUs {
		for _, dram := range gpu.MemoryControllers {
			t := dramTransactionCountTracer{}
			// t.dram = dram.(*idealmemcontroller.Comp)
			// t.dram = dram.(*ram.MemController)
			t.dram = dram
			t.tracer = tracing.NewAverageTimeTracer(
				func(task tracing.Task) bool {
					return true
				})

			tracing.CollectTrace(t.dram, t.tracer)

			r.DRAMTransactionCounters = append(r.DRAMTransactionCounters, t)
		}
	}
}

func (r *Runner) addL2TLBSQLTracer() {
	if !r.L2TLBSQLTracing {
		return
	}
	tracer := tracing.NewMySQLTracer()
	tracer.Init()
	for _, gpu := range r.GPUDriver.GPUs {
		for _, l2tlb := range gpu.L2TLBs {
			tracing.CollectTrace(l2tlb, tracer)
		}
	}
	for _, gpu := range r.GPUDriver.GPUs {
		for _, l1tlb := range gpu.L1VTLBs {
			tracing.CollectTrace(l1tlb, tracer)
		}
	}
	for _, gpu := range r.GPUDriver.GPUs {
		for _, l1tlb := range gpu.L1ITLBs {
			tracing.CollectTrace(l1tlb, tracer)
		}
	}
	for _, gpu := range r.GPUDriver.GPUs {
		for _, l1tlb := range gpu.L1STLBs {
			tracing.CollectTrace(l1tlb, tracer)
		}
	}
}

func (r *Runner) addTranslationReqTracer() {
	if !r.ReportTranslationReqLatency {
		return
	}
	tracer := tracing.NewTranslationReqTracer(
		func(task tracing.Task) bool {
			if task.Kind == "trace-trans-req" {
				return true
			}
			return false
		})
	r.TranslationReqTracer = tracer
	for _, gpu := range r.GPUDriver.GPUs {
		for _, l2tlb := range gpu.L2TLBs {
			tracing.CollectTrace(l2tlb, tracer)
		}
	}
	for _, gpu := range r.GPUDriver.GPUs {
		for _, l1tlb := range gpu.L1VTLBs {
			tracing.CollectTrace(l1tlb, tracer)
		}
	}
	for _, gpu := range r.GPUDriver.GPUs {
		for _, l1tlb := range gpu.L1ITLBs {
			tracing.CollectTrace(l1tlb, tracer)
		}
	}
	for _, gpu := range r.GPUDriver.GPUs {
		for _, l1tlb := range gpu.L1STLBs {
			tracing.CollectTrace(l1tlb, tracer)
		}
	}

	for _, gpu := range r.GPUDriver.GPUs {
		for _, l1vAddrTrans := range gpu.L1VAddrTranslator {
			tracing.CollectTrace(l1vAddrTrans, tracer)
		}
	}
	for _, gpu := range r.GPUDriver.GPUs {
		for _, l1sAddrTrans := range gpu.L1SAddrTranslator {
			tracing.CollectTrace(l1sAddrTrans, tracer)
		}
	}
	for _, gpu := range r.GPUDriver.GPUs {
		for _, l1iAddrTrans := range gpu.L1IAddrTranslator {
			tracing.CollectTrace(l1iAddrTrans, tracer)
		}
	}
}

func (r *Runner) addL2TLBQueueingImbalanceTracer() {
	if !r.ReportL2TLBQueueImbalance {
		return
	}
	for _, gpu := range r.GPUDriver.GPUs {
		for _, this := range gpu.L2TLBs {
			tracer := tlb.NewGlobalTLBQueueingTracer(
				func(task tracing.Task) bool {
					if task.Kind == "imbalance" {
						return true
					}
					return false
				})
			tracer.AddThis(this)
			for _, tlb := range gpu.L2TLBs {
				tracer.AddTLB(tlb)
			}
			tracing.CollectTrace(this, tracer)
			r.L2TLBQueueingImbalanceTracers = append(r.L2TLBQueueingImbalanceTracers,
				TLBQueueImbalanceTracer{tracer: tracer, tlb: this})
		}
	}
}

func (r *Runner) addL2TLBEntropyTracer() {
	if !r.ReportEntropy {
		return
	}
	for _, gpu := range r.GPUDriver.GPUs {
		for _, this := range gpu.L2TLBs {
			tracer := tlb.NewEntropyTracer(
				func(task tracing.Task) bool {
					if task.Kind == "entropy" {
						return true
					}
					return false
				})
			tracing.CollectTrace(this, tracer)
			r.EntropyTracers = append(r.EntropyTracers,
				EntropyTracer{tracer: tracer, tlb: this})
		}
	}
}

func (r *Runner) addTLBSetMissTracer() {
	if !r.ReportTLBSetMissTracing {
		return
	}
	for _, gpu := range r.GPUDriver.GPUs {
		for _, tlb := range gpu.L2TLBs {
			tracer := tracing.NewReferenceTracer(
				func(task tracing.Task) bool {
					if task.Kind == "set_miss_tracing" {
						return true
					}
					return false
				})
			tracing.CollectTrace(tlb, tracer)
			r.TLBSetMissTracers = append(r.TLBSetMissTracers,
				TLBSetMissTracer{tracer: tracer, tlb: tlb})
		}
	}
}

func (r *Runner) addTLBMSHRStallTracer() {
	if !r.ReportTLBMSHRStallTracing {
		return
	}
	for _, gpu := range r.GPUDriver.GPUs {
		for _, tlb := range gpu.L2TLBs {
			tracer := tracing.NewTotalTimeTracer(
				func(task tracing.Task) bool {
					if task.Kind == "mshr_stall" {
						return true
					}
					return false
				})
			tracing.CollectTrace(tlb, tracer)
			r.TLBMSHRStallTracers = append(r.TLBMSHRStallTracers,
				TLBMSHRStallTracer{tracer: tracer, tlb: tlb})
		}
	}
}

func (r *Runner) addTLBReqStallTracer() {
	if !r.ReportTLBReqStalls {
		return
	}
	for _, gpu := range r.GPUDriver.GPUs {
		for _, tlb := range gpu.L2TLBs {
			tracer := tracing.NewStepCountTracer(
				func(task tracing.Task) bool {
					return task.Kind == "stalled-l2-tlb-req-count" || task.Kind == "l2-tlb-req-count"
				})
			tracing.CollectTrace(tlb, tracer)
			r.TLBReqStallTracers = append(r.TLBReqStallTracers,
				TLBReqStallTracer{tracer: tracer, tlb: tlb})
		}
	}
}

func (r *Runner) addAccessCountPerAddrTracer() {
	if !r.TraceAccessCountPerAddrs {
		return
	}
	tracer := tracing.NewAccessCountTracer()

	r.AccessCountPerAddrTracer = tracer

	for _, gpu := range r.GPUDriver.GPUs {
		// for _, cache := range gpu.L1ICaches {
		// 	tracing.CollectTrace(cache, tracer)
		// }

		for _, cache := range gpu.L1SCaches {
			tracing.CollectTrace(cache, tracer)
		}

		for _, cache := range gpu.L1VCaches {
			tracing.CollectTrace(cache, tracer)
		}
	}
}

func (r *Runner) addEvictTracer() {
	r.EvictTracer = tracing.NewEvictTracer(*xDim * *yDim)

	for _, gpu := range r.GPUDriver.GPUs {
		for _, l2Cache := range gpu.L2Caches {
			tracing.CollectTrace(l2Cache, r.EvictTracer)
		}
	}
}

func (r *Runner) addWorkgroupCountTracer() {
	if !r.ReportWorkgroupCount {
		return
	}

	perKernelTracer := tracing.NewWorkgroupCountPerKernelTracer(
		func(task tracing.Task) bool {
			return task.Kind == "workgroup-per-kernel"
		})
	r.WorkgroupCountPerKernelTracer = perKernelTracer
	for _, gpu := range r.GPUDriver.GPUs {
		for _, dispatcher := range gpu.CommandProcessor.Dispatchers {
			tracing.CollectTrace(dispatcher, perKernelTracer)
		}
	}

	perChipletTracer := tracing.NewWorkgroupCountPerChipletTracer(
		*xDim**yDim, // number of chiplets
		func(task tracing.Task) bool {
			return task.Kind == "workgroup-per-chiplet"
		})
	r.WorkgroupCountPerChipletTracer = perChipletTracer
	for _, gpu := range r.GPUDriver.GPUs {
		for _, dispatcher := range gpu.CommandProcessor.Dispatchers {
			tracing.CollectTrace(dispatcher, perChipletTracer)
		}
	}
}

func (r *Runner) addRemoteRtwTracer() {
	if !r.ReportHopPerRDMA {
		return
	}

	r.RemotePtwTracer = tracing.NewRemotePtwTracer(
		func(task tracing.Task) bool {
			return task.Kind == "remote-ptw-trace"
		},
		*xDim,
		*yDim)

	for _, gpu := range r.GPUDriver.GPUs {
		for _, engine := range gpu.ChipRDMAEngines {
			tracing.CollectTrace(engine, r.RemotePtwTracer)
		}
		for _, net := range gpu.InterChipletNetwork {
			for _, swArr := range net.GetChipSwitches() {
				for _, sw := range swArr {
					tracing.CollectTrace(sw, r.RemotePtwTracer)
				}
			}
		}
	}
}

func (r *Runner) addHopTracer() {
	// Attach unconditionally: IBPA (needs avg-hop-traced) is reported on every
	// platform regardless of -report-hop-count-per-rdma. The flag only gates
	// the verbose per-hop CSV breakdown in reportHopPerRDMA.

	r.HopTracer = tracing.NewHopTracer(
		func(task tracing.Task) bool {
			return task.Kind == "hop-trace"
		},
		*xDim,
		*yDim)

	for _, gpu := range r.GPUDriver.GPUs {
		for _, engine := range gpu.ChipRDMAEngines {
			tracing.CollectTrace(engine, r.HopTracer)
		}
		for _, net := range gpu.InterChipletNetwork {
			for _, swArr := range net.GetChipSwitches() {
				for _, sw := range swArr {
					tracing.CollectTrace(sw, r.HopTracer)
				}
			}
		}
	}
}

func (r *Runner) addRDMAFlightTracer() {
	if !r.ReportHopPerRDMA {
		return
	}

	// NetworkFlightTracer: endpoint↔endpoint window. Tasks are emitted
	// by EndPoint.Send (source) and EndPoint.tryDeliver (dest) in
	// noc/networking/switching/endpoint.go.
	r.NetworkFlightTracer = tracing.NewNetworkFlightTracer(
		func(task tracing.Task) bool {
			return task.Kind == tracing.NetworkFlightKindReq ||
				task.Kind == tracing.NetworkFlightKindRsp
		},
		*xDim,
		*yDim)

	// RDMAFlightTracer: rdma↔rdma window. Tasks are emitted by
	// RDMAEngineImpl at each outbound Send-success site (StartTask) and
	// each inbound Retrieve site (EndTask) — see
	// mgpusim/rdma/flight_tracing.go for the helper definitions.
	r.RDMAFlightTracer = tracing.NewRDMAFlightTracer(
		func(task tracing.Task) bool {
			return task.Kind == tracing.RDMAFlightKindReq ||
				task.Kind == tracing.RDMAFlightKindRsp
		},
		*xDim,
		*yDim)

	for _, gpu := range r.GPUDriver.GPUs {
		for _, net := range gpu.InterChipletNetwork {
			sc := net.GetSwitchConnector()
			if sc == nil {
				continue
			}
			for _, ep := range sc.GetEndPoints() {
				tracing.CollectTrace(ep, r.NetworkFlightTracer)
			}
		}
		for _, engine := range gpu.ChipRDMAEngines {
			tracing.CollectTrace(engine, r.RDMAFlightTracer)
		}
	}
}

func (r *Runner) addSendOutFailTracer() {
	if !r.ReportHopPerRDMA {
		return
	}

	r.SendOutFailTracer = tracing.NewSendOutFailTracer(
		func(task tracing.Task) bool {
			return task.Kind == tracing.SendOutFailKind
		})

	for _, gpu := range r.GPUDriver.GPUs {
		for _, net := range gpu.InterChipletNetwork {
			for _, swArr := range net.GetChipSwitches() {
				for _, sw := range swArr {
					tracing.CollectTrace(sw, r.SendOutFailTracer)
				}
			}
		}
	}
}

func (r *Runner) addPortQueueTracer() {
	if !r.TracePortQueue {
		return
	}

	r.PortQueueTracer = tracing.NewPortQueueTracer()

	for _, gpu := range r.GPUDriver.GPUs {
		r.attachPortQueueToRDMA(gpu.ChipRDMAEngines)
		r.attachPortQueueToEndpoints(gpu.InterChipletNetwork)
		r.attachPortQueueToChipletSwitches(gpu.InterChipletNetwork)
		r.attachPortQueueToL2(gpu.L2Caches)
		r.attachPortQueueToCUs(gpu.CUs)
		r.attachPortQueueToROBs(gpu.L1VROBs)
		r.attachPortQueueToROBs(gpu.L1SROBs)
		r.attachPortQueueToROBs(gpu.L1IROBs)
		// Address-translation port-queue tracing disabled.
		// r.attachPortQueueToAddrTranslators(gpu.L1VAddrTranslator)
		// r.attachPortQueueToAddrTranslators(gpu.L1SAddrTranslator)
		// r.attachPortQueueToAddrTranslators(gpu.L1IAddrTranslator)
		// r.attachPortQueueToL1TLBs(gpu.L1VTLBs)
		// r.attachPortQueueToL1TLBs(gpu.L1STLBs)
		// r.attachPortQueueToL1TLBs(gpu.L1ITLBs)
		// r.attachPortQueueToL2TLBs(gpu.L2TLBs)
		// r.attachPortQueueToMMUs(gpu.MMUs)
		r.attachPortQueueToL1Caches(gpu.L1VCaches)
		r.attachPortQueueToL1Caches(gpu.L1SCaches)
		r.attachPortQueueToL1Caches(gpu.L1ICaches)
		r.attachPortQueueToL2TopPorts(gpu.L2Caches)
		r.attachPortQueueToDRAM(gpu.MemoryControllers)
	}
}

func (r *Runner) attachPortQueueToL2(caches []cache.Cache) {
	for _, c := range caches {
		r.attachIfPort(c.GetBottomToRdmaPort())
		r.attachIfPort(c.GetBottomToDramPort())
		r.attachIfPort(c.GetRDMAPort())
	}
}

func (r *Runner) attachPortQueueToRDMA(engines []rdma.RDMAEngine) {
	for _, e := range engines {
		r.attachIfPort(e.GetRequestPort())
		r.attachIfPort(e.GetResponsePort())
		r.attachIfPort(e.GetToL2RDMA())
		r.attachIfPort(e.GetToL2Bottom())
		r.attachIfPort(e.GetToDramTop())
		r.attachIfPort(e.GetPwPort())
		r.attachIfPort(e.GetCtrlPort())
	}
}

func (r *Runner) attachPortQueueToEndpoints(
	nets []chipnetwork.InterChipletConnector,
) {
	for _, net := range nets {
		conn := net.GetSwitchConnector()
		if conn == nil {
			continue
		}
		for _, ep := range conn.GetEndPoints() {
			r.attachIfPort(ep.NetworkPort)
		}
	}
}

// attachPortQueueToChipletSwitches walks every multi-flit chiplet switch
// in the mesh (one per chiplet) and attaches the PortQueueTracer to each
// local port (north/south/east/west/local plus any RDMA-facing ports).
// Surfaces back-pressure inside the switching fabric itself, separate
// from the endpoint NetworkPorts attached above.
func (r *Runner) attachPortQueueToChipletSwitches(
	nets []chipnetwork.InterChipletConnector,
) {
	for _, net := range nets {
		for _, row := range net.GetChipSwitches() {
			for _, sw := range row {
				if sw == nil {
					continue
				}
				for _, p := range sw.GetPorts() {
					r.attachIfPort(p)
				}
			}
		}
	}
}

func (r *Runner) attachIfPort(port akita.Port) {
	if port == nil {
		return
	}
	hookable, ok := port.(tracing.NamedHookable)
	if !ok {
		return
	}
	r.PortQueueTracer.AttachTo(hookable)
}

func (r *Runner) addL2MissTracer() {
	if !r.ReportL2MissTrace {
		return
	}

	// l2-miss tracing is currently disabled — emission sites in
	// mem/cache/writeback/directorystage.go are commented out, so the
	// tracer would never receive an event. Re-enable both ends together
	// if you need it again.
	// r.L2MissTracer = tracing.NewL2MissTracer(
	// 	func(task tracing.Task) bool {
	// 		return task.Kind == "l2-miss-trace"
	// 	},
	// 	*xDim,
	// 	*yDim)

	// for _, gpu := range r.GPUDriver.GPUs {
	// 	for _, l2Cache := range gpu.L2Caches {
	// 		tracing.CollectTrace(l2Cache, r.L2MissTracer)
	// 	}
	// }
}

func (r *Runner) addMSHRFailTracer() {
	if !r.ReportMSHRFailTrace {
		return
	}

	r.L1MSHRFailTracer = tracing.NewMSHRFailTracer()
	r.L2MSHRFailTracer = tracing.NewMSHRFailTracer()

	for _, gpu := range r.GPUDriver.GPUs {
		for _, l2Cache := range gpu.L2Caches {
			tracing.CollectTrace(l2Cache, r.L2MSHRFailTracer)
		}
		for _, l1vCache := range gpu.L1VCaches {
			tracing.CollectTrace(l1vCache, r.L1MSHRFailTracer)
		}
	}
}

func (r *Runner) addTLBMSHRWaitTracer() {
	if !r.ReportTLBMSHRWait {
		return
	}

	for _, gpu := range r.GPUDriver.GPUs {
		allL1TLBs := make([]*tlb.TLB, 0, len(gpu.L1VTLBs)+len(gpu.L1STLBs)+len(gpu.L1ITLBs))
		allL1TLBs = append(allL1TLBs, gpu.L1VTLBs...)
		allL1TLBs = append(allL1TLBs, gpu.L1STLBs...)
		allL1TLBs = append(allL1TLBs, gpu.L1ITLBs...)
		for _, t := range allL1TLBs {
			tracer := tracing.NewTLBMSHRWaitTracer()
			tracing.CollectTrace(t, tracer)
			r.L1TLBMSHRWaitTracers = append(r.L1TLBMSHRWaitTracers,
				L1TLBMSHRWaitTracerEntry{tracer: tracer, tlb: t})
		}
		for _, t := range gpu.L2TLBs {
			tracer := tracing.NewTLBMSHRWaitTracer()
			tracing.CollectTrace(t, tracer)
			r.L2TLBMSHRWaitTracers = append(r.L2TLBMSHRWaitTracers,
				L2TLBMSHRWaitTracerEntry{tracer: tracer, tlb: t})
		}
	}
}

func (r *Runner) attachPortQueueToCUs(cus []akita.Component) {
	for _, comp := range cus {
		cu, ok := comp.(*cupkg.ComputeUnit)
		if !ok {
			continue
		}
		r.attachIfPort(cu.ToInstMem)
		r.attachIfPort(cu.ToScalarMem)
		r.attachIfPort(cu.ToVectorMem)
	}
}

func (r *Runner) attachPortQueueToROBs(robs []*rob.ReorderBuffer) {
	for _, rb := range robs {
		r.attachIfPort(rb.TopPort)
		r.attachIfPort(rb.BottomPort)
	}
}

func (r *Runner) attachPortQueueToAddrTranslators(
	translators []addresstranslator.AddressTranslator,
) {
	for _, at := range translators {
		r.attachIfPort(at.GetTopPort())
		// IDEAL-AT has no translation port (resolves inline) — GetTranslationPort panics.
		if _, ok := at.(*addresstranslator.IdealAddressTranslator); !ok {
			r.attachIfPort(at.GetTranslationPort())
		}
		r.attachIfPort(at.GetBottomPort())
	}
}

func (r *Runner) attachPortQueueToL1TLBs(tlbs []*tlb.TLB) {
	for _, t := range tlbs {
		r.attachIfPort(t.TopPort)
		r.attachIfPort(t.BottomPort)
	}
}

func (r *Runner) attachPortQueueToL2TLBs(l2tlbs []tlb.L2TLB) {
	for _, t := range l2tlbs {
		r.attachIfPort(t.GetTopPort())
		r.attachIfPort(t.GetBottomPort())
	}
}

func (r *Runner) attachPortQueueToMMUs(mmus []*mmu.MMUImpl) {
	for _, m := range mmus {
		r.attachIfPort(m.ToTop)
		r.attachIfPort(m.LocalTransPort)
		r.attachIfPort(m.RemoteTransPort)
	}
}

func (r *Runner) attachPortQueueToL1Caches(l1caches []caches.L1Cache) {
	for _, c := range l1caches {
		r.attachIfPort(c.GetTopPort())
		r.attachIfPort(c.GetBottomPort())
	}
}

func (r *Runner) attachPortQueueToL2TopPorts(l2caches []cache.Cache) {
	for _, c := range l2caches {
		r.attachIfPort(c.GetTopPort())
	}
}

func (r *Runner) attachPortQueueToDRAM(drms []*idealmemcontroller.Comp) {
	for _, d := range drms {
		r.attachIfPort(d.ToTop)
	}
}
