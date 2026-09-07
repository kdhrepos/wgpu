// PteCachingTracer reports every access count per memory address
package tracing

import (
	"strconv"
	"strings"
)

type PteCachingTracer struct {
	readHitCount     uint64
	readMissCount    uint64
	readMshrHitCount uint64
	remotePtwCount   uint64
	localPtwCount    uint64

	inflightTasks map[uint64]*HopInfo
	xDim          int
	yDim          int
}

func NewPteCachingTracer(
	xDim int,
	yDim int,
) *PteCachingTracer {
	t := &PteCachingTracer{
		xDim:          xDim,
		yDim:          yDim,
		inflightTasks: make(map[uint64]*HopInfo),
	}

	return t
}

func (t *PteCachingTracer) StartTask(task Task) {

	switch task.Kind {
	case "pte-caching-read-hit":
		t.readHitCount++
	case "pte-caching-read-miss":
		t.readMissCount++
	case "pte-caching-read-mshr-hit":
		t.readMshrHitCount++
	case "pte-caching-remote-ptw":
		t.remotePtwCount++
	case "pte-caching-local-ptw":
		t.localPtwCount++
	case "pte-caching-remote-ptw-trace":
		if task.What == "retry" {
			if hopInfo, ok := t.inflightTasks[task.ID]; ok {
				hopInfo.startTime = float64(task.StartTime)
				hopInfo.hopTraced = 0
				hopInfo.elapsedTime = 0
				return
			} else {
				// if the task is not in the inflightTasks,
				// it means this msg is response.
				return
			}
		}

		srcChipId, _ := strconv.Atoi(
			strings.TrimPrefix(
				strings.Split(task.Where, ".")[1],
				"chiplet_"))

		t.inflightTasks[task.ID] = &HopInfo{
			startTime:   float64(task.StartTime),
			hopTraced:   0,
			hopRequired: t.SetHopRequired(task),
			replied:     false,
			elapsedTime: 0,
			srcChipId:   srcChipId,
		}
	case "pte-eviction":
		// Do nothing
	}
}

func (t *PteCachingTracer) StepTask(task Task) {

	hopInfo, ok := t.inflightTasks[task.ID]
	if !ok {
		return
	}

	hopInfo.hopTraced++
}

func (t *PteCachingTracer) EndTask(task Task) {

	hopInfo, ok := t.inflightTasks[task.ID]
	if !ok {
		return
	}

	dstChipId, _ := strconv.Atoi(
		strings.TrimPrefix(
			strings.Split(task.Where, ".")[1],
			"chiplet_"))

	hopInfo.elapsedTime = float64(task.EndTime) - hopInfo.startTime
	hopInfo.dstChipId = dstChipId
	hopInfo.replied = true
}

func (t *PteCachingTracer) GetReadHitCount() uint64 {
	return t.readHitCount
}

func (t *PteCachingTracer) GetReadMissCount() uint64 {
	return t.readMissCount
}

func (t *PteCachingTracer) GetReadMshrHitCount() uint64 {
	return t.readMshrHitCount
}

func (t *PteCachingTracer) GetRemotePtwCount() uint64 {
	return t.remotePtwCount
}

func (t *PteCachingTracer) GetLocalPtwCount() uint64 {
	return t.localPtwCount
}

func (t *PteCachingTracer) GetAvgLatencyPerHopTraced() []float64 {
	numPerHopTraced := make([]uint64, t.xDim*t.yDim)
	avgLatencyPerHopTraced := make([]float64, t.xDim*t.yDim)

	for _, task := range t.inflightTasks {
		if !task.replied {
			continue
		}

		hopTraced := task.hopTraced
		numPerHopTraced[hopTraced]++
		avgLatencyPerHopTraced[hopTraced] += task.elapsedTime
	}

	for h := range avgLatencyPerHopTraced {
		avgLatencyPerHopTraced[h] /= float64(numPerHopTraced[h])
	}

	return avgLatencyPerHopTraced
}

func (t *PteCachingTracer) GetNumPerHopTraced() []uint64 {
	numPerHopTraced := make([]uint64, t.xDim*t.yDim)

	for _, task := range t.inflightTasks {
		if !task.replied || task.hopTraced == 0 {
			continue
		}

		hopTraced := task.hopTraced
		numPerHopTraced[hopTraced]++
	}

	return numPerHopTraced
}

func (t *PteCachingTracer) GetC2CTraced() [][]int {
	numChiplets := t.xDim * t.yDim
	numC2CTraced := make([][]int, numChiplets)
	for i := 0; i < numChiplets; i++ {
		numC2CTraced[i] = make([]int, numChiplets)
	}

	for _, task := range t.inflightTasks {
		if !task.replied || task.hopTraced == 0 {
			continue
		}

		numC2CTraced[task.srcChipId][task.dstChipId]++
	}

	return numC2CTraced
}

func (t *PteCachingTracer) GetNumPerHopRequired() []uint64 {
	numPerHopRequired := make([]uint64, t.xDim+t.yDim-1)

	for _, task := range t.inflightTasks {
		numPerHopRequired[task.hopRequired]++
	}

	return numPerHopRequired
}

func (t *PteCachingTracer) GetTotalRdmaReqs() int {
	return len(t.inflightTasks)
}

func (t *PteCachingTracer) SetHopRequired(task Task) uint64 {
	srcChip, _ := strconv.Atoi(
		strings.TrimPrefix(
			strings.Split(task.Detail.(string), ".")[0],
			"chiplet_"))
	dstChip, _ := strconv.Atoi(
		strings.TrimPrefix(
			strings.Split(task.Detail.(string), ".")[1],
			"chiplet_"))

	srcX := srcChip % t.xDim
	srcY := srcChip / t.xDim
	dstX := dstChip % t.xDim
	dstY := dstChip / t.xDim

	hopRequired := uint64(Abs(srcY-dstY) + Abs(srcX-dstX))

	return hopRequired
}
