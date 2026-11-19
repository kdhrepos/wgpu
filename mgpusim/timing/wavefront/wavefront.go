package wavefront

import (
	"gitlab.com/akita/akita"
	"gitlab.com/akita/mgpusim/emu"
	"gitlab.com/akita/mgpusim/insts"
	"gitlab.com/akita/mgpusim/kernels"
	"gitlab.com/akita/util/ca"
)

// WfState marks what state that wavefront it in.
type WfState int

// A list of all possible WfState
const (
	WfDispatching WfState = iota // Dispatching in progress, not ready to run
	WfReady                      // Allow the scheduler to schedule instruction
	WfRunning                    // Instruction in fight
	WfCompleted                  // Wavefront completed
	WfAtBarrier                  // Wavefront at barrier
)

// A Wavefront in the timing package contains the information of the progress
// of a wavefront
type Wavefront struct {
	*kernels.Wavefront

	WG *WorkGroup

	pid            ca.PID
	State          WfState
	inst           *Inst            // The instruction that is being executed
	scratchpad     emu.Scratchpad   // A temp data buf that is shared by different stages
	LastFetchTime  akita.VTimeInSec // The time that the last instruction was fetched
	CompletedLanes int              // The number of lanes that is completed in the SIMD unit

	InstBuffer        []byte
	InstBufferStartPC uint64
	IsFetching        bool
	InstToIssue       *Inst

	SIMDID     int
	SRegOffset int
	VRegOffset int
	LDSOffset  int

	PC   uint64
	EXEC uint64
	VCC  uint64
	M0   uint32
	SCC  uint8

	OutstandingScalarMemAccess int
	OutstandingVectorMemAccess int

	// vmemIssueOrder holds vector-memory instructions that have been issued
	// but not yet retired, in issue order. vmemCompleted marks the ones whose
	// memory transactions have all returned. Together they enforce in-order
	// vmcnt retirement on top of out-of-order memory completion: the GCN3
	// s_waitcnt counter assumes loads retire in issue order, so a younger
	// instruction that finishes first must be held until all older ones retire.
	vmemIssueOrder []*Inst
	vmemCompleted  map[*Inst]bool
}

// VMemIssueOrder returns a snapshot of unretired vmem insts in issue order.
// Read-only — callers must not mutate. Used by the CU's stuck-inst recovery
// watchdog ([computeunit.go] reclaimStuckVMemInsts).
func (wf *Wavefront) VMemIssueOrder() []*Inst {
	return wf.vmemIssueOrder
}

// VMemCompleted reports whether the given inst has been marked completed via
// CompleteVMemInst (i.e., its last transaction returned and instFullyReturned
// was true at that point). Used by reclaimStuckVMemInsts.
func (wf *Wavefront) VMemCompleted(inst *Inst) bool {
	if wf.vmemCompleted == nil {
		return false
	}
	return wf.vmemCompleted[inst]
}

// EnqueueVMemInst records a vector-memory instruction at issue time so its
// vmcnt retirement happens in issue order even when the memory system
// completes the underlying transactions out of order.
func (wf *Wavefront) EnqueueVMemInst(inst *Inst) {
	wf.vmemIssueOrder = append(wf.vmemIssueOrder, inst)
}

// CompleteVMemInst marks inst as fully returned and retires every now-completed
// instruction from the head of the issue-order queue. It returns the retired
// instructions in issue order so the caller can update the vmcnt/lgkmcnt
// counters and emit completion traces. A younger instruction that finishes
// before an older one is held (not returned) until the older one retires,
// which is required by the GCN3 s_waitcnt in-order counter contract.
func (wf *Wavefront) CompleteVMemInst(inst *Inst) []*Inst {
	if wf.vmemCompleted == nil {
		wf.vmemCompleted = make(map[*Inst]bool)
	}
	wf.vmemCompleted[inst] = true

	var retired []*Inst
	for len(wf.vmemIssueOrder) > 0 {
		head := wf.vmemIssueOrder[0]
		if !wf.vmemCompleted[head] {
			break
		}
		retired = append(retired, head)
		delete(wf.vmemCompleted, head)
		wf.vmemIssueOrder = wf.vmemIssueOrder[1:]
	}
	return retired
}

// NewWavefront creates a new Wavefront of the timing package, wrapping the
// wavefront from the kernels package.
func NewWavefront(raw *kernels.Wavefront) *Wavefront {
	wf := new(Wavefront)
	wf.Wavefront = raw

	wf.scratchpad = make([]byte, 4096)
	wf.InstBuffer = make([]byte, 0, 256)

	return wf
}

// Inst return the instruction that is being simulated
func (wf *Wavefront) Inst() *insts.Inst {
	if wf.inst == nil {
		return nil
	}
	return wf.inst.Inst
}

// DynamicInst returns the insts with an ID
func (wf *Wavefront) DynamicInst() *Inst {
	return wf.inst
}

// SetDynamicInst sets the dynamic inst to execute
func (wf *Wavefront) SetDynamicInst(i *Inst) {
	wf.inst = i
}

// ManagedInst returns the wrapped Inst
func (wf *Wavefront) ManagedInst() *Inst {
	return wf.inst
}

// Scratchpad returns the scratchpad of the wavefront
func (wf *Wavefront) Scratchpad() emu.Scratchpad {
	return wf.scratchpad
}

// PID returns pid
func (wf *Wavefront) PID() ca.PID {
	return 1
	// return wf.pid
}

// SetPID sets pid
func (wf *Wavefront) SetPID(pid ca.PID) {
	wf.pid = 1
	// wf.pid = pid
}
