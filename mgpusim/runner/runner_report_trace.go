// Package runner defines how default benchmark samples are executed.
package runner

import (
	"flag"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"gitlab.com/akita/akita"
	"gitlab.com/akita/mem/cache"
	"gitlab.com/akita/mem/cache/writeback"
	"gitlab.com/akita/noc/networking/switching"
	"gitlab.com/akita/util/tracing"
)

func (r *Runner) reportLocalRemoteCount() {
	localAccessCount := r.LocalRemoteTracer.GetLocalAccessCount()
	remoteAccessCount := r.LocalRemoteTracer.GetRemoteAccessCount()

	r.metricsCollector.Collect(
		"GPU1.chiplet_all.Caches",
		"total-l2-local-access-count",
		float64(localAccessCount))
	r.metricsCollector.Collect(
		"GPU1.chiplet_all.Caches",
		"total-l2-remote-access-count",
		float64(remoteAccessCount))
}

func (r *Runner) reportPteCaching() {
	readHitCount := r.PteCachingTracer.GetReadHitCount()
	readMissCount := r.PteCachingTracer.GetReadMissCount()
	readMshrHitCount := r.PteCachingTracer.GetReadMshrHitCount()

	r.metricsCollector.Collect(
		"GPU1.chiplet_all.Caches",
		"pte-caching-read-hit-count",
		float64(readHitCount))
	r.metricsCollector.Collect(
		"GPU1.chiplet_all.Caches",
		"pte-caching-read-miss-count",
		float64(readMissCount))
	r.metricsCollector.Collect(
		"GPU1.chiplet_all.Caches",
		"pte-caching-read-mshr-hit-count",
		float64(readMshrHitCount))

	remotePtwCount := r.PteCachingTracer.GetRemotePtwCount()
	localPtwCount := r.PteCachingTracer.GetLocalPtwCount()

	r.metricsCollector.Collect(
		"GPU1.chiplet_all.Caches",
		"pte-caching-remote-ptw-count",
		float64(remotePtwCount))
	r.metricsCollector.Collect(
		"GPU1.chiplet_all.Caches",
		"pte-caching-local-ptw-count",
		float64(localPtwCount))

	totalNumPerHopTraced := r.PteCachingTracer.GetNumPerHopTraced()
	totalNumPerHopRequired := r.PteCachingTracer.GetNumPerHopRequired()
	totalAvgLatencyPerHop := r.PteCachingTracer.GetAvgLatencyPerHopTraced()
	totalRemotePtwNum := r.PteCachingTracer.GetTotalRdmaReqs()
	totalC2CTraced := r.PteCachingTracer.GetC2CTraced()

	numChiplet := *xDim * *yDim

	r.metricsCollector.Collect(
		"GPU1.chiplet_all.ChipRDMA",
		"total-remote-ptw-num-pte-caching",
		float64(totalRemotePtwNum))

	for h, n := range totalNumPerHopRequired {
		r.metricsCollector.Collect(
			"GPU1.chiplet_all.ChipRDMA",
			"number-ptw-hop-required-"+strconv.Itoa(h),
			float64(n))
	}

	for h, n := range totalNumPerHopTraced {
		if n == 0 {
			continue
		}

		r.metricsCollector.Collect(
			"GPU1.chiplet_all.ChipRDMA",
			"number-ptw-hop-traced-"+strconv.Itoa(h),
			float64(n))
	}
	for h, l := range totalAvgLatencyPerHop {
		r.metricsCollector.Collect(
			"GPU1.chiplet_all.ChipRDMA",
			"average-latency-ptw-hop-traced-"+strconv.Itoa(h),
			l)
	}

	for srcChip := range numChiplet {
		for dstChip := range numChiplet {
			if totalC2CTraced[srcChip][dstChip] == 0 {
				continue
			}
			r.metricsCollector.Collect(
				"Chip-to-chip-ptw-traced",
				strconv.Itoa(srcChip)+"-"+strconv.Itoa(dstChip),
				float64(totalC2CTraced[srcChip][dstChip]))
		}
	}
}

func (r *Runner) reportMemAllocPerChiplet() {
	memAllocator := r.GPUDriver.GetMemAllocator()
	allocatedPagesPerChiplet := memAllocator.GetPageAllocatedPerChiplet()
	totalPagesAllocated := 0

	for c, count := range allocatedPagesPerChiplet {
		r.metricsCollector.Collect(
			r.GPUDriver.Name(),
			"pages-per-chiplet-"+strconv.Itoa(c),
			float64(count))

		totalPagesAllocated += count
	}

	r.metricsCollector.Collect(
		r.GPUDriver.Name(),
		"total-pages-allocated",
		float64(totalPagesAllocated))
}

func (r *Runner) reportNumFlits() {
	numFlits := r.NumFlitTracer.GetNumFlits()

	r.metricsCollector.Collect(
		r.GPUDriver.Name(),
		"num-flits",
		float64(numFlits))
}

func (r *Runner) reportFlushMetrics() {
	totalFlushes := r.FlushTracer.GetTotalFlushes()
	totalFlushTime := float64(0)

	for id, task := range totalFlushes {
		if task.EndTime == 0 {
			continue
		}
		totalFlushTime += float64(task.EndTime - task.StartTime)

		r.metricsCollector.Collect(
			"GPU1.chiplet_all.ChipRDMA",
			"flush-" + strconv.FormatUint(id, 10) + "-elapsed-time",
			float64(task.EndTime-task.StartTime))
	}

	r.metricsCollector.Collect(
		"GPU1.chiplet_all.ChipRDMA",
		"total-flush-time",
		totalFlushTime)
}

func (r *Runner) reportWorkgroupDimension() {
	numKernels := r.WorkgroupDimensionTracer.GetNumKernels()

	for i := 0; i < numKernels; i++ {
		r.metricsCollector.Collect(
			r.GPUDriver.Name(),
			"kernel-"+strconv.Itoa(i)+"-workgroup-dimension-grid-size-x",
			float64(r.WorkgroupDimensionTracer.GetGridSizeX(i)))
		r.metricsCollector.Collect(
			r.GPUDriver.Name(),
			"kernel-"+strconv.Itoa(i)+"-workgroup-dimension-grid-size-y",
			float64(r.WorkgroupDimensionTracer.GetGridSizeY(i)))
		r.metricsCollector.Collect(
			r.GPUDriver.Name(),
			"kernel-"+strconv.Itoa(i)+"-workgroup-dimension-grid-size-z",
			float64(r.WorkgroupDimensionTracer.GetGridSizeZ(i)))

		r.metricsCollector.Collect(
			r.GPUDriver.Name(),
			"kernel-"+strconv.Itoa(i)+"-workgroup-size-x",
			float64(r.WorkgroupDimensionTracer.GetWorkgroupX(i)))
		r.metricsCollector.Collect(
			r.GPUDriver.Name(),
			"kernel-"+strconv.Itoa(i)+"-workgroup-size-y",
			float64(r.WorkgroupDimensionTracer.GetWorkgroupY(i)))
		r.metricsCollector.Collect(
			r.GPUDriver.Name(),
			"kernel-"+strconv.Itoa(i)+"-workgroup-size-z",
			float64(r.WorkgroupDimensionTracer.GetWorkgroupZ(i)))

		r.metricsCollector.Collect(
			r.GPUDriver.Name(),
			"kernel-"+strconv.Itoa(i)+"-grid-dimension-x",
			float64(r.WorkgroupDimensionTracer.GetGridNumX(i)))
		r.metricsCollector.Collect(
			r.GPUDriver.Name(),
			"kernel-"+strconv.Itoa(i)+"-grid-dimension-y",
			float64(r.WorkgroupDimensionTracer.GetGridNumY(i)))
		r.metricsCollector.Collect(
			r.GPUDriver.Name(),
			"kernel-"+strconv.Itoa(i)+"-grid-dimension-z",
			float64(r.WorkgroupDimensionTracer.GetGridNumZ(i)))
	}
}

func (r *Runner) reportKernelTime() {
	if r.Timing {
		numKernels := r.KernelTracer.GetNumKernels()

		kernelLaunchTime := r.KernelTracer.GetKernelLaunchTimes()
		kernelTerminateTime := r.KernelTracer.GetKernelTerminateTimes()
		kernelBasePCList := r.KernelTracer.GetBasePCList()

		totalKernelTime := float64(0)

		for i := range numKernels {
			if i == numKernels-1 && terminated {
				kernelTerminateTime[i] = r.Engine.CurrentTime()
			}

			r.metricsCollector.Collect(
				r.GPUDriver.Name(),
				"kernel-launch-time-"+strconv.Itoa(i),
				float64(kernelLaunchTime[i]))
			r.metricsCollector.Collect(
				r.GPUDriver.Name(),
				"kernel-terminate-time-"+strconv.Itoa(i),
				float64(kernelTerminateTime[i]))
			r.metricsCollector.Collect(
				r.GPUDriver.Name(),
				"kernel-execution-time-"+strconv.Itoa(i),
				float64(kernelTerminateTime[i]-kernelLaunchTime[i]))
			totalKernelTime += float64(kernelTerminateTime[i] - kernelLaunchTime[i])
		}

		for i := range numKernels {
			r.metricsCollector.Collect(
				r.GPUDriver.Name(),
				"kernel-"+strconv.Itoa(i)+"-PC",
				float64(kernelBasePCList[i]))
		}

		lastKernelLaunchTime = kernelLaunchTime[numKernels-1]

		r.metricsCollector.Collect(
			r.GPUDriver.Name(),
			"last-kernel-launch-time",
			float64(kernelLaunchTime[numKernels-1]))
		r.metricsCollector.Collect(
			r.GPUDriver.Name(),
			"last-traced-time",
			float64(r.Engine.CurrentTime()))
		r.metricsCollector.Collect(
			r.GPUDriver.Name(),
			"total-kernel-time",
			totalKernelTime)
	}
}

func (r *Runner) reportMemoryUsage() {
	numDataAllocated := r.MemoryUsageTracer.GetNumDataAllocated()
	memoryUsagePerData := r.MemoryUsageTracer.GetMemoryUsagePerData()

	r.metricsCollector.Collect(
		r.GPUDriver.Name(),
		"total-memory-usage",
		r.MemoryUsageTracer.GetTotalMemoryUsage(),
	)

	for i := 0; i < int(numDataAllocated); i++ {
		r.metricsCollector.Collect(
			r.GPUDriver.Name(),
			"memory-usage "+strconv.Itoa(i),
			memoryUsagePerData[i])
	}
}

func (r *Runner) reportInstCount() {
	totalInstCount := uint64(0)

	for _, t := range r.InstCountTracers {
		totalInstCount += t.tracer.count
	}

	r.metricsCollector.Collect(
		r.GPUDriver.Name(),
		"total_inst_count",
		float64(totalInstCount))

	r.metricsCollector.Collect(
		r.GPUDriver.Name(),
		"max-instruction",
		float64(*maxInstCount))

	for _, t := range r.InstCountTracers {
		r.metricsCollector.Collect(
			t.cu.Name(), "inst_count", float64(t.tracer.count))
	}

	numCUs := len(r.GPUDriver.GPUs[0].CUs)
	numChiplets := *xDim * *yDim
	numCUsPerChiplet := numCUs / numChiplets
	avgLastWGFinishTime := make([]akita.VTimeInSec, numChiplets)

	for i, t := range r.InstCountTracers {
		chipletId := i / numCUsPerChiplet
		avgLastWGFinishTime[chipletId] += (t.tracer.lastWGFinishTime - lastKernelLaunchTime)

		r.metricsCollector.Collect(
			t.cu.Name(), "last_WG_finish_time", float64(t.tracer.lastWGFinishTime))
	}
	for i, t := range avgLastWGFinishTime {
		avgLastWGFinishTime[i] = t / akita.VTimeInSec(numCUsPerChiplet)
		r.metricsCollector.Collect(
			"chiplet-"+strconv.Itoa(i),
			"avg_last_WG_finish_time",
			float64(avgLastWGFinishTime[i]))
	}
}

func (r *Runner) reportAllParameters() {
	var parameterList string
	flag.Visit(func(f *flag.Flag) {
		parameterList += fmt.Sprintf("%v: %v", f.Name, f.Value) + "\n"
	})
	parameterList += "Simulation Start At:" + r.SimulationStartTime.Format("2006-01-02 15:04:05") + "\n"
	parameterList += "Simulation End At:" + time.Now().Format("2006-01-02 15:04:05")

	r.metricsCollector.Collect(
		"Parameters",
		"\n"+parameterList,
		0)
}

func (r *Runner) reportSimulationTime() {
	totalSimulationTime := time.Since(r.SimulationStartTime).Seconds()

	r.metricsCollector.Collect(
		r.GPUDriver.Name(),
		"simulation_time",
		totalSimulationTime/3600)

	r.metricsCollector.Collect(
		r.GPUDriver.Name(),
		"simulation_time_min",
		totalSimulationTime/60)

	r.metricsCollector.Collect(
		r.GPUDriver.Name(),
		"simulation_time_sec",
		totalSimulationTime)
}

