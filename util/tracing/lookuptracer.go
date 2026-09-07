// LookupTracer reports every access count per memory address
package tracing

import "gitlab.com/akita/protocol"

type LookupTracer struct {
	taskFilter            TaskFilter
	lookupCount           uint64
	lookupCountPerChiplet []uint64
	hitCount              uint64
	mshrHitCount          uint64
	distHitCount          uint64
	missCount             uint64

	lookupPteHitCount     uint64
	lookupPteMissCount    uint64
	lookupPteMshrHitCount uint64
	lookupPteDistCount    uint64
}

func NewLookupTracer(
	xDim int,
	yDim int,
) *LookupTracer {
	t := &LookupTracer{}
	t.lookupCountPerChiplet = make([]uint64, xDim*yDim)
	return t
}

func (t *LookupTracer) StartTask(task Task) {
	switch task.Kind {
	case "lookup":
		t.lookupCount++

		chipletId, ok := task.Detail.(int)
		if ok {
			t.lookupCountPerChiplet[chipletId]++
		}
	case "lookup-hit":
		t.hitCount++

		readReq, ok := task.Detail.(*protocol.ReadReq)
		if !ok {
			return
		}

		if readReq.IsPtw {
			t.lookupPteHitCount++
		}
	case "lookup-miss":
		t.missCount++

		readReq, ok := task.Detail.(*protocol.ReadReq)
		if !ok {
			return
		}

		if readReq.IsPtw {
			t.lookupPteMissCount++
		}
	case "lookup-mshr-hit":
		t.mshrHitCount++

		readReq, ok := task.Detail.(*protocol.ReadReq)
		if !ok {
			return
		}

		if readReq.IsPtw {
			t.lookupPteMshrHitCount++
		}
	case "lookup-distribution-hit":
		t.distHitCount++
		readReq, ok := task.Detail.(*protocol.ReadReq)
		if !ok {
			return
		}

		if readReq.IsPtw {
			t.lookupPteDistCount++
		}
	}
}

func (t *LookupTracer) StepTask(task Task) {
	// Do nothing
}

func (t *LookupTracer) EndTask(task Task) {
	// Do nothing
}

func (t *LookupTracer) GetLookupCount() uint64 {
	return t.lookupCount
}

func (t *LookupTracer) GetHitCount() uint64 {
	return t.hitCount
}

func (t *LookupTracer) GetMSHRHitCount() uint64 {
	return t.mshrHitCount
}

func (t *LookupTracer) GetMissCount() uint64 {
	return t.missCount
}

func (t *LookupTracer) GetLookupPteHitCount() uint64 {
	return t.lookupPteHitCount
}

func (t *LookupTracer) GetLookupPteMissCount() uint64 {
	return t.lookupPteMissCount
}

func (t *LookupTracer) GetLookupPteMshrHitCount() uint64 {
	return t.lookupPteMshrHitCount
}

func (t *LookupTracer) GetLookupPteDistCount() uint64 {
	return t.lookupPteDistCount
}

func (t *LookupTracer) GetDistributionHitCount() uint64 {
	return t.distHitCount
}

func (t *LookupTracer) GetLookupCountPerChiplet() []uint64 {
	return t.lookupCountPerChiplet
}
