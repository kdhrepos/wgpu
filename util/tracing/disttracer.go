// DistTracer reports every access count per memory address
package tracing

import (
	"fmt"
	"os"
	"slices"
	"time"

	"gitlab.com/akita/akita"
	"gitlab.com/akita/protocol"
)

type DistType int

const (
	TYPE_DIST DistType = iota
	TYPE_EVICT_LOCAL_HOME_CACHELINE
	TYPE_OVERWRITE_LOCAL_HOME_CACHELINE
	TYPE_HOTNESS_DEMOTE_COOLING // hotness -1 via ApplyCooling (passed-over set)
	TYPE_HOTNESS_REASSIGN_DIST  // hotness overwritten by re-distribution (already-exists path)
)

type DistInfo struct {
	Addr         uint64
	PC           uint64
	Home         uint64
	Hotness      protocol.Hotness
	ToDistribute int
	ToResponse   int
	DistType     DistType
}

type DistTracer struct {
	distCount                           uint64
	distHitCount                        uint64
	distCountPerChiplet                 []uint64
	distSendCountPerChiplet             []uint64
	distCountPerPC                      map[uint64]uint64
	distFromLocalHomeCount              uint64
	distEvictCount                      uint64
	distOverwriteCount                  uint64
	distAlreadyExistsCount              uint64
	distPrefetchCount                   uint64
	distBypassedCount                   uint64
	distBypassedByLookupCount           uint64
	distBypassedByHotnessCount          uint64
	distWarmNonSharedCount              uint64
	distHotNonSharedCount               uint64
	distBypassedByHotnessLocalHomeCount uint64
	localHomeEvictCount                 uint64
	localHomeOverwriteCount             uint64

	distCoolingGlobalToHotCount uint64
	distCoolingHotToWarmCount   uint64
	distCoolingWarmToColdCount  uint64
	distReassignCount           uint64
	numKernels                          int

	hotnessCountmap      map[protocol.Hotness]uint64
	fromLocalHomeHotness map[protocol.Hotness]uint64

	distributionPerTime map[akita.VTimeInSec]map[uint64]map[uint64]*DistInfo

	simulationStartTime       time.Time
	distributionTraceFileName string

	xDim int
	yDim int
}

func NewDistTracer(
	xDim int,
	yDim int,
	name string,
) *DistTracer {
	t := &DistTracer{
		xDim:                      xDim,
		yDim:                      yDim,
		distCountPerChiplet:       make([]uint64, xDim*yDim),
		distSendCountPerChiplet:   make([]uint64, xDim*yDim),
		distCountPerPC:            make(map[uint64]uint64),
		hotnessCountmap:           make(map[protocol.Hotness]uint64),
		fromLocalHomeHotness:      make(map[protocol.Hotness]uint64),
		distributionPerTime:       make(map[akita.VTimeInSec]map[uint64]map[uint64]*DistInfo),
		simulationStartTime:       time.Now(),
		distributionTraceFileName: name,
	}

	return t
}