func (r *Runner) reportExecutionTime() {
	if r.Timing {
		r.metricsCollector.Collect(
			r.GPUDriver.Name(),
			"kernel_time",
			float64(r.KernelTimeCounter.BusyTime()))

		r.metricsCollector.Collect(
			r.GPUDriver.Name(),
			"total_time",
			float64(r.Engine.CurrentTime()))

		kernelTimes := r.KernelTimeCounter.KernelTimes()
		for i, k := range kernelTimes {
			r.metricsCollector.Collect(
				r.GPUDriver.Name(),
				"kernel_time "+strconv.Itoa(i),
				float64(k))
		}

		for i, c := range r.PerGPUKernelTimeCounter {
			r.metricsCollector.Collect(
				r.GPUDriver.GPUs[i].CommandProcessor.Name(),
				"kernel_time",
				float64(c.BusyTime()))
			kernelTimes := r.KernelTimeCounter.KernelTimes()
			for j, k := range kernelTimes {
				r.metricsCollector.Collect(
					r.GPUDriver.GPUs[i].CommandProcessor.Name(),
					"kernel_time "+strconv.Itoa(j),
					float64(k))
			}

			// Only should be called at Exit()
			r.KernelTimeCounter.TerminateAllTasks(r.Engine.CurrentTime())

			kernelTimesForceStop := r.KernelTimeCounter.KernelTimesForceStop(r.Engine.CurrentTime())
			for j, k := range kernelTimesForceStop {
				r.metricsCollector.Collect(
					r.GPUDriver.GPUs[i].CommandProcessor.Name(),
					"kernel_time (force stop) "+strconv.Itoa(j),
					float64(k))
			}
		}
	}
}

func (r *Runner) reportCacheLatency() {
	l1vCacheAvgLatency := float64(0)
	l1vCacheCount := 0

	l1sCacheAvgLatency := float64(0)
	l1sCacheCount := 0

	l2CacheAvgLatency := float64(0)
	l2CacheCount := 0

	for _, tracer := range r.CacheLatencyTracers {
		if tracer.tracer.AverageTime() == 0 {
			continue
		}

		if strings.Contains(tracer.cache.Name(), "L1V") {
			l1vCacheAvgLatency += float64(tracer.tracer.AverageTime())
			l1vCacheCount++
		}

		if strings.Contains(tracer.cache.Name(), "L1S") {
			l1sCacheAvgLatency += float64(tracer.tracer.AverageTime())
			l1sCacheCount++
		}

		if strings.Contains(tracer.cache.Name(), "L2") {
			l2CacheAvgLatency += float64(tracer.tracer.AverageTime())
			l2CacheCount++
		}

		r.metricsCollector.Collect(
			tracer.cache.Name(),
			"req_average_latency",
			float64(tracer.tracer.AverageTime()),
		)
	}

	r.metricsCollector.Collect(
		r.GPUDriver.Name(),
		"total-l1v-avg-latency",
		l1vCacheAvgLatency/float64(l1vCacheCount),
	)
	r.metricsCollector.Collect(
		r.GPUDriver.Name(),
		"total-l1s-avg-latency",
		l1sCacheAvgLatency/float64(l1sCacheCount),
	)
	r.metricsCollector.Collect(
		r.GPUDriver.Name(),
		"total-l2-avg-latency",
		l2CacheAvgLatency/float64(l2CacheCount),
	)
	// for _, tracer := range r.L2PipelineLatencyTracers {
	// 	if tracer.tracer.AverageTime() == 0 {
	// 		continue
	// 	}

	// 	r.metricsCollector.Collect(
	// 		tracer.pipeline.Name(),
	// 		"req_average_pipeline_latency",
	// 		float64(tracer.tracer.AverageTime()),
	// 	)
	// }
}

func (r *Runner) reportTLBLatency() {
	avgLatency := float64(0)
	count := 0

	for _, tracer := range r.TLBLatencyTracers {
		if tracer.tracer.AverageTime() == 0 {
			continue
		}

		latency := float64(tracer.tracer.AverageTime())
		avgLatency += latency
		count++

		// r.metricsCollector.Collect(
		// 	tracer.tlb.Name(),
		// 	"req_average_latency",
		// 	latency,
		// )
	}

	r.metricsCollector.Collect(
		r.GPUDriver.Name(),
		"total-avg-tlb-latency",
		avgLatency/float64(count))
	// for _, tracer := range r.DownTLBLatencyTracers {
	// 	if tracer.tracer.AverageTime() == 0 {
	// 		continue
	// 	}

	// 	r.metricsCollector.Collect(
	// 		tracer.tlb.Name(),
	// 		"down_req_average_latency",
	// 		float64(tracer.tracer.AverageTime()),
	// 	)
	// }

	// for i, tracer := range r.L2TLBBufLenTracers {
	// 	if tracer.AverageCount() == 0 {
	// 		continue
	// 	}
	// 	r.metricsCollector.Collect(
	// 		"L2TLB"+strconv.Itoa(i),
	// 		"average_buf_len",
	// 		float64(tracer.AverageCount()),
	// 	)
	// }
	// for i, tracer := range r.L2TLBBufLenG0Tracers {
	// 	if tracer.AverageCount() == 0 {
	// 		continue
	// 	}
	// 	r.metricsCollector.Collect(
	// 		"L2TLB"+strconv.Itoa(i),
	// 		"average_buf_len_g0",
	// 		float64(tracer.AverageCount()),
	// 	)
	// }
	// for i, tracer := range r.L2TLBCoalesceAddrTracers {
	// 	if tracer.AverageCount() == 0 {
	// 		continue
	// 	}
	// 	r.metricsCollector.Collect(
	// 		"L2TLB"+strconv.Itoa(i),
	// 		"coalesce_addr",
	// 		float64(tracer.AverageCount()),
	// 	)
	// }
	// for i, tracer := range r.L2TLBCoalesceTracers {
	// 	if tracer.AverageCount() == 0 {
	// 		continue
	// 	}
	// 	r.metricsCollector.Collect(
	// 		"L2TLB"+strconv.Itoa(i),
	// 		"coalesce",
	// 		float64(tracer.AverageCount()),
	// 	)
	// }

	// for _, tracer := range r.TLBPipelineLatencyTracers {
	// 	if tracer.tracer.AverageTime() == 0 {
	// 		continue
	// 	}

	// 	r.metricsCollector.Collect(
	// 		tracer.pipeline.Name(),
	// 		"req_average_pipeline_latency",
	// 		float64(tracer.tracer.AverageTime()),
	// 	)
	// }

	// for i, tracer := range r.L2TLBMSHRLenTracers {
	// 	if tracer.AverageCount() == 0 {
	// 		continue
	// 	}
	// 	r.metricsCollector.Collect(
	// 		"L2TLB"+strconv.Itoa(i),
	// 		"average_mshr_len",
	// 		float64(tracer.AverageCount()),
	// 	)
	// }
	// for i, tracer := range r.L2TLBMSHRLenG0Tracers {
	// 	if tracer.AverageCount() == 0 {
	// 		continue
	// 	}
	// 	r.metricsCollector.Collect(
	// 		"L2TLB"+strconv.Itoa(i),
	// 		"average_mshr_len_g0",
	// 		float64(tracer.AverageCount()),
	// 	)
	// }

	// for i, tracer := range r.L2TLBMSHRUniqLenTracers {
	// 	if tracer.AverageCount() == 0 {
	// 		continue
	// 	}
	// 	r.metricsCollector.Collect(
	// 		"L2TLB"+strconv.Itoa(i),
	// 		"average_mshr_uniq_len",
	// 		float64(tracer.AverageCount()),
	// 	)
	// }

	// for i, tracer := range r.L2TLBMSHRUniqLenG0Tracers {
	// 	if tracer.AverageCount() == 0 {
	// 		continue
	// 	}
	// 	r.metricsCollector.Collect(
	// 		"L2TLB"+strconv.Itoa(i),
	// 		"average_mshr_uniq_len_g0",
	// 		float64(tracer.AverageCount()),
	// 	)
	// }
}

func (r *Runner) reportTLBConditionalStats() {
	for _, tracer := range r.TLBConditionalLatencyTracers {
		tlbTracer := tracer.tracer
		for _, stepName := range tlbTracer.GetStepNames() {
			if tlbTracer.AverageTime(stepName) == 0 {
				continue
			}

			r.metricsCollector.Collect(
				tracer.tlb.Name(),
				stepName+"-latency",
				float64(tlbTracer.AverageTime(stepName)),
			)

			r.metricsCollector.Collect(
				tracer.tlb.Name(),
				stepName+"-num",
				float64(tlbTracer.TotalCount(stepName)),
			)
		}
	}
}

func (r *Runner) reportPageWalkLatency() {
	total_avg_latency := float64(0)
	count := 0

	for _, tracer := range r.PageWalkLatencyTracers {
		if tracer.tracer.AverageTime() == 0 {
			continue
		}

		total_avg_latency += float64(tracer.tracer.AverageTime())
		count++

		r.metricsCollector.Collect(
			tracer.mmu.Name(),
			"req_average_latency",
			float64(tracer.tracer.AverageTime()),
		)
	}

	r.metricsCollector.Collect(
		r.GPUDriver.Name(),
		"total_page_walk_average_latency",
		total_avg_latency/float64(count),
	)
}

func (r *Runner) reportMMUConditionalStats() {
	for _, tracer := range r.MMUMemoryLatencyTracers {
		mmuTracer := tracer.tracer
		for _, stepName := range mmuTracer.GetStepNames() {
			if mmuTracer.AverageTime(stepName) == 0 {
				continue
			}

			r.metricsCollector.Collect(
				tracer.mmu.Name(),
				stepName+"-latency",
				float64(mmuTracer.AverageTime(stepName)),
			)

			r.metricsCollector.Collect(
				tracer.mmu.Name(),
				stepName+"-num",
				float64(mmuTracer.TotalCount(stepName)),
			)
		}
	}
}

func (r *Runner) reportActiveWalkerCount() {
	for _, tracer := range r.ActivePageWalkerTracers {
		if tracer.tracer.AverageCount() == 0 {
			continue
		}
		r.metricsCollector.Collect(
			tracer.mmu.Name(),
			"average active walkers",
			float64(tracer.tracer.AverageCount()),
		)
	}
}

func (r *Runner) reportDRAMLatency() {
	for _, tracer := range r.DRAMLatencyTracers {
		if tracer.tracer.AverageTime() == 0 {
			continue
		}

		r.metricsCollector.Collect(
			tracer.dram.Name(),
			"req_average_latency",
			float64(tracer.tracer.AverageTime()),
		)
	}
}

