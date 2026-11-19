// hop-count tracer reports average and total number of hop-count of RDMAs
package tracing

import (
	"strconv"
	"strings"
)

type HopInfo struct {
	// startTime is the only field ever needed from the originating Task, so we
	// keep just the timestamp instead of the whole Task. The full Task pins its
	// Detail (the RDMA msg) for the tracer's entire lifetime, and HopTracer
	// never evicts, so storing it leaked one msg per RDMA request.
	startTime   float64
	hopTraced   uint64 // traced hops for the task
	hopRequired uint64 // needed hops for the task
	elapsedTime float64
	// lastStepTime is the absolute time of the most recent outbound switch
	// hop. Since response hops carry the response msg ID (a different task),
	// the request-trace task stops accumulating steps once it reaches the
	// serving/home chiplet, so lastStepTime marks that arrival. It splits the
	// summed round trip into a request side (start->lastStep) and response
	// side (lastStep->end) without any per-platform milestone.
	lastStepTime float64
	replied      bool
	srcChipId    int
	dstChipId    int
}

// C2CFlitKind tags the per-message flit-count task emitted by the network
// switch at the source-injection point. One task per message (head flit
// leaving a local endpoint), so req and rsp land on opposite directed
// edges of the C2C matrix with their own flit counts.
const C2CFlitKind = "c2c-flit"

// C2CFlitStep carries the original src->dst chiplet pair and the message's
// flit count for the C2C flit matrix.
type C2CFlitStep struct {
	SrcChip int
	DstChip int
	Flits   uint64
}

type HopTracer struct {
	filter        TaskFilter
	inflightTasks map[uint64]*HopInfo
	xDim          int
	yDim          int

	rdmaPerChiplet []uint64
	rdmaPerPC      map[string]uint64

	c2cFlitTraced [][]uint64 // [src][dst] = Σ flit count of messages injected
}

func NewHopTracer(
	filter TaskFilter,
	xDim int,
	yDim int,
) *HopTracer {
	numChiplets := xDim * yDim
	c2cFlitTraced := make([][]uint64, numChiplets)
	for i := range c2cFlitTraced {
		c2cFlitTraced[i] = make([]uint64, numChiplets)
	}

	t := &HopTracer{
		filter:         filter,
		xDim:           xDim,
		yDim:           yDim,
		inflightTasks:  make(map[uint64]*HopInfo),
		rdmaPerChiplet: make([]uint64, numChiplets),
		rdmaPerPC:      make(map[string]uint64),
		c2cFlitTraced:  c2cFlitTraced,
	}

	return t
}

