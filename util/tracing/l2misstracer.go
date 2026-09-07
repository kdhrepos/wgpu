package tracing

import (
	"strconv"
)

type L2MissTracer struct {
	taskFilter        TaskFilter
	numPerHopRequired []uint64
	numC2CAccesses    [][]uint64

	xDim int
	yDim int
}

func NewL2MissTracer(
	taskFilter TaskFilter,
	xDim int,
	yDim int,
) *L2MissTracer {
	numChiplets := xDim * yDim

	numC2CAccesses := make([][]uint64, numChiplets)
	for i := 0; i < numChiplets; i++ {
		numC2CAccesses[i] = make([]uint64, numChiplets)
	}

	t := &L2MissTracer{
		taskFilter:        taskFilter,
		xDim:              xDim,
		yDim:              yDim,
		numPerHopRequired: make([]uint64, xDim+yDim-1),
		numC2CAccesses:    numC2CAccesses,
	}

	return t
}

func (t *L2MissTracer) StartTask(task Task) {
	if !t.taskFilter(task) {
		return
	}

	hopRequired := t.SetHopRequired(task)
	t.numPerHopRequired[hopRequired]++
}

func (t *L2MissTracer) StepTask(task Task) {
	// Do nothing
}

func (t *L2MissTracer) EndTask(task Task) {
	// Do nothing
}

func (t *L2MissTracer) GetRdmaCount() float64 {
	rdmaCount := float64(0)

	for i := range t.numPerHopRequired {
		rdmaCount += float64(t.numPerHopRequired[i])
	}

	return rdmaCount
}

func (t *L2MissTracer) GetHopRequired() []uint64 {
	return t.numPerHopRequired
}

func (t *L2MissTracer) GetC2C() [][]uint64 {
	return t.numC2CAccesses
}

func (t *L2MissTracer) SetHopRequired(task Task) uint64 {
	srcChip, _ := strconv.Atoi(task.What)
	dstChip, _ := strconv.Atoi(task.Detail.(string))

	t.numC2CAccesses[srcChip][dstChip]++

	srcX := srcChip % t.xDim
	srcY := srcChip / t.xDim
	dstX := dstChip % t.xDim
	dstY := dstChip / t.xDim

	hopRequired := uint64(Abs(srcY-dstY) + Abs(srcX-dstX))

	return hopRequired
}