func (r *Runner) reportCacheHitRate() {
	totalTransaction := uint64(0)
	totalL2Transaction := uint64(0)
	totalL1ReadHit := uint64(0)
	totalL1ReadMiss := uint64(0)
	totalL1ReadMSHRHit := uint64(0)
	totalL1WriteHit := uint64(0)
	totalL1WriteMiss := uint64(0)
	totalL1WriteMSHRHit := uint64(0)
	totalL2ReadHit := uint64(0)
	totalL2ReadMiss := uint64(0)
	totalL2ReadMSHRHit := uint64(0)
	totalL2WriteHit := uint64(0)
	totalL2WriteMiss := uint64(0)
	totalL2WriteMSHRHit := uint64(0)

	// L1HitRatePerChiplet := make(map[string]float64)
	// L1MissRatePerChiplet := make(map[string]float64)
	L2HitRatePerChiplet := make(map[string]float64)
	L2MissRatePerChiplet := make(map[string]float64)

	for _, tracer := range r.CacheHitRateTracers {
		chiplet := strings.Split(tracer.cache.Name(), ".")[1]

		// L2 (writeback) emits split metrics: data-* for data accesses and a
		// separate pte-caching-* task family for PTE accesses (tracked by
		// PteCachingTracer). L1V keeps the legacy unprefixed names. Summing
		// both step names preserves the existing read-hit-rate / write-hit-rate
		// semantics for both cache levels without double-counting.
		readHit := tracer.tracer.GetStepCount("read-hit") +
			tracer.tracer.GetStepCount("data-read-hit")
		readMiss := tracer.tracer.GetStepCount("read-miss") +
			tracer.tracer.GetStepCount("data-read-miss")
		readMSHRHit := tracer.tracer.GetStepCount("read-mshr-hit") +
			tracer.tracer.GetStepCount("data-read-mshr-hit")
		writeHit := tracer.tracer.GetStepCount("write-hit") +
			tracer.tracer.GetStepCount("data-write-hit")
		writeMiss := tracer.tracer.GetStepCount("write-miss") +
			tracer.tracer.GetStepCount("data-write-miss")
		writeMSHRHit := tracer.tracer.GetStepCount("write-mshr-hit") +
			tracer.tracer.GetStepCount("data-write-mshr-hit")

		curTotalTransaction := readHit + readMiss + readMSHRHit +
			writeHit + writeMiss + writeMSHRHit

		if curTotalTransaction == 0 {
			continue
		}

		if strings.Contains(tracer.cache.Name(), "L1") {
			totalTransaction += curTotalTransaction
			totalL1ReadHit += readHit
			totalL1ReadMiss += readMiss
			totalL1ReadMSHRHit += readMSHRHit
			totalL1WriteHit += writeHit
			totalL1WriteMiss += writeMiss
			totalL1WriteMSHRHit += writeMSHRHit
		}
		if strings.Contains(tracer.cache.Name(), "L2") {
			totalTransaction += curTotalTransaction
			totalL2Transaction += curTotalTransaction
			totalL2ReadHit += readHit
			totalL2ReadMiss += readMiss
			totalL2ReadMSHRHit += readMSHRHit
			totalL2WriteHit += writeHit
			totalL2WriteMiss += writeMiss
			totalL2WriteMSHRHit += writeMSHRHit

			L2HitRatePerChiplet[chiplet] = float64(readHit+readMSHRHit+writeHit+writeMSHRHit) / float64(curTotalTransaction)
			L2MissRatePerChiplet[chiplet] = float64(readMiss+writeMiss) / float64(curTotalTransaction)
		}

		r.metricsCollector.Collect(
			tracer.cache.Name(), "read-hit", float64(readHit))
		r.metricsCollector.Collect(
			tracer.cache.Name(), "read-miss", float64(readMiss))
		r.metricsCollector.Collect(
			tracer.cache.Name(), "read-mshr-hit", float64(readMSHRHit))
		r.metricsCollector.Collect(
			tracer.cache.Name(), "write-hit", float64(writeHit))
		r.metricsCollector.Collect(
			tracer.cache.Name(), "write-miss", float64(writeMiss))
		r.metricsCollector.Collect(
			tracer.cache.Name(), "write-mshr-hit", float64(writeMSHRHit))
		r.metricsCollector.Collect(
			tracer.cache.Name(), "read-hit-rate", float64(readHit+readMSHRHit)/float64(readHit+readMiss+readMSHRHit))
		r.metricsCollector.Collect(
			tracer.cache.Name(), "write-hit-rate", float64(writeHit+writeMSHRHit)/float64(writeHit+writeMiss+writeMSHRHit))
		r.metricsCollector.Collect(
			tracer.cache.Name(), "hit-rate", float64(readHit+readMSHRHit+writeHit+writeMSHRHit)/float64(curTotalTransaction))
	}

	// for c, h := range L1HitRatePerChiplet {
	// 	r.metricsCollector.Collect(
	// 		c, "hit-rate", h)
	// }
	// for c, h := range L1MissRatePerChiplet {
	// 	r.metricsCollector.Collect(
	// 		c, "miss-rate", h)
	// }
	chiplets := make([]string, 0, len(L2HitRatePerChiplet))
	for c := range L2HitRatePerChiplet {
		chiplets = append(chiplets, c)
	}
	sort.Strings(chiplets)

	for _, c := range chiplets {
		r.metricsCollector.Collect(
			c,
			"l2-total-hit-rate",
			L2HitRatePerChiplet[c])
		r.metricsCollector.Collect(
			c,
			"l2-total-miss-rate",
			L2MissRatePerChiplet[c])
	}

	r.metricsCollector.Collect(
		"GPU1.chiplet_all.Caches", "total-transaction", float64(totalTransaction))
	r.metricsCollector.Collect(
		"GPU1.chiplet_all.Caches", "total-l1-read-hit", float64(totalL1ReadHit))
	r.metricsCollector.Collect(
		"GPU1.chiplet_all.Caches", "total-l1-read-miss", float64(totalL1ReadMiss))
	r.metricsCollector.Collect(
		"GPU1.chiplet_all.Caches", "total-l1-read-mshr-hit", float64(totalL1ReadMSHRHit))
	r.metricsCollector.Collect(
		"GPU1.chiplet_all.Caches", "total-l1-write-hit", float64(totalL1WriteHit))
	r.metricsCollector.Collect(
		"GPU1.chiplet_all.Caches", "total-l1-write-miss", float64(totalL1WriteMiss))
	r.metricsCollector.Collect(
		"GPU1.chiplet_all.Caches", "total-l1-write-mshr-hit", float64(totalL1WriteMSHRHit))
	r.metricsCollector.Collect(
		"GPU1.chiplet_all.Caches", "total-l1-hit",
		float64(totalL1ReadHit+totalL1ReadMSHRHit+totalL1WriteHit+totalL1WriteMSHRHit))
	r.metricsCollector.Collect(
		"GPU1.chiplet_all.Caches", "total-l1-miss", float64(totalL1ReadMiss+totalL1WriteMiss))
	r.metricsCollector.Collect(
		"GPU1.chiplet_all.Caches", "total-l2-read-hit", float64(totalL2ReadHit))
	r.metricsCollector.Collect(
		"GPU1.chiplet_all.Caches", "total-l2-read-miss", float64(totalL2ReadMiss))
	r.metricsCollector.Collect(
		"GPU1.chiplet_all.Caches", "total-l2-read-mshr-hit", float64(totalL2ReadMSHRHit))
	r.metricsCollector.Collect(
		"GPU1.chiplet_all.Caches", "total-l2-write-hit", float64(totalL2WriteHit))
	r.metricsCollector.Collect(
		"GPU1.chiplet_all.Caches", "total-l2-write-miss", float64(totalL2WriteMiss))
	r.metricsCollector.Collect(
		"GPU1.chiplet_all.Caches", "total-l2-write-mshr-hit", float64(totalL2WriteMSHRHit))
	r.metricsCollector.Collect(
		"GPU1.chiplet_all.Caches", "total-l2-hit",
		float64(totalL2ReadHit+totalL2ReadMSHRHit+totalL2WriteHit+totalL2WriteMSHRHit))
	r.metricsCollector.Collect(
		"GPU1.chiplet_all.Caches", "total-l2-miss", float64(totalL2ReadMiss+totalL2WriteMiss))
	r.metricsCollector.Collect(
		"GPU1.chiplet_all.Caches", "total-l2-transaction", float64(totalL2Transaction))
	r.metricsCollector.Collect(
		"GPU1.chiplet_all.Caches", "total-l2-hit-rate",
		float64(totalL2ReadHit+totalL2ReadMSHRHit+totalL2WriteHit+totalL2WriteMSHRHit)/float64(totalL2Transaction))
	r.metricsCollector.Collect(
		"GPU1.chiplet_all.Caches", "total-l2-miss-rate",
		float64(totalL2ReadMiss+totalL2WriteMiss)/float64(totalL2Transaction))
}

func (r *Runner) reportTLBHitRate() {
	totalTransaction := uint64(0)
	totalL1VTransaction := uint64(0)
	totalL1STransaction := uint64(0)
	totalL1ITransaction := uint64(0)
	totalL2Transaction := uint64(0)

	totalL1VHit := uint64(0)
	totalL1VMiss := uint64(0)
	totalL1VMSHRHit := uint64(0)

	totalL1SHit := uint64(0)
	totalL1SMiss := uint64(0)
	totalL1SMSHRHit := uint64(0)

	totalL1IHit := uint64(0)
	totalL1IMiss := uint64(0)
	totalL1IMSHRHit := uint64(0)

	totalL2Hit := uint64(0)
	totalL2Miss := uint64(0)
	totalL2MSHRHit := uint64(0)

	for _, tracer := range r.TLBHitRateTracers {
		tlbHit := tracer.tracer.GetStepCount("tlb-hit")
		tlbMiss := tracer.tracer.GetStepCount("tlb-miss")
		tlbMSHRHit := tracer.tracer.GetStepCount("tlb-mshr-hit")

		curTotalTransaction := tlbHit + tlbMiss + tlbMSHRHit
		totalTransaction += curTotalTransaction
		if curTotalTransaction == 0 {
			continue
		}

		if strings.Contains(tracer.tlb.Name(), "L1V") {
			totalL1VTransaction += curTotalTransaction
			totalL1VHit += tlbHit
			totalL1VMiss += tlbMiss
			totalL1VMSHRHit += tlbMSHRHit
		} else if strings.Contains(tracer.tlb.Name(), "L1S") {
			totalL1STransaction += curTotalTransaction
			totalL1SHit += tlbHit
			totalL1SMiss += tlbMiss
			totalL1SMSHRHit += tlbMSHRHit
		} else if strings.Contains(tracer.tlb.Name(), "L1I") {
			totalL1ITransaction += curTotalTransaction
			totalL1IHit += tlbHit
			totalL1IMiss += tlbMiss
			totalL1IMSHRHit += tlbMSHRHit
		} else if strings.Contains(tracer.tlb.Name(), "L2") {
			totalL2Transaction += curTotalTransaction
			totalL2Hit += tlbHit
			totalL2Miss += tlbMiss
			totalL2MSHRHit += tlbMSHRHit
		}

		// tlbName := tracer.tlb.Name()
		// r.metricsCollector.Collect(
		// 	tlbName, "tlb-hit", float64(tlbHit))
		// r.metricsCollector.Collect(
		// 	tlbName, "tlb-miss", float64(tlbMiss))
		// r.metricsCollector.Collect(
		// 	tlbName, "tlb-mshr-hit", float64(tlbMSHRHit))
	}

	r.metricsCollector.Collect(
		"GPU1.chiplet_all.TLBs",
		"total-transaction",
		float64(totalTransaction))
	r.metricsCollector.Collect(
		"GPU1.chiplet_all.L1VTLBs",
		"total-transaction",
		float64(totalL1VTransaction))
	r.metricsCollector.Collect(
		"GPU1.chiplet_all.L1STLBs",
		"total-transaction",
		float64(totalL1STransaction))
	r.metricsCollector.Collect(
		"GPU1.chiplet_all.L1ITLBs",
		"total-transaction",
		float64(totalL1ITransaction))
	r.metricsCollector.Collect(
		"GPU1.chiplet_all.L2TLBs",
		"total-transaction",
		float64(totalL2Transaction))

	r.metricsCollector.Collect(
		"GPU1.chiplet_all.L1VTLBs",
		"total-l1v-hit",
		float64(totalL1VHit))
	r.metricsCollector.Collect(
		"GPU1.chiplet_all.L1STLBs",
		"total-l1s-hit",
		float64(totalL1SHit))
	r.metricsCollector.Collect(
		"GPU1.chiplet_all.L1ITLBs",
		"total-l1i-hit",
		float64(totalL1IHit))
	r.metricsCollector.Collect(
		"GPU1.chiplet_all.L2TLBs",
		"total-l2-hit",
		float64(totalL2Hit))

	r.metricsCollector.Collect(
		"GPU1.chiplet_all.L1VTLBs",
		"total-l1v-mshr-hit",
		float64(totalL1VMSHRHit))
	r.metricsCollector.Collect(
		"GPU1.chiplet_all.L1STLBs",
		"total-l1s-mshr-hit",
		float64(totalL1SMSHRHit))
	r.metricsCollector.Collect(
		"GPU1.chiplet_all.L1ITLBs",
		"total-l1i-mshr-hit",
		float64(totalL1IMSHRHit))
	r.metricsCollector.Collect(
		"GPU1.chiplet_all.L2TLBs",
		"total-l2-mshr-hit",
		float64(totalL2MSHRHit))

	r.metricsCollector.Collect(
		"GPU1.chiplet_all.L1VTLBs",
		"hit-rate",
		float64(totalL1VHit+totalL1VMSHRHit)/float64(totalL1VTransaction))
	r.metricsCollector.Collect(
		"GPU1.chiplet_all.L1STLBs",
		"hit-rate",
		float64(totalL1SHit+totalL1SMSHRHit)/float64(totalL1STransaction))
	r.metricsCollector.Collect(
		"GPU1.chiplet_all.L1ITLBs",
		"hit-rate",
		float64(totalL1IHit+totalL1IMSHRHit)/float64(totalL1ITransaction))
	r.metricsCollector.Collect(
		"GPU1.chiplet_all.L2TLBs",
		"hit-rate",
		float64(totalL2Hit+totalL2MSHRHit)/float64(totalL2Transaction))
}