func (t *DistTracer) StartTask(task Task) {

	switch task.Kind {
	case "distribution-from-local-home":
		hotness, ok := task.Detail.(protocol.Hotness)
		if !ok {
			return
		}

		t.distFromLocalHomeCount++
		t.fromLocalHomeHotness[hotness]++
	case "distribution-trace", "evict-local-home-cacheline", "overwrite-local-home-cacheline":
		distInfo, ok := task.Detail.(*DistInfo)
		if !ok {
			return
		}

		switch distInfo.DistType {
		case TYPE_EVICT_LOCAL_HOME_CACHELINE:
			t.localHomeEvictCount++
		case TYPE_OVERWRITE_LOCAL_HOME_CACHELINE:
			t.localHomeOverwriteCount++
		case TYPE_HOTNESS_DEMOTE_COOLING:
			// distInfo.Hotness carries the after-demotion value
			switch distInfo.Hotness {
			case protocol.Hot:
				t.distCoolingGlobalToHotCount++
			case protocol.Warm:
				t.distCoolingHotToWarmCount++
			case protocol.Cold:
				t.distCoolingWarmToColdCount++
			}
		case TYPE_HOTNESS_REASSIGN_DIST:
			t.distReassignCount++
		}

		addr := distInfo.Addr
		pc := distInfo.PC
		home := distInfo.Home
		hotness := distInfo.Hotness
		currentTime := task.StartTime

		distributionPerTime, ok := t.distributionPerTime[currentTime]
		if !ok {
			distributionPerTime = make(map[uint64]map[uint64]*DistInfo)
			t.distributionPerTime[currentTime] = distributionPerTime
		}

		distributionPerAddr, ok := distributionPerTime[addr]
		if !ok {
			distributionPerAddr = make(map[uint64]*DistInfo)
			distributionPerTime[addr] = distributionPerAddr
		}

		distributionPerAddr[pc|uint64(distInfo.DistType)<<48] = &DistInfo{
			Addr:         addr,
			PC:           pc,
			Home:         home,
			ToDistribute: distInfo.ToDistribute,
			ToResponse:   distInfo.ToResponse,
			Hotness:      hotness,
			DistType:     distInfo.DistType,
		}

		if time.Since(t.simulationStartTime) >= time.Hour {
			dataToDump := t.distributionPerTime
			t.distributionPerTime = make(map[akita.VTimeInSec]map[uint64]map[uint64]*DistInfo)
			t.simulationStartTime = time.Now()

			t.Dump(dataToDump)
		}
	case "hotness-trace":
		hotness, ok := task.Detail.(protocol.Hotness)
		if !ok {
			return
		}

		t.hotnessCountmap[hotness]++
	case "distribution":
		chipletId, ok := task.Detail.(int)
		if !ok {
			return
		}

		t.distCountPerChiplet[chipletId]++
		t.distCount++
	case "distribution-send-count":
		chipletId, ok := task.Detail.(int)
		if !ok {
			return
		}
		t.distSendCountPerChiplet[chipletId]++
	case "distribution-per-pc":
		pc, ok := task.Detail.(uint64)
		if !ok {
			return
		}

		t.distCountPerPC[pc]++
	case "distribution-hit":
		t.distHitCount++
	case "evict-by-distribution":
		t.distEvictCount++
	case "overwrite-by-distribution":
		t.distOverwriteCount++
	case "already-exists":
		t.distAlreadyExistsCount++
	case "distribution-prefetch":
		t.distPrefetchCount++
	case "bypassed-by-streaming":
		t.distBypassedCount++
	case "bypassed-by-lookup":
		t.distBypassedByLookupCount++
	case "distribution-non-shared":
		hotness, ok := task.Detail.(protocol.Hotness)
		if !ok {
			return
		}

		switch hotness {
		case protocol.Warm_NonShared:
			t.distWarmNonSharedCount++
		case protocol.Hot_NonShared:
			t.distHotNonSharedCount++
		}
	case "bypassed-by-hotness":
		t.distBypassedByHotnessCount++
	case "bypassed-by-hotness-local-home":
		t.distBypassedByHotnessLocalHomeCount++
	default:
		return
	}
}

func (t *DistTracer) StepTask(task Task) {
	// Do nothing
}

func (t *DistTracer) EndTask(task Task) {
	// Do nothing
}

