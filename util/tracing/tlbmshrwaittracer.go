// TLBMSHRWaitTracer tracks two classes of MSHR-related stall in L1 and L2
// TLBs.
//
// "tlb-mshr-stall": fired (StartTask-only, no EndTask) when a translation
// miss cannot be placed in the MSHR because it is full. Increments stallCount.
//
// "tlb-mshr-hit-wait": fired as a StartTask/EndTask pair for every request
// that coalesces into an existing MSHR entry. StartTask is emitted in
// processTLBMSHRHit; EndTask is emitted in respondMSHREntry when the
// coalesced response is finally sent back to the requester. The difference
// measures how long the coalesced request waited beyond the entry already
// being in-flight.
package tracing

import "gitlab.com/akita/akita"

// TLBMSHRWaitTracer is attached to a single TLB component.
type TLBMSHRWaitTracer struct {
	stallCount       uint64
	hitWaitCount     uint64
	hitWaitTotalTime akita.VTimeInSec
	inflightTasks    map[uint64]Task
}

// NewTLBMSHRWaitTracer creates a TLBMSHRWaitTracer.
func NewTLBMSHRWaitTracer() *TLBMSHRWaitTracer {
	return &TLBMSHRWaitTracer{
		inflightTasks: make(map[uint64]Task),
	}
}

// StartTask handles the two event kinds described above.
func (t *TLBMSHRWaitTracer) StartTask(task Task) {
	switch task.Kind {
	case "tlb-mshr-stall":
		t.stallCount++
	case "tlb-mshr-hit-wait":
		t.inflightTasks[task.ID] = task
	}
}

// StepTask does nothing.
func (t *TLBMSHRWaitTracer) StepTask(task Task) {}

// EndTask finalises a "tlb-mshr-hit-wait" measurement.
func (t *TLBMSHRWaitTracer) EndTask(task Task) {
	original, ok := t.inflightTasks[task.ID]
	if !ok {
		return
	}
	t.hitWaitTotalTime += task.EndTime - original.StartTime
	t.hitWaitCount++
	delete(t.inflightTasks, task.ID)
}

// GetStallCount returns the number of times the MSHR was full and a miss
// could not be placed.
func (t *TLBMSHRWaitTracer) GetStallCount() uint64 {
	return t.stallCount
}

// GetHitWaitCount returns the number of coalesced requests that waited.
func (t *TLBMSHRWaitTracer) GetHitWaitCount() uint64 {
	return t.hitWaitCount
}

// GetHitWaitTotalTime returns the cumulative wait time for all coalesced
// requests.
func (t *TLBMSHRWaitTracer) GetHitWaitTotalTime() akita.VTimeInSec {
	return t.hitWaitTotalTime
}