func (r *Runner) reportPWCHitRate() {
	totalPwcHit0 := uint64(0)
	totalPwcHit1 := uint64(0)
	totalPwcHit2 := uint64(0)
	totalPwcHit3 := uint64(0)

	totalPwcMiss0 := uint64(0)
	totalPwcMiss1 := uint64(0)
	totalPwcMiss2 := uint64(0)
	totalPwcMiss3 := uint64(0)

	totalPageWalkReqLocal := uint64(0)
	totalPageWalkReqRemote := uint64(0)

	totalL0RemotePageWalkReq := uint64(0) // top level
	totalL1RemotePageWalkReq := uint64(0)
	totalL2RemotePageWalkReq := uint64(0)
	totalL3RemotePageWalkReq := uint64(0) // last level

	totalL0LocalPageWalkReq := uint64(0) // top level
	totalL1LocalPageWalkReq := uint64(0)
	totalL2LocalPageWalkReq := uint64(0)
	totalL3LocalPageWalkReq := uint64(0) // last level

	for _, tracer := range r.PWCHitRateTracers {
		pwcHit0 := tracer.tracer.GetStepCount("pwc-hit-level0")
		pwcHit1 := tracer.tracer.GetStepCount("pwc-hit-level1")
		pwcHit2 := tracer.tracer.GetStepCount("pwc-hit-level2")
		pwcHit3 := tracer.tracer.GetStepCount("pwc-hit-level3")

		pwcMiss0 := tracer.tracer.GetStepCount("pwc-miss-level0")
		pwcMiss1 := tracer.tracer.GetStepCount("pwc-miss-level1")
		pwcMiss2 := tracer.tracer.GetStepCount("pwc-miss-level2")
		pwcMiss3 := tracer.tracer.GetStepCount("pwc-miss-level3")

		topLevelRemoteMemReq := tracer.tracer.GetStepCount("pw-level-0-remote-reqs")
		level1RemoteMemReq := tracer.tracer.GetStepCount("pw-level-1-remote-reqs")
		level2RemoteMemReq := tracer.tracer.GetStepCount("pw-level-2-remote-reqs")
		lastLevelRemoteMemReq := tracer.tracer.GetStepCount("pw-level-3-remote-reqs")

		topLevelLocalMemReq := tracer.tracer.GetStepCount("pw-level-0-local-reqs")
		level1LocalMemReq := tracer.tracer.GetStepCount("pw-level-1-local-reqs")
		level2LocalMemReq := tracer.tracer.GetStepCount("pw-level-2-local-reqs")
		lastLevelLocalMemReq := tracer.tracer.GetStepCount("pw-level-3-local-reqs")

		totalTransaction := pwcHit3 + pwcHit2 + pwcHit1 + pwcHit0 +
			pwcMiss3 + pwcMiss2 + pwcMiss1 + pwcMiss0

		if totalTransaction == 0 {
			continue
		}

		totalPwcHit0 += pwcHit0
		totalPwcHit1 += pwcHit1
		totalPwcHit2 += pwcHit2
		totalPwcHit3 += pwcHit3

		totalPwcMiss0 += pwcMiss0
		totalPwcMiss1 += pwcMiss1
		totalPwcMiss2 += pwcMiss2
		totalPwcMiss3 += pwcMiss3

		totalL0RemotePageWalkReq += topLevelRemoteMemReq
		totalL1RemotePageWalkReq += level1RemoteMemReq
		totalL2RemotePageWalkReq += level2RemoteMemReq
		totalL3RemotePageWalkReq += lastLevelRemoteMemReq

		totalL0LocalPageWalkReq += topLevelLocalMemReq
		totalL1LocalPageWalkReq += level1LocalMemReq
		totalL2LocalPageWalkReq += level2LocalMemReq
		totalL3LocalPageWalkReq += lastLevelLocalMemReq

		remoteMemReq := level1RemoteMemReq + level2RemoteMemReq + lastLevelRemoteMemReq + topLevelRemoteMemReq
		localMemReq := level1LocalMemReq + level2LocalMemReq + lastLevelLocalMemReq + topLevelLocalMemReq

		totalPageWalkReqLocal += localMemReq
		totalPageWalkReqRemote += remoteMemReq

		// r.metricsCollector.Collect(
		// 	tracer.mmu.Name(), "pwc-hit-level0", float64(pwcHit0))
		// r.metricsCollector.Collect(
		// 	tracer.mmu.Name(), "pwc-hit-level1", float64(pwcHit1))
		// r.metricsCollector.Collect(
		// 	tracer.mmu.Name(), "pwc-hit-level2", float64(pwcHit2))
		// r.metricsCollector.Collect(
		// 	tracer.mmu.Name(), "pwc-hit-level3", float64(pwcHit3))
		// r.metricsCollector.Collect(
		// 	tracer.mmu.Name(), "pwc-miss-level0", float64(pwcMiss0))
		// r.metricsCollector.Collect(
		// 	tracer.mmu.Name(), "pwc-miss-level1", float64(pwcMiss1))
		// r.metricsCollector.Collect(
		// 	tracer.mmu.Name(), "pwc-miss-level2", float64(pwcMiss2))
		// r.metricsCollector.Collect(
		// 	tracer.mmu.Name(), "pwc-miss-level3", float64(pwcMiss3))
		// r.metricsCollector.Collect(
		// 	tracer.mmu.Name(), "page_walk_req_local", float64(localMemReq))
		// r.metricsCollector.Collect(
		// 	tracer.mmu.Name(), "page_walk_req_remote", float64(remoteMemReq))
		// r.metricsCollector.Collect(
		// 	tracer.mmu.Name(), "pw-level-3-remote-reqs", float64(lastLevelRemoteMemReq))
	}

	r.metricsCollector.Collect(
		"GPU1.chiplet_all.MMU",
		"total-pwc-hit-level0",
		float64(totalPwcHit0))
	r.metricsCollector.Collect(
		"GPU1.chiplet_all.MMU",
		"total-pwc-hit-level1",
		float64(totalPwcHit1))
	r.metricsCollector.Collect(
		"GPU1.chiplet_all.MMU",
		"total-pwc-hit-level2",
		float64(totalPwcHit2))
	r.metricsCollector.Collect(
		"GPU1.chiplet_all.MMU",
		"total-pwc-hit-level3",
		float64(totalPwcHit3))
	r.metricsCollector.Collect(
		"GPU1.chiplet_all.MMU",
		"total-pwc-miss-level0",
		float64(totalPwcMiss0))
	r.metricsCollector.Collect(
		"GPU1.chiplet_all.MMU",
		"total-pwc-miss-level1",
		float64(totalPwcMiss1))
	r.metricsCollector.Collect(
		"GPU1.chiplet_all.MMU",
		"total-pwc-miss-level2",
		float64(totalPwcMiss2))
	r.metricsCollector.Collect(
		"GPU1.chiplet_all.MMU",
		"total-pwc-miss-level3",
		float64(totalPwcMiss3))

	r.metricsCollector.Collect(
		"GPU1.chiplet_all.MMU",
		"total-page-walk-req-local",
		float64(totalPageWalkReqLocal))
	r.metricsCollector.Collect(
		"GPU1.chiplet_all.MMU",
		"total-page-walk-req-remote",
		float64(totalPageWalkReqRemote))
	r.metricsCollector.Collect(
		"GPU1.chiplet_all.MMU",
		"total-top-level-remote-page-walk-req",
		float64(totalL0RemotePageWalkReq))
	r.metricsCollector.Collect(
		"GPU1.chiplet_all.MMU",
		"total-l1-remote-page-walk-req",
		float64(totalL1RemotePageWalkReq))
	r.metricsCollector.Collect(
		"GPU1.chiplet_all.MMU",
		"total-l2-remote-page-walk-req",
		float64(totalL2RemotePageWalkReq))
	r.metricsCollector.Collect(
		"GPU1.chiplet_all.MMU",
		"total-last-level-remote-page-walk-req",
		float64(totalL3RemotePageWalkReq))
}

func (r *Runner) reportRDMATransactionCount() {

	for _, t := range r.RDMATransactionCounters {
		r.metricsCollector.Collect(
			t.rdmaEngine.Name(),
			"outgoing_trans_count",
			float64(t.outgoingTracer.TotalCount()),
		)
		r.metricsCollector.Collect(
			t.rdmaEngine.Name(),
			"incoming_trans_count",
			float64(t.incomingTracer.TotalCount()),
		)
	}
}

func (r *Runner) reportCDMATransactionCount() {
	totalIncomingTransCount := uint64(0)
	totalOutgoingTransCount := uint64(0)
	totalIncomingTransLatency := float64(0)
	totalOutgoingTransLatency := float64(0)
	totalIncomingTransLatencyWeighted := float64(0)
	totalOutgoingTransLatencyWeighted := float64(0)

	for _, t := range r.CDMATransactionCounters {
		inCount := t.incomingTracer.TotalCount()
		outCount := t.outgoingTracer.TotalCount()
		inAvg := float64(t.incomingTracer.AverageTime())
		outAvg := float64(t.outgoingTracer.AverageTime())

		r.metricsCollector.Collect(
			t.rdmaEngine.Name(),
			"outgoing_trans_count",
			float64(outCount),
		)
		r.metricsCollector.Collect(
			t.rdmaEngine.Name(),
			"outgoing_trans_latency",
			outAvg,
		)
		r.metricsCollector.Collect(
			t.rdmaEngine.Name(),
			"incoming_trans_count",
			float64(inCount),
		)
		r.metricsCollector.Collect(
			t.rdmaEngine.Name(),
			"incoming_trans_latency",
			inAvg,
		)
		totalIncomingTransCount += inCount
		totalOutgoingTransCount += outCount
		totalIncomingTransLatency += inAvg
		totalOutgoingTransLatency += outAvg
		totalIncomingTransLatencyWeighted += inAvg * float64(inCount)
		totalOutgoingTransLatencyWeighted += outAvg * float64(outCount)
	}
	for _, t := range r.CDMAAccessTracers {
		cdmaEngine := t.cdmaEngine
		cdmaEngineTracer := t.tracer
		for _, step := range cdmaEngineTracer.GetStepNames() {
			r.metricsCollector.Collect(
				cdmaEngine.Name(),
				"inter_chiplet_traffic: "+step,
				float64(cdmaEngineTracer.GetStepCount(step)),
			)
		}
	}

	for _, t := range r.PageTransactionCounters {
		inCount := t.incomingTracer.TotalCount()
		outCount := t.outgoingTracer.TotalCount()
		inAvg := float64(t.incomingTracer.AverageTime())
		outAvg := float64(t.outgoingTracer.AverageTime())

		r.metricsCollector.Collect(
			t.rdmaEngine.Name(),
			"outgoing_trans_count",
			float64(outCount),
		)
		r.metricsCollector.Collect(
			t.rdmaEngine.Name(),
			"outgoing_trans_latency",
			outAvg,
		)
		r.metricsCollector.Collect(
			t.rdmaEngine.Name(),
			"incoming_trans_count",
			float64(inCount),
		)
		r.metricsCollector.Collect(
			t.rdmaEngine.Name(),
			"incoming_trans_latency",
			inAvg,
		)
		totalIncomingTransCount += inCount
		totalOutgoingTransCount += outCount
		totalIncomingTransLatency += inAvg
		totalOutgoingTransLatency += outAvg
		totalIncomingTransLatencyWeighted += inAvg * float64(inCount)
		totalOutgoingTransLatencyWeighted += outAvg * float64(outCount)
	}
	for _, t := range r.PageAccessTracers {
		cdmaEngine := t.cdmaEngine
		cdmaEngineTracer := t.tracer
		for _, step := range cdmaEngineTracer.GetStepNames() {
			r.metricsCollector.Collect(
				cdmaEngine.Name(),
				"inter_chiplet_traffic: "+step,
				float64(cdmaEngineTracer.GetStepCount(step)),
			)
		}
	}
	avgIncomingLatency := float64(0)
	if totalIncomingTransCount > 0 {
		avgIncomingLatency = totalIncomingTransLatencyWeighted / float64(totalIncomingTransCount)
	}
	avgOutgoingLatency := float64(0)
	if totalOutgoingTransCount > 0 {
		avgOutgoingLatency = totalOutgoingTransLatencyWeighted / float64(totalOutgoingTransCount)
	}

	r.metricsCollector.Collect(
		"GPU1.chiplet_all.ChipRDMA",
		"total_incoming_trans_count",
		float64(totalIncomingTransCount))
	r.metricsCollector.Collect(
		"GPU1.chiplet_all.ChipRDMA",
		"total_incoming_trans_latency",
		totalIncomingTransLatency)
	r.metricsCollector.Collect(
		"GPU1.chiplet_all.ChipRDMA",
		"avg_incoming_trans_latency",
		avgIncomingLatency)
	r.metricsCollector.Collect(
		"GPU1.chiplet_all.ChipRDMA",
		"total_outgoing_trans_count",
		float64(totalOutgoingTransCount))
	r.metricsCollector.Collect(
		"GPU1.chiplet_all.ChipRDMA",
		"total_outgoing_trans_latency",
		totalOutgoingTransLatency)
	r.metricsCollector.Collect(
		"GPU1.chiplet_all.ChipRDMA",
		"avg_outgoing_trans_latency",
		avgOutgoingLatency)
}

func (r *Runner) reportDRAMTransactionCount() {
	totalTransaction := uint64(0)
	for _, t := range r.DRAMTransactionCounters {
		count := t.tracer.TotalCount()
		totalTransaction += count

		// r.metricsCollector.Collect(
		// 	t.dram.Name(),
		// 	"trans-count",
		// 	float64(count),
		// )
	}

	r.metricsCollector.Collect(
		"GPU1.chiplet_all.DRAM_all",
		"total-trans-count",
		float64(totalTransaction),
	)
}

func (r *Runner) reportTranslationReqLatency() {
	tracer := r.TranslationReqTracer
	if tracer == nil {
		return
	}
	for _, stepName := range tracer.GetStepNames() {
		// if tracer.AverageTime(stepName) == 0 {
		// 	continue
		// }
		r.metricsCollector.Collect(
			"trace-trans-req",
			stepName+"-latency",
			float64(tracer.AverageTime(stepName)),
		)
		r.metricsCollector.Collect(
			"trace-trans-req",
			stepName+"-num",
			float64(tracer.TotalCount(stepName)),
		)
	}
}

