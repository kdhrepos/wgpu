// Package runner defines how default benchmark samples are executed.
package runner

import (
	"strings"

	"gitlab.com/akita/akita"
	"gitlab.com/akita/mem/vm/tlb"
	"gitlab.com/akita/util/tracing"
)

func (r *Runner) addLocalRemoteTracer() {
	r.LocalRemoteTracer = tracing.NewLocalRemoteTracer()

	for _, gpu := range r.GPUDriver.GPUs {
		for _, cache := range gpu.L2Caches {
			tracing.CollectTrace(cache, r.LocalRemoteTracer)
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