func (t *HopTracer) StartTask(task Task) {
	if task.Kind == C2CFlitKind {
		t.recordC2CFlit(task)
		return
	}

	if !t.filter(task) {
		return
	}

	// if the task is already in the inflightTasks,
	// it means the task is retried.

	if task.What == "retry" {
		if hopInfo, ok := t.inflightTasks[task.ID]; ok {
			hopInfo.startTime = float64(task.StartTime)
			hopInfo.hopTraced = 0
			hopInfo.elapsedTime = 0
			hopInfo.lastStepTime = 0
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
	t.rdmaPerChiplet[srcChipId]++
	t.rdmaPerPC[task.What]++
}

func (t *HopTracer) StepTask(task Task) {

	hopInfo, ok := t.inflightTasks[task.ID]
	if !ok {
		return
	}

	hopInfo.hopTraced++
	if len(task.Steps) > 0 {
		hopInfo.lastStepTime = float64(task.Steps[0].Time)
	}
}

func (t *HopTracer) EndTask(task Task) {

	hopInfo, ok := t.inflightTasks[task.ID]
	if !ok {
		return
	}

	dstChipId, _ := strconv.Atoi(
		strings.TrimPrefix(
			strings.Split(task.Where, ".")[1],
			"chiplet_"))

	// elapsedTime tracks the full round trip, so always advance it to the last
	// reply. dstChipId is first-write-wins.
	hopInfo.elapsedTime = float64(task.EndTime) - hopInfo.startTime
	if !hopInfo.replied {
		hopInfo.dstChipId = dstChipId
	}
	hopInfo.replied = true
}

func (t *HopTracer) GetAvgLatencyPerHopTraced() []float64 {
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

// GetAvgReqSideLatencyPerHopTraced returns, per traced-hop bucket, the mean
// request-side latency: origin injection -> arrival at the serving/home
// chiplet (last outbound hop), summed across all legs. Binned by the same
// hopTraced key as GetAvgLatencyPerHopTraced, so req-side + rsp-side of a
// bucket reconstruct the full round trip.
func (t *HopTracer) GetAvgReqSideLatencyPerHopTraced() []float64 {
	num := make([]uint64, t.xDim*t.yDim)
	sum := make([]float64, t.xDim*t.yDim)

	for _, task := range t.inflightTasks {
		if !task.replied || task.hopTraced == 0 || task.lastStepTime == 0 {
			continue
		}
		req := task.lastStepTime - task.startTime
		if req < 0 {
			continue
		}
		num[task.hopTraced]++
		sum[task.hopTraced] += req
	}

	for h := range sum {
		if num[h] > 0 {
			sum[h] /= float64(num[h])
		}
	}
	return sum
}

// GetAvgRspSideLatencyPerHopTraced returns, per traced-hop bucket, the mean
// response-side latency: arrival at the serving/home chiplet (last outbound
// hop) -> data delivered back to the requestor, summed across all return
// legs. Includes the home L2/DRAM service that sits between the two sides.
func (t *HopTracer) GetAvgRspSideLatencyPerHopTraced() []float64 {
	num := make([]uint64, t.xDim*t.yDim)
	sum := make([]float64, t.xDim*t.yDim)

	for _, task := range t.inflightTasks {
		if !task.replied || task.hopTraced == 0 || task.lastStepTime == 0 {
			continue
		}
		end := task.startTime + task.elapsedTime
		rsp := end - task.lastStepTime
		if rsp < 0 {
			continue
		}
		num[task.hopTraced]++
		sum[task.hopTraced] += rsp
	}

	for h := range sum {
		if num[h] > 0 {
			sum[h] /= float64(num[h])
		}
	}
	return sum
}

func (t *HopTracer) GetNumPerHopTraced() []uint64 {
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

func (t *HopTracer) recordC2CFlit(task Task) {
	info, ok := task.Detail.(C2CFlitStep)
	if !ok {
		return
	}
	if info.SrcChip < 0 || info.SrcChip >= len(t.c2cFlitTraced) ||
		info.DstChip < 0 || info.DstChip >= len(t.c2cFlitTraced) {
		return
	}
	t.c2cFlitTraced[info.SrcChip][info.DstChip] += info.Flits
}

// GetC2CFlitTraced returns the [src][dst] matrix of total flit counts
// injected from src to dst chiplet (req and rsp on opposite edges).
func (t *HopTracer) GetC2CFlitTraced() [][]uint64 {
	return t.c2cFlitTraced
}

func (t *HopTracer) GetC2CTraced() [][]int {
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

func (t *HopTracer) GetNumReqsReceivedPerChiplet() []int {
	numChiplets := t.xDim * t.yDim
	received := make([]int, numChiplets)

	for _, task := range t.inflightTasks {
		if !task.replied || task.hopTraced == 0 {
			continue
		}
		received[task.dstChipId]++
	}

	return received
}

func (t *HopTracer) GetNumPerHopRequired() []uint64 {
	numPerHopRequired := make([]uint64, t.xDim+t.yDim-1)

	for _, task := range t.inflightTasks {
		numPerHopRequired[task.hopRequired]++
	}

	return numPerHopRequired
}

func (t *HopTracer) GetTotalRdmaReqs() int {
	return len(t.inflightTasks)
}

func (t *HopTracer) SetHopRequired(task Task) uint64 {
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

func (t *HopTracer) GetRdmaPerChiplet() []uint64 {
	return t.rdmaPerChiplet
}

func (t *HopTracer) GetRdmaPerPC() map[string]uint64 {
	return t.rdmaPerPC
}

func Abs(x int) int {
	if x < 0 {
		return -x
	}
	return x
}