func (r *Runner) reportAddressTranslatorLatency() {
	l1vReqLatency := float64(0)
	l1vReqCount := uint64(0)
	l1vAddrTranslatorLatency := float64(0)
	l1vAddrTranslatorCount := uint64(0)

	l1sReqLatency := float64(0)
	l1sReqCount := uint64(0)
	l1sAddrTranslatorLatency := float64(0)
	l1sAddrTranslatorCount := uint64(0)

	for _, t := range r.AddressTranslatorLatencyTracers {
		addressTranslator := t.addressTranslator
		tracer := t.tracer

		reqLatency := tracer.AverageTime("req-latency")
		reqCount := tracer.TotalCount("req-latency")
		translationLatency := tracer.AverageTime("translation-latency")
		translationCount := tracer.TotalCount("translation-latency")

		if strings.Contains(addressTranslator.Name(), "L1V") {
			l1vReqLatency += float64(reqLatency)
			l1vReqCount += reqCount
			l1vAddrTranslatorLatency += float64(translationLatency)
			l1vAddrTranslatorCount += translationCount
		} else if strings.Contains(addressTranslator.Name(), "L1S") {
			l1sReqLatency += float64(reqLatency)
			l1sReqCount += reqCount
			l1sAddrTranslatorLatency += float64(translationLatency)
			l1sAddrTranslatorCount += translationCount

		}

		// r.metricsCollector.Collect(
		// 	addressTranslator.Name(),
		// 	"req-latency",
		// 	float64(reqLatency),
		// )
		// r.metricsCollector.Collect(
		// 	addressTranslator.Name(),
		// 	"req-num",
		// 	float64(reqCount),
		// )
		// r.metricsCollector.Collect(
		// 	addressTranslator.Name(),
		// 	"translation-latency",
		// 	float64(tracer.AverageTime("translation-latency")),
		// )
		// r.metricsCollector.Collect(
		// 	addressTranslator.Name(),
		// 	"translation-num",
		// 	float64(tracer.TotalCount("translation-latency")),
		// )
	}

	r.metricsCollector.Collect(
		"GPU1.chiplet_all.L1VAddressTranslator",
		"req-latency",
		l1vReqLatency/float64(l1vReqCount),
	)
	r.metricsCollector.Collect(
		"GPU1.chiplet_all.L1VAddressTranslator",
		"translation-latency",
		l1vAddrTranslatorLatency/float64(l1vAddrTranslatorCount),
	)
	r.metricsCollector.Collect(
		"GPU1.chiplet_all.L1SAddressTranslator",
		"req-latency",
		l1sReqLatency/float64(l1sReqCount),
	)
	r.metricsCollector.Collect(
		"GPU1.chiplet_all.L1SAddressTranslator",
		"translation-latency",
		l1sAddrTranslatorLatency/float64(l1sAddrTranslatorCount),
	)
}

func (r *Runner) reportL2TLBQueueImbalance() {
	for _, t := range r.L2TLBQueueingImbalanceTracers {
		r.metricsCollector.Collect(
			t.tlb.Name(),
			"imbalance",
			float64(t.tracer.ReportImbalance()),
		)
		r.metricsCollector.Collect(
			t.tlb.Name(),
			"imbalance-count",
			float64(t.tracer.ReportCount()),
		)
		for i, bucket := range t.tracer.ReportImbalanceBuckets() {
			r.metricsCollector.Collect(
				t.tlb.Name(),
				"imbalance-"+strconv.Itoa(i*10),
				float64(bucket),
			)
		}
	}
}

func (r *Runner) reportL2TLBEntropy() {
	for _, t := range r.EntropyTracers {
		for i := 12; i < 28; i++ {
			r.metricsCollector.Collect(
				t.tlb.Name(),
				"entropy_"+strconv.Itoa(i),
				float64(t.tracer.ReportEntropy()[i]),
			)
		}
	}
}

func (r *Runner) reportTLBSetMissTracing() {
	for _, t := range r.TLBSetMissTracers {
		r.metricsCollector.Collect(
			t.tlb.Name(),
			"miss_count_",
			float64(t.tracer.ReturnAverageReferenceCount()),
		)
	}
	for _, t := range r.TLBSetMissTracers {
		r.metricsCollector.Collect(
			t.tlb.Name(),
			"miss_interval_avg_",
			float64(t.tracer.ReturnAverageReferenceInterval()),
		)
	}
	for _, t := range r.TLBSetMissTracers {
		t.tracer.CalculateHistogramSummary()
		for i := 0; i < 64; i++ {
			r.metricsCollector.Collect(
				t.tlb.Name(),
				"miss_histogram_summary_"+strconv.Itoa(i),
				float64(t.tracer.ReturnHistogramSummary(uint(i))),
			)
		}
	}
	for _, t := range r.TLBSetMissTracers {
		for i := 0; i < 32; i++ {
			r.metricsCollector.Collect(
				t.tlb.Name(),
				"miss_count_histogram_summary_"+strconv.Itoa(i),
				float64(t.tracer.ReturnCountHistogramSummary(uint(i))),
			)
		}
	}
}

func (r *Runner) reportTLBMSHRStallTracing() {
	for _, t := range r.TLBMSHRStallTracers {
		r.metricsCollector.Collect(
			t.tlb.Name(),
			"mshr_stall",
			float64(t.tracer.TotalTime()),
		)
	}
}

func (r *Runner) reportTLBReqStalls() {
	for _, t := range r.TLBReqStallTracers {
		tracer := t.tracer
		tlb := t.tlb
		for _, stepName := range tracer.GetStepNames() {
			r.metricsCollector.Collect(
				tlb.Name(),
				stepName,
				float64(tracer.GetStepCount(stepName)),
			)
		}
	}
}

/*
 *
 * Custom Tracer
 *
 */

func (r *Runner) reportPteAccessTraces() {
	r.PteAccessTracer.Dump(nil)
}

func (r *Runner) reportMemoryAccessTraces() {
	r.MemoryAccessTracer.Dump(nil)
	// r.RdmaTracer.Dump()
}

func (r *Runner) reportCacheLineHopStats() {
	where := "GPU1.chiplet_all.CacheLineHop"
	for _, row := range r.CacheLineHopTracer.PercentileStats() {
		prefix := "top-" + strconv.Itoa(row.TopPct)
		r.metricsCollector.Collect(where, prefix+"-n-lines-in-cut", float64(row.NLinesInCut))
		r.metricsCollector.Collect(where, prefix+"-n-lines-with-hop-data", float64(row.NLinesWithHopData))
		r.metricsCollector.Collect(where, prefix+"-max-hop", row.Max)
		r.metricsCollector.Collect(where, prefix+"-min-hop", row.Min)
		r.metricsCollector.Collect(where, prefix+"-avg-hop", row.Avg)
		r.metricsCollector.Collect(where, prefix+"-median-hop", row.Median)
	}
}

func (r *Runner) reportAccessCountPerAddrs(name string) {
	r.AccessCountPerAddrTracer.Dump(name)
}

func (r *Runner) reportEvictMetrics() {
	localEvictCount := r.EvictTracer.GetLocalEvictCount()
	remoteEvictCount := r.EvictTracer.GetRemoteEvictCount()
	localOverwriteCount := r.EvictTracer.GetLocalOverwriteCount()
	remoteOverwriteCount := r.EvictTracer.GetRemoteOverwriteCount()

	localEvictPteCount := r.EvictTracer.GetLocalEvictPteCount()
	remoteEvictPteCount := r.EvictTracer.GetRemoteEvictPteCount()
	localOverwritePteCount := r.EvictTracer.GetLocalOverwritePteCount()
	remoteOverwritePteCount := r.EvictTracer.GetRemoteOverwritePteCount()

	r.metricsCollector.Collect(
		"GPU1.chiplet_all.Caches",
		"local-evict-count",
		float64(localEvictCount))
	r.metricsCollector.Collect(
		"GPU1.chiplet_all.Caches",
		"remote-evict-count",
		float64(remoteEvictCount))
	r.metricsCollector.Collect(
		"GPU1.chiplet_all.Caches",
		"local-overwritten-count",
		float64(localOverwriteCount))
	r.metricsCollector.Collect(
		"GPU1.chiplet_all.Caches",
		"remote-overwritten-count",
		float64(remoteOverwriteCount))

	r.metricsCollector.Collect(
		"GPU1.chiplet_all.Caches",
		"local-evict-pte-count",
		float64(localEvictPteCount))
	r.metricsCollector.Collect(
		"GPU1.chiplet_all.Caches",
		"remote-evict-pte-count",
		float64(remoteEvictPteCount))
	r.metricsCollector.Collect(
		"GPU1.chiplet_all.Caches",
		"local-overwritten-pte-count",
		float64(localOverwritePteCount))
	r.metricsCollector.Collect(
		"GPU1.chiplet_all.Caches",
		"remote-overwritten-pte-count",
		float64(remoteOverwritePteCount))

	for c, v := range r.EvictTracer.GetLocalEvictCountPerChiplet() {
		r.metricsCollector.Collect(
			"GPU1.chiplet_"+strconv.Itoa(c)+".Caches",
			"local-evict-count",
			float64(v))
	}
	for c, v := range r.EvictTracer.GetRemoteEvictCountPerChiplet() {
		r.metricsCollector.Collect(
			"GPU1.chiplet_"+strconv.Itoa(c)+".Caches",
			"remote-evict-count",
			float64(v))
	}
	for c, v := range r.EvictTracer.GetLocalOverwriteCountPerChiplet() {
		r.metricsCollector.Collect(
			"GPU1.chiplet_"+strconv.Itoa(c)+".Caches",
			"local-overwritten-count",
			float64(v))
	}
	for c, v := range r.EvictTracer.GetRemoteOverwriteCountPerChiplet() {
		r.metricsCollector.Collect(
			"GPU1.chiplet_"+strconv.Itoa(c)+".Caches",
			"remote-overwritten-count",
			float64(v))
	}
}

func (r *Runner) reportWorkgroupCount() {
	// Tracers exist only under -report-all / -report-workgroup-count; skip
	// when absent so minimal-flag runs don't nil-deref at reportStats.
	if r.WorkgroupCountPerKernelTracer == nil ||
		r.WorkgroupCountPerChipletTracer == nil {
		return
	}
	workgroupCountPerKernel := r.WorkgroupCountPerKernelTracer.GetWorkgroupCountPerKernel()
	workgroupCountPerChiplet := r.WorkgroupCountPerChipletTracer.GetWorkgroupCountPerChiplet()

	kernelNames := make([]string, 0, len(workgroupCountPerKernel))
	for kernelName, _ := range workgroupCountPerKernel {
		kernelNames = append(kernelNames, kernelName)
	}
	sort.Slice(kernelNames, func(i, j int) bool {
		return kernelNames[i] < kernelNames[j]
	})

	for i, kernelName := range kernelNames {
		r.metricsCollector.Collect(
			r.GPUDriver.Name(),
			"kernel-"+strconv.Itoa(i)+"-workgroup-count",
			float64(workgroupCountPerKernel[kernelName]))
	}

	chipletIds := make([]int, 0, len(workgroupCountPerChiplet))
	for chipletId, _ := range workgroupCountPerChiplet {
		chipletIds = append(chipletIds, chipletId)
	}
	sort.Slice(chipletIds, func(i, j int) bool {
		return chipletIds[i] < chipletIds[j]
	})

	for i, chipletId := range chipletIds {
		r.metricsCollector.Collect(
			r.GPUDriver.Name(),
			"Chiplet-"+strconv.Itoa(i)+"-workgroup-count",
			float64(workgroupCountPerChiplet[chipletId]))
	}
}

// reportDispatchProgress emits work-group completion progress per kernel and
// an aggregate over all kernels, summed across all GPUs. Always reported,
// independent of -no-progress-bar. On a clean finish completed==total; on a
// killed/deadlocked run it shows the partial progress of the in-flight kernel.
func (r *Runner) reportDispatchProgress() {
	perKernel := make(map[int][2]int)
	for _, gpu := range r.GPUDriver.GPUs {
		for seq, ct := range gpu.CommandProcessor.DispatchProgressPerKernel() {
			v := perKernel[seq]
			v[0] += ct[0]
			v[1] += ct[1]
			perKernel[seq] = v
		}
	}

	where := r.GPUDriver.Name()

	seqs := make([]int, 0, len(perKernel))
	for seq := range perKernel {
		seqs = append(seqs, seq)
	}
	sort.Ints(seqs)

	var totalCompleted, totalWG int
	for _, seq := range seqs {
		completed, total := perKernel[seq][0], perKernel[seq][1]
		totalCompleted += completed
		totalWG += total

		prefix := "kernel-" + strconv.Itoa(seq) + "-wg-"
		r.metricsCollector.Collect(where, prefix+"completed", float64(completed))
		r.metricsCollector.Collect(where, prefix+"total", float64(total))
		r.metricsCollector.Collect(where, prefix+"progress-percent",
			percentOf(completed, total))
	}

	r.metricsCollector.Collect(where, "wg-completed", float64(totalCompleted))
	r.metricsCollector.Collect(where, "wg-total", float64(totalWG))
	r.metricsCollector.Collect(where, "wg-progress-percent",
		percentOf(totalCompleted, totalWG))
}