func (t *DistTracer) Dump(
	data map[akita.VTimeInSec]map[uint64]map[uint64]*DistInfo,
) {
	dumpFromCall := false
	if data == nil {
		data = t.distributionPerTime
		dumpFromCall = true
	}

	var f *os.File
	var err error

	_, statErr := os.Stat(t.distributionTraceFileName + ".csv")
	if os.IsNotExist(statErr) {
		f, err = os.Create(t.distributionTraceFileName + ".csv")
		if err == nil {
			fmt.Fprintf(f, "time,addr,pc,requestor,local_home,global_home,hotness,type\n")
		}
	} else {
		f, err = os.OpenFile(t.distributionTraceFileName+".csv", os.O_APPEND|os.O_WRONLY, 0644)
	}

	if err != nil {
		panic(err)
	}
	defer f.Close()

	times := make([]akita.VTimeInSec, 0, len(data))
	for time := range data {
		times = append(times, time)
	}
	slices.Sort(times)

	for _, time := range times {
		addrMap := data[time]
		for addr, pcMap := range addrMap {
			for pc, info := range pcMap {
				fmt.Fprintf(f, "%.10f,%d,%d,%d,%d,%d,%d,%d\n",
					time, addr, pc, info.ToResponse, info.ToDistribute, info.Home, info.Hotness, info.DistType)
			}
		}
	}

	if dumpFromCall {
		t.distributionPerTime = make(map[akita.VTimeInSec]map[uint64]map[uint64]*DistInfo)
		t.simulationStartTime = time.Now()
	}
}

func (t *DistTracer) GetDistCount() uint64 {
	return t.distCount
}

func (t *DistTracer) GetDistHitCount() uint64 {
	return t.distHitCount
}

func (t *DistTracer) GetDistEvictCount() uint64 {
	return t.distEvictCount
}

func (t *DistTracer) GetDistOverwriteCount() uint64 {
	return t.distOverwriteCount
}

func (t *DistTracer) GetLocalHomeEvictCount() uint64 {
	return t.localHomeEvictCount
}

func (t *DistTracer) GetLocalHomeOverwriteCount() uint64 {
	return t.localHomeOverwriteCount
}

func (t *DistTracer) GetDistCountPerChiplet() []uint64 {
	return t.distCountPerChiplet
}

func (t *DistTracer) GetDistSendCountPerChiplet() []uint64 {
	return t.distSendCountPerChiplet
}

func (t *DistTracer) GetDistCountPerPC() map[uint64]uint64 {
	return t.distCountPerPC
}

func (t *DistTracer) GetHotnessCountmap() map[protocol.Hotness]uint64 {
	return t.hotnessCountmap
}

func (t *DistTracer) GetFromLocalHomeHotness() map[protocol.Hotness]uint64 {
	return t.fromLocalHomeHotness
}

func (t *DistTracer) GetDistFromLocalHomeCount() uint64 {
	return t.distFromLocalHomeCount
}

func (t *DistTracer) GetDistAlreadyExistsCount() uint64 {
	return t.distAlreadyExistsCount
}

func (t *DistTracer) GetDynamicClusteringCount() uint64 {
	return t.distPrefetchCount
}

func (t *DistTracer) GetDistBypassedCount() uint64 {
	return t.distBypassedCount
}

func (t *DistTracer) GetDistBypassedByLookupCount() uint64 {
	return t.distBypassedByLookupCount
}

func (t *DistTracer) GetDistWarmNonSharedCount() uint64 {
	return t.distWarmNonSharedCount
}

func (t *DistTracer) GetDistHotNonSharedCount() uint64 {
	return t.distHotNonSharedCount
}

func (t *DistTracer) GetDistBypassedByHotnessCount() uint64 {
	return t.distBypassedByHotnessCount
}

func (t *DistTracer) GetDistBypassedByHotnessLocalHomeCount() uint64 {
	return t.distBypassedByHotnessLocalHomeCount
}

func (t *DistTracer) GetDistCoolingGlobalToHotCount() uint64 {
	return t.distCoolingGlobalToHotCount
}

func (t *DistTracer) GetDistCoolingHotToWarmCount() uint64 {
	return t.distCoolingHotToWarmCount
}

func (t *DistTracer) GetDistCoolingWarmToColdCount() uint64 {
	return t.distCoolingWarmToColdCount
}

func (t *DistTracer) GetDistReassignCount() uint64 {
	return t.distReassignCount
}