func percentOf(completed, total int) float64 {
	if total <= 0 {
		return 100.0
	}
	return float64(completed) / float64(total) * 100.0
}

func (r *Runner) reportRemotePtw() {
	if *pteCaching {
		return
	}

	totalNumPerHopTraced := r.RemotePtwTracer.GetNumPerHopTraced()
	totalNumPerHopRequired := r.RemotePtwTracer.GetNumPerHopRequired()
	totalAvgLatencyPerHop := r.RemotePtwTracer.GetAvgLatencyPerHopTraced()
	totalRemotePtwNum := r.RemotePtwTracer.GetTotalRdmaReqs()
	totalC2CTraced := r.RemotePtwTracer.GetC2CTraced()

	numChiplet := *xDim * *yDim

	r.metricsCollector.Collect(
		"GPU1.chiplet_all.ChipRDMA",
		"total-remote-ptw-num",
		float64(totalRemotePtwNum))

	for h, n := range totalNumPerHopRequired {
		r.metricsCollector.Collect(
			"GPU1.chiplet_all.ChipRDMA",
			"number-ptw-hop-required-"+strconv.Itoa(h),
			float64(n))
	}

	for h, n := range totalNumPerHopTraced {
		if n == 0 {
			continue
		}

		r.metricsCollector.Collect(
			"GPU1.chiplet_all.ChipRDMA",
			"number-ptw-hop-traced-"+strconv.Itoa(h),
			float64(n))
	}
	for h, l := range totalAvgLatencyPerHop {
		r.metricsCollector.Collect(
			"GPU1.chiplet_all.ChipRDMA",
			"average-latency-ptw-hop-traced-"+strconv.Itoa(h),
			l)
	}

	for srcChip := range numChiplet {
		for dstChip := range numChiplet {
			if totalC2CTraced[srcChip][dstChip] == 0 {
				continue
			}
			r.metricsCollector.Collect(
				"Chip-to-chip-ptw-traced",
				strconv.Itoa(srcChip)+"-"+strconv.Itoa(dstChip),
				float64(totalC2CTraced[srcChip][dstChip]))
		}
	}
}

// FIXME: we measure the hop count only for "request", currently.
func (r *Runner) reportHopPerRDMA() {
	if !r.ReportHopPerRDMA {
		return
	}
	totalNumPerHopTraced := r.HopTracer.GetNumPerHopTraced()
	totalNumPerHopRequired := r.HopTracer.GetNumPerHopRequired()
	totalAvgLatencyPerHop := r.HopTracer.GetAvgLatencyPerHopTraced()
	totalRdmaReqNum := r.HopTracer.GetTotalRdmaReqs()
	totalRdmaPerChiplet := r.HopTracer.GetRdmaPerChiplet()
	totalRdmaPerPC := r.HopTracer.GetRdmaPerPC()
	totalC2CTraced := r.HopTracer.GetC2CTraced()

	numChiplet := *xDim * *yDim

	r.metricsCollector.Collect(
		"GPU1.chiplet_all.ChipRDMA",
		"total-chip-rdma-num",
		float64(totalRdmaReqNum))

	for h, n := range totalNumPerHopRequired {
		r.metricsCollector.Collect(
			"GPU1.chiplet_all.ChipRDMA",
			"number-hop-required-"+strconv.Itoa(h),
			float64(n))
	}

	var hopWeightedSum uint64
	var hopTotalCount uint64
	for h, n := range totalNumPerHopTraced {
		if n == 0 {
			continue
		}

		r.metricsCollector.Collect(
			"GPU1.chiplet_all.ChipRDMA",
			"number-hop-traced-"+strconv.Itoa(h),
			float64(n))

		hopWeightedSum += uint64(h) * n
		hopTotalCount += n
	}
	if hopTotalCount > 0 {
		r.metricsCollector.Collect(
			"GPU1.chiplet_all.ChipRDMA",
			"average-hop-traced",
			float64(hopWeightedSum)/float64(hopTotalCount))
	}
	var latencyWeightedSum float64
	var latencyTotalCount uint64
	for h, l := range totalAvgLatencyPerHop {
		r.metricsCollector.Collect(
			"GPU1.chiplet_all.ChipRDMA",
			"average-latency-hop-traced-"+strconv.Itoa(h),
			l)

		if h >= len(totalNumPerHopTraced) {
			continue
		}
		n := totalNumPerHopTraced[h]
		if n == 0 {
			continue
		}
		latencyWeightedSum += l * float64(n)
		latencyTotalCount += n
	}
	if latencyTotalCount > 0 {
		r.metricsCollector.Collect(
			"GPU1.chiplet_all.ChipRDMA",
			"average-latency-hop-traced",
			latencyWeightedSum/float64(latencyTotalCount))
	}

	// Request-side / response-side split of the same summed round trip,
	// binned by the identical traced-hop key (unlike the per-flight
	// network/rdma flight tracers).
	reqSideLatencyPerHop := r.HopTracer.GetAvgReqSideLatencyPerHopTraced()
	rspSideLatencyPerHop := r.HopTracer.GetAvgRspSideLatencyPerHopTraced()
	for h, l := range reqSideLatencyPerHop {
		r.metricsCollector.Collect(
			"GPU1.chiplet_all.ChipRDMA",
			"average-req-latency-hop-traced-"+strconv.Itoa(h),
			l)
	}
	for h, l := range rspSideLatencyPerHop {
		r.metricsCollector.Collect(
			"GPU1.chiplet_all.ChipRDMA",
			"average-rsp-latency-hop-traced-"+strconv.Itoa(h),
			l)
	}

	for h, n := range totalRdmaPerChiplet {
		r.metricsCollector.Collect(
			"GPU1.chiplet_all.ChipRDMA",
			"number-rdma-per-chiplet-"+strconv.Itoa(h),
			float64(n))
	}

	for pc, n := range totalRdmaPerPC {
		r.metricsCollector.Collect(
			"GPU1.chiplet_all.ChipRDMA",
			"number-rdma-per-pc-"+pc,
			float64(n))
	}

	for srcChip := range numChiplet {
		for dstChip := range numChiplet {
			if totalC2CTraced[srcChip][dstChip] == 0 {
				continue
			}
			r.metricsCollector.Collect(
				"Chip-to-chip-traced",
				strconv.Itoa(srcChip)+"-"+strconv.Itoa(dstChip),
				float64(totalC2CTraced[srcChip][dstChip]))
		}
	}

	totalC2CFlitTraced := r.HopTracer.GetC2CFlitTraced()
	for srcChip := range numChiplet {
		for dstChip := range numChiplet {
			if totalC2CFlitTraced[srcChip][dstChip] == 0 {
				continue
			}
			r.metricsCollector.Collect(
				"Chip-to-chip-flit-traced",
				strconv.Itoa(srcChip)+"-"+strconv.Itoa(dstChip),
				float64(totalC2CFlitTraced[srcChip][dstChip]))
		}
	}

	reqsReceivedPerChiplet := r.HopTracer.GetNumReqsReceivedPerChiplet()
	for dstChip, n := range reqsReceivedPerChiplet {
		r.metricsCollector.Collect(
			"GPU1.chiplet_all.ChipRDMA",
			"rdma-reqs-received-chiplet-"+strconv.Itoa(dstChip),
			float64(n))
	}
}

// reportIBPA emits the Inter-module Bandwidth Per Access metric:
// IBPA = ibpaFlitHopSum / (localLLCMiss + remoteLLCMiss) = total flit*hops on
// the inter-chiplet network per LLC miss. The numerator is the switch-step
// flit-hop sum (AddFlitHops in each switch's traceHop, ID-independent).
func (r *Runner) reportIBPA() {
	flitHops := tracing.GetIBPAFlitHopSum()
	localMiss := tracing.GetIBPALocalLLCMiss()
	remoteMiss := tracing.GetIBPARemoteLLCMiss()
	n := localMiss + remoteMiss

	ibpa := 0.0
	if n > 0 {
		ibpa = float64(flitHops) / float64(n)
	}

	r.metricsCollector.Collect(
		"GPU1.chiplet_all.ChipRDMA", "IBPA", ibpa)
	r.metricsCollector.Collect(
		"GPU1.chiplet_all.ChipRDMA", "IBPA-flit-hop-sum", float64(flitHops))
	r.metricsCollector.Collect(
		"GPU1.chiplet_all.ChipRDMA", "IBPA-local-llc-miss", float64(localMiss))
	r.metricsCollector.Collect(
		"GPU1.chiplet_all.ChipRDMA", "IBPA-remote-llc-miss", float64(remoteMiss))
}

// reportRDMAFlightLatency emits per-hop bins for two complementary flight
// metrics: network-only (endpoint↔endpoint) and end-to-end RDMA
// (rdma↔rdma). Contrasts with reportHopPerRDMA which measures
// source-RDMA-Send → source-RDMA-forward-rsp-to-L2 round-trip.
func (r *Runner) reportRDMAFlightLatency() {
	if !r.ReportHopPerRDMA {
		return
	}

	if r.NetworkFlightTracer != nil {
		r.emitNetworkFlightDirection(
			tracing.NetworkFlightKindReq, "network-req-flight")
		r.emitNetworkFlightDirection(
			tracing.NetworkFlightKindRsp, "network-rsp-flight")
	}

	if r.RDMAFlightTracer != nil {
		r.emitRDMAFlightDirection(
			tracing.RDMAFlightKindReq, "rdma-req-flight")
		r.emitRDMAFlightDirection(
			tracing.RDMAFlightKindRsp, "rdma-rsp-flight")
	}
}

func (r *Runner) emitNetworkFlightDirection(kind, label string) {
	counts := r.NetworkFlightTracer.GetCountPerHop(kind)
	avgs := r.NetworkFlightTracer.GetAvgLatencyPerHop(kind)
	total := r.NetworkFlightTracer.GetTotalCompleted(kind)
	r.emitFlightCSV(label, counts, avgs, total)
}

func (r *Runner) emitRDMAFlightDirection(kind, label string) {
	counts := r.RDMAFlightTracer.GetCountPerHop(kind)
	avgs := r.RDMAFlightTracer.GetAvgLatencyPerHop(kind)
	total := r.RDMAFlightTracer.GetTotalCompleted(kind)
	r.emitFlightCSV(label, counts, avgs, total)
}

func (r *Runner) emitFlightCSV(
	label string, counts []uint64, avgs []float64, total int,
) {
	r.metricsCollector.Collect(
		"GPU1.chiplet_all.ChipRDMA",
		"total-"+label+"-num",
		float64(total))

	for h, n := range counts {
		if n == 0 {
			continue
		}
		r.metricsCollector.Collect(
			"GPU1.chiplet_all.ChipRDMA",
			"number-"+label+"-hop-"+strconv.Itoa(h),
			float64(n))
	}
	for h, l := range avgs {
		if counts[h] == 0 {
			continue
		}
		r.metricsCollector.Collect(
			"GPU1.chiplet_all.ChipRDMA",
			"average-latency-"+label+"-hop-"+strconv.Itoa(h),
			l)
	}
}

// reportLinkUtilization emits per-direction bandwidth utilization for
// every switch-to-switch link in the mesh interconnect. Utilization =
// admittedFlits / (pipelineWidth × elapsedCycles); 0.0 means idle link,
// 1.0 means the link was saturated every cycle. Device-to-switch links
// (RDMA <-> first-hop switch) are skipped — they measure injection
// rate, not the inter-chiplet wire.
//
// CSV row format: <metric-collector-group> "Link" / <a>-<b>-<net>-link
// where <a>-<b> is the directed chiplet pair and <net> is "req" or
// "rsp" (the request and response networks are wired as separate
// physical links). So 0-1-req-link is the req-network link from
// chiplet 0 to chiplet 1; 1-0-req-link is its reverse direction.
func (r *Runner) reportLinkUtilization() {
	if !r.ReportHopPerRDMA {
		return
	}

	now := r.Engine.CurrentTime()

	for _, gpu := range r.GPUDriver.GPUs {
		for _, net := range gpu.InterChipletNetwork {
			sc := net.GetSwitchConnector()
			if sc == nil {
				continue
			}
			for _, conn := range sc.GetSwitchConnection() {
				r.emitOneLinkUtil(conn, now)
			}
		}
	}
}

func (r *Runner) emitOneLinkUtil(
	conn *switching.MultiFlitSwitchConnection,
	now akita.VTimeInSec,
) {
	leftName := conn.LeftPortName()
	rightName := conn.RightPortName()

	leftChip, leftOk := chipletIdFromSwitchPortName(leftName)
	rightChip, rightOk := chipletIdFromSwitchPortName(rightName)
	if !leftOk || !rightOk {
		return
	}

	network := "req"
	if strings.Contains(leftName, ".Rsp") {
		network = "rsp"
	}

	cycles := conn.Cycles(now)
	if cycles == 0 {
		return
	}
	denom := float64(uint64(conn.PipelineWidth()) * cycles)
	if denom == 0 {
		return
	}

	r.metricsCollector.Collect(
		"Link",
		fmt.Sprintf("%d-%d-%s-link", leftChip, rightChip, network),
		float64(conn.AdmittedLeftToRight())/denom)
	r.metricsCollector.Collect(
		"Link",
		fmt.Sprintf("%d-%d-%s-link", rightChip, leftChip, network),
		float64(conn.AdmittedRightToLeft())/denom)
}

// chipletIdFromSwitchPortName parses a switch port name like
// "InterChipletMesh.SW_[y][x].ReqPort1" and returns the chiplet ID
// (y*xDim + x). Returns (0, false) if the name doesn't carry a
// SW_[y][x] tag (e.g. EndPoint side of a device-to-switch link).
func chipletIdFromSwitchPortName(name string) (int, bool) {
	idx := strings.Index(name, "SW_[")
	if idx < 0 {
		return 0, false
	}
	rest := name[idx+len("SW_["):]
	yEnd := strings.Index(rest, "]")
	if yEnd < 0 || yEnd+2 > len(rest) {
		return 0, false
	}
	y, err := strconv.Atoi(rest[:yEnd])
	if err != nil {
		return 0, false
	}
	rest = rest[yEnd+2:] // skip "]["
	xEnd := strings.Index(rest, "]")
	if xEnd < 0 {
		return 0, false
	}
	x, err := strconv.Atoi(rest[:xEnd])
	if err != nil {
		return 0, false
	}
	return y**xDim + x, true
}

// reportSendOutFail emits per-switch and per-(switch,port) counts of
// pipeline stalls in each switch's sendOut stage (downstream link could
// not accept the flit). A high count localizes the saturated link.
func (r *Runner) reportSendOutFail() {
	if !r.ReportHopPerRDMA || r.SendOutFailTracer == nil {
		return
	}

	r.metricsCollector.Collect(
		"GPU1.chiplet_all.Switch",
		"total-sendout-fail",
		float64(r.SendOutFailTracer.GetTotal()))

	perSwitch := r.SendOutFailTracer.GetPerSwitch()
	swNames := make([]string, 0, len(perSwitch))
	for sw := range perSwitch {
		swNames = append(swNames, sw)
	}
	sort.Strings(swNames)
	for _, sw := range swNames {
		r.metricsCollector.Collect(
			sw, "sendout-fail", float64(perSwitch[sw]))
	}

	perPort := r.SendOutFailTracer.GetPerPort()
	for _, sw := range swNames {
		ports := perPort[sw]
		portNames := make([]string, 0, len(ports))
		for p := range ports {
			portNames = append(portNames, p)
		}
		sort.Strings(portNames)
		for _, p := range portNames {
			r.metricsCollector.Collect(
				sw,
				"sendout-fail-port-"+shortPortName(p, sw),
				float64(ports[p]))
		}
	}
}

// shortPortName trims the switch-name prefix from a fully-qualified port
// name so the CSV column reads like "sendout-fail-port-east_local"
// instead of "sendout-fail-port-InterChipletMesh.SW_[0][0].east_local".
func shortPortName(port, sw string) string {
	if strings.HasPrefix(port, sw+".") {
		return port[len(sw)+1:]
	}
	return port
}

// reportPortQueue emits per-port average and max queueing-wait time
// (Recv→Retrieve) for ports the PortQueueTracer was attached to —
// typically endpoint NetworkPorts and RDMA engine ports. High avg/max
// localizes where messages pile up inside port buffers, separate from
// network flight or downstream processing time.
func (r *Runner) reportPortQueue() {
	if !r.TracePortQueue || r.PortQueueTracer == nil {
		return
	}

	count, sumSec, maxSec := r.PortQueueTracer.GetTotal()
	r.emitPortQueueTotals(count, sumSec, maxSec)
	r.emitPortQueueByPipelineStage()
	r.emitPortQueuePerType()
	r.emitPortQueuePerPort()
}

func (r *Runner) emitPortQueueTotals(count uint64, sumSec, maxSec float64) {
	var avg float64
	if count > 0 {
		avg = sumSec / float64(count)
	}
	r.metricsCollector.Collect(
		"GPU1.chiplet_all.PortQueue",
		"total-port-queue-count", float64(count))
	r.metricsCollector.Collect(
		"GPU1.chiplet_all.PortQueue",
		"total-port-queue-wait-avg", avg)
	r.metricsCollector.Collect(
		"GPU1.chiplet_all.PortQueue",
		"total-port-queue-wait-max", maxSec)
}

func (r *Runner) emitPortQueuePerPort() {
	perPort := r.PortQueueTracer.GetPerPort()
	names := make([]string, 0, len(perPort))
	for n := range perPort {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		s := perPort[n]
		var avg float64
		if s.Count > 0 {
			avg = s.TotalSec / float64(s.Count)
		}
		r.metricsCollector.Collect(n, "port-queue-count",
			float64(s.Count))
		r.metricsCollector.Collect(n, "port-queue-wait-avg", avg)
		r.metricsCollector.Collect(n, "port-queue-wait-max", s.MaxSec)
	}
}

// emitPortQueuePerType aggregates per-port wait stats across all chiplets
// for ports of the same type — e.g. every chiplet's ChipRDMA.RequestPort
// collapses into one "ChipRDMA.RequestPort" row. The type key is derived
// from the full port name by stripping the GPU<N>. and chiplet_<NN>.
// prefixes (see portTypeKey). Useful for spotting which class of port
// (RDMA Req vs Rsp vs ToL2 vs endpoint NetworkPort) dominates queueing.
func (r *Runner) emitPortQueuePerType() {
	perType := r.aggregatePortQueueByType()
	types := make([]string, 0, len(perType))
	for t := range perType {
		types = append(types, t)
	}
	sort.Strings(types)
	for _, t := range types {
		r.emitPortQueueTypeRow(t, perType[t])
	}
}

func (r *Runner) aggregatePortQueueByType() map[string]*tracing.PortQueueStats {
	perPort := r.PortQueueTracer.GetPerPort()
	perType := make(map[string]*tracing.PortQueueStats)
	for name, s := range perPort {
		t := portTypeKey(name)
		agg := perType[t]
		if agg == nil {
			agg = &tracing.PortQueueStats{}
			perType[t] = agg
		}
		agg.Count += s.Count
		agg.TotalSec += s.TotalSec
		if s.MaxSec > agg.MaxSec {
			agg.MaxSec = s.MaxSec
		}
	}
	return perType
}

func (r *Runner) emitPortQueueTypeRow(t string, s *tracing.PortQueueStats) {
	var avg float64
	if s.Count > 0 {
		avg = s.TotalSec / float64(s.Count)
	}
	where := "GPU1.chiplet_all.PortQueue"
	r.metricsCollector.Collect(where,
		"port-queue-type-"+t+"-count", float64(s.Count))
	r.metricsCollector.Collect(where,
		"port-queue-type-"+t+"-wait-avg", avg)
	r.metricsCollector.Collect(where,
		"port-queue-type-"+t+"-wait-sum", s.TotalSec)
	r.metricsCollector.Collect(where,
		"port-queue-type-"+t+"-wait-max", s.MaxSec)
}

// portTypeKey strips the GPU<N>. and chiplet_<NN>. prefixes from a port
// name so all chiplets' same-role ports collapse to one key. Examples:
//
//	GPU1.chiplet_00.ChipRDMA.RequestPort
//	  → ChipRDMA.RequestPort
//	GPU1.chiplet_07.ChipRDMA.ResponsePort.EP.NetworkPort
//	  → ChipRDMA.ResponsePort.EP.NetworkPort
//
// Names that don't match the expected prefix pattern pass through
// unchanged and form their own per-type row.
func portTypeKey(name string) string {
	s := stripPortPrefix(name, "GPU")
	s = stripPortPrefix(s, "chiplet_")
	return s
}

// pipelineStageKey maps a port-type key (after stripping GPU/chiplet prefix)
// to a human-readable pipeline stage name. Matching is done on the component
// name before the first dot.
func pipelineStageKey(typeKey string) string {
	comp := typeKey
	if dot := strings.Index(typeKey, "."); dot >= 0 {
		comp = typeKey[:dot]
	}
	switch {
	case strings.HasPrefix(comp, "CU_"):
		return "ComputeUnit"
	case strings.HasPrefix(comp, "L1VROB") ||
		strings.HasPrefix(comp, "L1SROB") ||
		strings.HasPrefix(comp, "L1IROB"):
		return "ROB"
	case strings.HasPrefix(comp, "L1VAddrTrans") ||
		strings.HasPrefix(comp, "L1SAddrTrans") ||
		strings.HasPrefix(comp, "L1IAddrTrans"):
		return "AddressTranslator"
	case strings.HasPrefix(comp, "L1VTLB") ||
		strings.HasPrefix(comp, "L1STLB") ||
		strings.HasPrefix(comp, "L1ITLB"):
		return "L1TLB"
	case comp == "L2TLB":
		return "L2TLB"
	case comp == "MMU":
		return "MMU"
	case strings.HasPrefix(comp, "L1VCache") ||
		strings.HasPrefix(comp, "L1SCache") ||
		strings.HasPrefix(comp, "L1ICache"):
		return "L1Cache"
	case strings.HasPrefix(comp, "L2_"):
		return "L2Cache"
	case strings.HasPrefix(comp, "DRAM_"):
		return "DRAM"
	case strings.HasPrefix(comp, "ChipRDMA") ||
		strings.HasPrefix(comp, "L2-RDMA"):
		return "ChipRDMA"
	default:
		return "Other"
	}
}

// emitPortQueueByPipelineStage aggregates per-port queue-wait stats into
// pipeline-stage buckets (ComputeUnit, ROB, AddressTranslator, L1TLB, L2TLB,
// MMU, L1Cache, L2Cache, DRAM, ChipRDMA, Other) and emits count / wait-avg /
// wait-max for each. Collapses across all chiplets and all instances so the
// reader sees one summary row per pipeline stage.
func (r *Runner) emitPortQueueByPipelineStage() {
	type stageStats struct {
		count    uint64
		totalSec float64
		maxSec   float64
	}
	stages := make(map[string]*stageStats)

	perPort := r.PortQueueTracer.GetPerPort()
	for name, s := range perPort {
		stage := pipelineStageKey(portTypeKey(name))
		agg := stages[stage]
		if agg == nil {
			agg = &stageStats{}
			stages[stage] = agg
		}
		agg.count += s.Count
		agg.totalSec += s.TotalSec
		if s.MaxSec > agg.maxSec {
			agg.maxSec = s.MaxSec
		}
	}

	stageNames := make([]string, 0, len(stages))
	for s := range stages {
		stageNames = append(stageNames, s)
	}
	sort.Strings(stageNames)

	where := "GPU1.chiplet_all.PortQueue"
	for _, stage := range stageNames {
		agg := stages[stage]
		var avg float64
		if agg.count > 0 {
			avg = agg.totalSec / float64(agg.count)
		}
		r.metricsCollector.Collect(where,
			"pipeline-stage-"+stage+"-count", float64(agg.count))
		r.metricsCollector.Collect(where,
			"pipeline-stage-"+stage+"-wait-avg", avg)
		r.metricsCollector.Collect(where,
			"pipeline-stage-"+stage+"-wait-max", agg.maxSec)
	}
}

func stripPortPrefix(s, prefix string) string {
	if !strings.HasPrefix(s, prefix) {
		return s
	}
	dot := strings.Index(s, ".")
	if dot < 0 {
		return s
	}
	return s[dot+1:]
}

func (r *Runner) reportL2MissTrace() {
	if !r.ReportL2MissTrace || r.L2MissTracer == nil {
		return
	}

	numChiplet := *xDim * *yDim

	rdmaCount := r.L2MissTracer.GetRdmaCount()
	hopCount := r.L2MissTracer.GetHopRequired()
	numC2C := r.L2MissTracer.GetC2C()

	r.metricsCollector.Collect(
		"GPU1.chiplet_all.ChipRDMA",
		"l2-miss-rdma-count",
		rdmaCount)

	for h, c := range hopCount {
		r.metricsCollector.Collect(
			"GPU1.chiplet_all.ChipRDMA",
			"l2-miss-hop-required-"+strconv.Itoa(h),
			float64(c))
	}

	for srcChip := range numChiplet {
		for dstChip := range numChiplet {
			if numC2C[srcChip][dstChip] == 0 {
				continue
			}
			r.metricsCollector.Collect(
				"GPU1.chiplet_"+strconv.Itoa(srcChip),
				"l2-miss-c2c-"+strconv.Itoa(srcChip)+"-"+strconv.Itoa(dstChip),
				float64(numC2C[srcChip][dstChip]))
		}
	}

	rdmaCountPerChiplet := make([]uint64, numChiplet)
	totalHopRequired := make([]uint64, numChiplet)

	for srcChip := range numChiplet {
		numberPerHop := make([]uint64, *xDim+*yDim-1)

		for dstChip := range numChiplet {
			srcX := srcChip % *xDim
			srcY := srcChip / *xDim
			dstX := dstChip % *xDim
			dstY := dstChip / *xDim

			hopRequired := uint64(Abs(srcY-dstY) + Abs(srcX-dstX))
			rdmaCountPerChiplet[srcChip] += numC2C[srcChip][dstChip]
			numberPerHop[hopRequired] += numC2C[srcChip][dstChip] * hopRequired
		}

		for h := range *xDim + *yDim - 1 {
			if numberPerHop[h] == 0 {
				continue
			}

			totalHopRequired[srcChip] += numberPerHop[h]
			r.metricsCollector.Collect(
				"GPU1.chiplet_"+strconv.Itoa(srcChip),
				"hop-count-per-chiplet-"+strconv.Itoa(srcChip)+"-"+strconv.Itoa(int(h)),
				float64(numberPerHop[h]))
		}
	}

	for srcChip := range numChiplet {
		r.metricsCollector.Collect(
			"GPU1.chiplet_"+strconv.Itoa(srcChip),
			"total-rdma-count-per-chiplet-"+strconv.Itoa(srcChip),
			float64(rdmaCountPerChiplet[srcChip]))
	}
	for srcChip := range numChiplet {
		r.metricsCollector.Collect(
			"GPU1.chiplet_"+strconv.Itoa(srcChip),
			"total-hop-count-per-chiplet-"+strconv.Itoa(srcChip),
			float64(totalHopRequired[srcChip]))
	}
}

func (r *Runner) reportMSHRFailTrace() {
	if !r.ReportMSHRFailTrace {
		return
	}

	r.collectMSHRFailMetrics(r.L1MSHRFailTracer, "l1")
	r.collectMSHRFailMetrics(r.L2MSHRFailTracer, "l2")
}

// collectMSHRFailMetrics emits every MSHR-fail / merge-fail / merge metric
// from a single tracer into the CSV with a level-qualified column prefix
// (e.g. "l1-mshr-..." or "l2-mshr-..."), so L1 and L2 pressure can be
// distinguished in downstream analysis.
func (r *Runner) collectMSHRFailMetrics(
	tracer *tracing.MSHRFailTracer,
	level string,
) {
	if tracer == nil {
		return
	}
	p := level + "-"

	readFail := tracer.GetReadFailCount()
	writeFail := tracer.GetWriteFailCount()

	r.metricsCollector.Collect(
		"GPU1.chiplet_all.Caches",
		p+"mshr-reservation-fail-read",
		float64(readFail))
	r.metricsCollector.Collect(
		"GPU1.chiplet_all.Caches",
		p+"mshr-reservation-fail-write",
		float64(writeFail))
	r.metricsCollector.Collect(
		"GPU1.chiplet_all.Caches",
		p+"mshr-reservation-fail-total",
		float64(readFail+writeFail))

	numChiplet := *xDim * *yDim
	readPerChip := tracer.GetReadFailPerChiplet()
	writePerChip := tracer.GetWriteFailPerChiplet()
	for srcChip := 0; srcChip < numChiplet; srcChip++ {
		r.metricsCollector.Collect(
			"GPU1.chiplet_"+strconv.Itoa(srcChip),
			p+"mshr-reservation-fail-read-"+strconv.Itoa(srcChip),
			float64(readPerChip[srcChip]))
		r.metricsCollector.Collect(
			"GPU1.chiplet_"+strconv.Itoa(srcChip),
			p+"mshr-reservation-fail-write-"+strconv.Itoa(srcChip),
			float64(writePerChip[srcChip]))
	}

	readMergeFail := tracer.GetReadMergeFailCount()
	writeMergeFail := tracer.GetWriteMergeFailCount()

	r.metricsCollector.Collect(
		"GPU1.chiplet_all.Caches",
		p+"mshr-merge-fail-read",
		float64(readMergeFail))
	r.metricsCollector.Collect(
		"GPU1.chiplet_all.Caches",
		p+"mshr-merge-fail-write",
		float64(writeMergeFail))
	r.metricsCollector.Collect(
		"GPU1.chiplet_all.Caches",
		p+"mshr-merge-fail-total",
		float64(readMergeFail+writeMergeFail))

	readMergeFailPerChip := tracer.GetReadMergeFailPerChiplet()
	writeMergeFailPerChip := tracer.GetWriteMergeFailPerChiplet()
	for srcChip := 0; srcChip < numChiplet; srcChip++ {
		r.metricsCollector.Collect(
			"GPU1.chiplet_"+strconv.Itoa(srcChip),
			p+"mshr-merge-fail-read-"+strconv.Itoa(srcChip),
			float64(readMergeFailPerChip[srcChip]))
		r.metricsCollector.Collect(
			"GPU1.chiplet_"+strconv.Itoa(srcChip),
			p+"mshr-merge-fail-write-"+strconv.Itoa(srcChip),
			float64(writeMergeFailPerChip[srcChip]))
	}

	mergeEvents := tracer.GetMergeEventCount()
	mergeReqsTotal := tracer.GetMergeRequestsTotal()
	avgMerge := float64(0)
	if mergeEvents > 0 {
		avgMerge = float64(mergeReqsTotal) / float64(mergeEvents)
	}

	r.metricsCollector.Collect(
		"GPU1.chiplet_all.Caches",
		p+"mshr-merge-event-count",
		float64(mergeEvents))
	r.metricsCollector.Collect(
		"GPU1.chiplet_all.Caches",
		p+"mshr-merge-requests-total",
		float64(mergeReqsTotal))
	r.metricsCollector.Collect(
		"GPU1.chiplet_all.Caches",
		p+"mshr-merge-requests-avg",
		avgMerge)

	for _, src := range []string{"buffer", "dram", "rdma"} {
		evts := tracer.GetMergeEventsBySource()[src]
		reqs := tracer.GetMergeRequestsBySource()[src]
		r.metricsCollector.Collect(
			"GPU1.chiplet_all.Caches",
			p+"mshr-merge-event-count-"+src,
			float64(evts))
		r.metricsCollector.Collect(
			"GPU1.chiplet_all.Caches",
			p+"mshr-merge-requests-total-"+src,
			float64(reqs))
	}

	histogram := tracer.GetMergeHistogram()
	bins := make([]int, 0, len(histogram))
	for k := range histogram {
		bins = append(bins, k)
	}
	sort.Ints(bins)
	for _, k := range bins {
		r.metricsCollector.Collect(
			"GPU1.chiplet_all.Caches",
			p+"mshr-merge-histogram-"+strconv.Itoa(k),
			float64(histogram[k]))
	}

	mergeEvtsPerChip := tracer.GetMergeEventsPerChiplet()
	mergeReqsPerChip := tracer.GetMergeRequestsPerChiplet()
	for srcChip := 0; srcChip < numChiplet; srcChip++ {
		r.metricsCollector.Collect(
			"GPU1.chiplet_"+strconv.Itoa(srcChip),
			p+"mshr-merge-event-count-"+strconv.Itoa(srcChip),
			float64(mergeEvtsPerChip[srcChip]))
		r.metricsCollector.Collect(
			"GPU1.chiplet_"+strconv.Itoa(srcChip),
			p+"mshr-merge-requests-total-"+strconv.Itoa(srcChip),
			float64(mergeReqsPerChip[srcChip]))
	}

	r.metricsCollector.Collect(
		"GPU1.chiplet_all",
		p+"mshr-wait-time-total",
		tracer.GetWaitTimeTotal())
	r.metricsCollector.Collect(
		"GPU1.chiplet_all",
		p+"mshr-wait-time-count",
		float64(tracer.GetWaitTimeCount()))
	r.metricsCollector.Collect(
		"GPU1.chiplet_all",
		p+"mshr-wait-time-avg",
		tracer.GetWaitTimeAvg())

	waitTimeTotalPerChip := tracer.GetWaitTimeTotalPerChiplet()
	waitTimeCountPerChip := tracer.GetWaitTimeCountPerChiplet()
	for srcChip := 0; srcChip < numChiplet; srcChip++ {
		r.metricsCollector.Collect(
			"GPU1.chiplet_"+strconv.Itoa(srcChip),
			p+"mshr-wait-time-total-"+strconv.Itoa(srcChip),
			waitTimeTotalPerChip[srcChip])
		r.metricsCollector.Collect(
			"GPU1.chiplet_"+strconv.Itoa(srcChip),
			p+"mshr-wait-time-count-"+strconv.Itoa(srcChip),
			float64(waitTimeCountPerChip[srcChip]))
		var chipAvg float64
		if waitTimeCountPerChip[srcChip] > 0 {
			chipAvg = waitTimeTotalPerChip[srcChip] / float64(waitTimeCountPerChip[srcChip])
		}
		r.metricsCollector.Collect(
			"GPU1.chiplet_"+strconv.Itoa(srcChip),
			p+"mshr-wait-time-avg-"+strconv.Itoa(srcChip),
			chipAvg)
	}
}

func Abs(x int) int {
	if x < 0 {
		return -x
	}
	return x
}

// reportSetAccessHistogram emits per-L2-bank per-setID access counts
// into the main metrics CSV. Each bank produces NumSets rows of the
// form (entity=cache.Name(), metric="set-access-<setID>", value).
// Gated by -trace-set-access. Only writeback.Cache L2s are read.
func (r *Runner) reportSetAccessHistogram() {
	if !*setAccessTraceFlag {
		return
	}

	for _, gpu := range r.GPUDriver.GPUs {
		for _, c := range gpu.L2Caches {
			wbCache, ok := c.(*writeback.Cache)
			if !ok {
				continue
			}
			dir, ok := wbCache.GetDirectory().(*cache.DirectoryImpl)
			if !ok {
				continue
			}
			counts := dir.GetSetAccessCount()
			for setID, n := range counts {
				r.metricsCollector.Collect(
					wbCache.Name(),
					"set-access-"+strconv.Itoa(setID),
					float64(n))
			}
		}
	}
}

func (r *Runner) reportTLBMSHRWait() {
	if !r.ReportTLBMSHRWait {
		return
	}

	totalL1Stall := uint64(0)
	totalL1HitWaitCount := uint64(0)
	totalL1HitWaitTime := akita.VTimeInSec(0)

	for _, t := range r.L1TLBMSHRWaitTracers {
		stall := t.tracer.GetStallCount()
		hitWaitCount := t.tracer.GetHitWaitCount()
		hitWaitTime := t.tracer.GetHitWaitTotalTime()

		totalL1Stall += stall
		totalL1HitWaitCount += hitWaitCount
		totalL1HitWaitTime += hitWaitTime

		r.metricsCollector.Collect(
			t.tlb.Name(), "tlb-mshr-stall-count", float64(stall))
		r.metricsCollector.Collect(
			t.tlb.Name(), "tlb-mshr-hit-wait-count", float64(hitWaitCount))
		if hitWaitCount > 0 {
			r.metricsCollector.Collect(
				t.tlb.Name(), "tlb-mshr-hit-wait-avg-time",
				float64(hitWaitTime)/float64(hitWaitCount))
		}
	}

	r.metricsCollector.Collect(
		"GPU1.chiplet_all.L1TLBs", "tlb-mshr-stall-count", float64(totalL1Stall))
	r.metricsCollector.Collect(
		"GPU1.chiplet_all.L1TLBs", "tlb-mshr-hit-wait-count", float64(totalL1HitWaitCount))
	if totalL1HitWaitCount > 0 {
		r.metricsCollector.Collect(
			"GPU1.chiplet_all.L1TLBs", "tlb-mshr-hit-wait-avg-time",
			float64(totalL1HitWaitTime)/float64(totalL1HitWaitCount))
	}

	totalL2Stall := uint64(0)
	totalL2HitWaitCount := uint64(0)
	totalL2HitWaitTime := akita.VTimeInSec(0)

	for _, t := range r.L2TLBMSHRWaitTracers {
		stall := t.tracer.GetStallCount()
		hitWaitCount := t.tracer.GetHitWaitCount()
		hitWaitTime := t.tracer.GetHitWaitTotalTime()

		totalL2Stall += stall
		totalL2HitWaitCount += hitWaitCount
		totalL2HitWaitTime += hitWaitTime

		r.metricsCollector.Collect(
			t.tlb.Name(), "tlb-mshr-stall-count", float64(stall))
		r.metricsCollector.Collect(
			t.tlb.Name(), "tlb-mshr-hit-wait-count", float64(hitWaitCount))
		if hitWaitCount > 0 {
			r.metricsCollector.Collect(
				t.tlb.Name(), "tlb-mshr-hit-wait-avg-time",
				float64(hitWaitTime)/float64(hitWaitCount))
		}
	}

	r.metricsCollector.Collect(
		"GPU1.chiplet_all.L2TLBs", "tlb-mshr-stall-count", float64(totalL2Stall))
	r.metricsCollector.Collect(
		"GPU1.chiplet_all.L2TLBs", "tlb-mshr-hit-wait-count", float64(totalL2HitWaitCount))
	if totalL2HitWaitCount > 0 {
		r.metricsCollector.Collect(
			"GPU1.chiplet_all.L2TLBs", "tlb-mshr-hit-wait-avg-time",
			float64(totalL2HitWaitTime)/float64(totalL2HitWaitCount))
	}
}
