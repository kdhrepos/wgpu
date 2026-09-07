package cu

import (
	"fmt"
	"log"

	"gitlab.com/akita/akita"
	"gitlab.com/akita/mgpusim/insts"
	"gitlab.com/akita/mgpusim/timing/wavefront"
	"gitlab.com/akita/protocol"
	"gitlab.com/akita/util"
	"gitlab.com/akita/util/pipelining"
	"gitlab.com/akita/util/tracing"
)

var vmemDbgCount int
var vmemDbgLimit = 1 << 30

// VA sanity bounds used by oooDebug traps. The lower bound rejects
// near-NULL pointers obviously derived from corrupt small integers; the
// upper bound rejects values much larger than any realistic GPU VA span
// (1 TB). Anything outside this window at issue time is treated as proof
// of upstream corruption and panics with full context.
const (
	oooVaSanityMin uint64 = 0x1000
	oooVaSanityMax uint64 = 0x10_000_000_000
)

// checkIssueAddrSanity panics if addr is clearly out of any plausible
// GPU VA range. Called at FLAT load/store issue when oooDebug is on, so
// corruption is trapped at the SOURCE instead of bubbling into MMU.
func checkIssueAddrSanity(wf *wavefront.Wavefront, op string, addr uint64, txIdx, txTotal int) {
	if addr >= oooVaSanityMin && addr < oooVaSanityMax {
		return
	}
	panic(fmt.Sprintf(
		"[ooo-bug] %s ISSUE out-of-range VA: wf=%s PC=0x%x tx=%d/%d addr=0x%x (bounds [0x%x,0x%x))",
		op, wf.UID, wf.PC, txIdx+1, txTotal, addr, oooVaSanityMin, oooVaSanityMax))
}

// checkStoreDataSanity scans the dirty 8-byte slots of a write and panics
// if any slot looks like a pointer outside the sanity window. Stored
// pointers that point above oooVaSanityMax are the smoking gun that
// produced the later MMU NULL PPN crash (the store corrupted comp[] with
// a garbage VA that the next load dereferenced).
func checkStoreDataSanity(wf *wavefront.Wavefront, addr uint64, data []byte, mask []bool) {
	n := len(data) &^ 7 // round down to multiple of 8
	if len(mask) < n {
		n = len(mask)
	}
	for off := 0; off+8 <= n; off += 8 {
		allDirty := true
		for k := 0; k < 8; k++ {
			if !mask[off+k] {
				allDirty = false
				break
			}
		}
		if !allDirty {
			continue
		}
		v := uint64(data[off]) | uint64(data[off+1])<<8 |
			uint64(data[off+2])<<16 | uint64(data[off+3])<<24 |
			uint64(data[off+4])<<32 | uint64(data[off+5])<<40 |
			uint64(data[off+6])<<48 | uint64(data[off+7])<<56
		// Treat 0 / small ints as legitimate. Only flag values that
		// look like pointers but exceed the sanity upper bound.
		if v >= oooVaSanityMax {
			panic(fmt.Sprintf(
				"[ooo-bug] FlatStore writes pointer-like garbage: wf=%s PC=0x%x storeAddr=0x%x slotOff=0x%x val=0x%x",
				wf.UID, wf.PC, addr, off, v))
		}
	}
}

type vectorMemInst struct {
	wavefront *wavefront.Wavefront
}

func (i vectorMemInst) TaskID() uint64 {
	return i.wavefront.DynamicInst().ID
}

// A VectorMemoryUnit is the block in a compute unit that can performs vector
// memory operations.
type VectorMemoryUnit struct {
	cu *ComputeUnit

	scratchpadPreparer ScratchpadPreparer
	coalescer          coalescer

	numInstInFlight         uint64
	numTransactionInFlight  uint64
	maxInstructionsInFlight uint64

	instructionPipeline           pipelining.Pipeline
	postInstructionPipelineBuffer util.Buffer
	transactionsWaiting           []VectorMemAccessInfo
	transactionPipeline           pipelining.Pipeline
	postTransactionPipelineBuffer util.Buffer

	// egressWidth: transactions/cycle drained from the vmem unit toward the
	// ROB (insert + send). Default 1; raised by -vmem-egress-width.
	egressWidth int

	isIdle bool

	withPC bool
}

// NewVectorMemoryUnit creates a new Vector Memory Unit.
func NewVectorMemoryUnit(
	cu *ComputeUnit,
	scratchpadPreparer ScratchpadPreparer,
	coalescer coalescer,
	withPC bool,
) *VectorMemoryUnit {
	u := new(VectorMemoryUnit)
	u.cu = cu

	u.scratchpadPreparer = scratchpadPreparer
	u.coalescer = coalescer

	u.withPC = withPC

	return u
}

// CanAcceptWave checks if the buffer of the read stage is occupied or not
func (u *VectorMemoryUnit) CanAcceptWave() bool {
	return u.instructionPipeline.CanAccept()
}

// AcceptWave moves one wavefront into the read buffer of the Scalar unit
func (u *VectorMemoryUnit) AcceptWave(
	wave *wavefront.Wavefront,
	now akita.VTimeInSec,
) {
	u.instructionPipeline.Accept(now, vectorMemInst{wavefront: wave})
	u.numInstInFlight++
}

// IsIdle moves one wavefront into the read buffer of the Scalar unit
func (u *VectorMemoryUnit) IsIdle() bool {
	u.isIdle = (u.numInstInFlight == 0) && (u.numTransactionInFlight == 0)
	return u.isIdle
}

// Run executes three pipeline stages that are controlled by the
// VectorMemoryUnit
func (u *VectorMemoryUnit) Run(now akita.VTimeInSec) bool {
	madeProgress := false
	w := u.egressWidth
	if w < 1 {
		w = 1
	}
	for i := 0; i < w; i++ {
		madeProgress = u.sendRequest(now) || madeProgress
	}
	madeProgress = u.transactionPipeline.Tick(now) || madeProgress
	madeProgress = u.instToTransaction(now) || madeProgress
	madeProgress = u.instructionPipeline.Tick(now) || madeProgress
	return madeProgress
}

func (u *VectorMemoryUnit) instToTransaction(
	now akita.VTimeInSec,
) bool {
	if len(u.transactionsWaiting) > 0 {
		w := u.egressWidth
		if w < 1 {
			w = 1
		}
		madeProgress := false
		for i := 0; i < w; i++ {
			if len(u.transactionsWaiting) == 0 {
				break
			}
			if !u.insertTransactionToPipeline(now) {
				break
			}
			madeProgress = true
		}
		return madeProgress
	}

	return u.execute(now)
}

func (u *VectorMemoryUnit) insertTransactionToPipeline(
	now akita.VTimeInSec,
) bool {
	if !u.transactionPipeline.CanAccept() {
		return false
	}

	u.transactionPipeline.Accept(now, u.transactionsWaiting[0])
	u.transactionsWaiting = u.transactionsWaiting[1:]

	return true
}

func (u *VectorMemoryUnit) execute(now akita.VTimeInSec) (madeProgress bool) {
	// Peek (not Pop): if executeFlatInsts cannot complete this cycle (InFlight
	// limit reached) the inst must stay in the buffer for retry. Popping first
	// and bailing on failure silently dropped the inst — leaving numInstInFlight
	// stuck >0 (VMU forever busy) and the wavefront pinned in WfRunning, the
	// root cause of the all-CUs-stuck deadlock.
	item := u.postInstructionPipelineBuffer.Peek()
	if item == nil {
		return false
	}

	wave := item.(vectorMemInst).wavefront
	inst := wave.Inst()
	switch inst.FormatType {
	case insts.FLAT:
		ok := u.executeFlatInsts(now, wave)
		if !ok {
			return false
		}
	default:
		log.Panicf("running inst %s in vector memory unit is not supported", inst.String(nil))
	}

	u.postInstructionPipelineBuffer.Pop()
	u.cu.UpdatePCAndSetReady(wave)
	u.numInstInFlight--

	return true
}

func (u *VectorMemoryUnit) executeFlatInsts(
	now akita.VTimeInSec,
	wavefront *wavefront.Wavefront,
) bool {
	inst := wavefront.DynamicInst()
	switch inst.Opcode {
	case 16, 17, 18, 19, 20, 21, 22, 23: // FLAT_LOAD_BYTE
		return u.executeFlatLoad(now, wavefront)
	case 24, 25, 26, 27, 28, 29, 30, 31:
		return u.executeFlatStore(now, wavefront)
	default:
		log.Panicf("Opcode %d for format FLAT is not supported.", inst.Opcode)
	}

	panic("never")
}

func (u *VectorMemoryUnit) executeFlatLoad(
	now akita.VTimeInSec,
	wave *wavefront.Wavefront,
) bool {
	u.scratchpadPreparer.Prepare(wave, wave)
	transactions := u.coalescer.generateMemTransactions(wave)

	if len(transactions) == 0 {
		u.cu.logInstTask(
			now,
			wave,
			wave.DynamicInst(),
			true,
		)
		// No memory work for this inst (all lanes masked off / empty). Treat as
		// complete so execute() advances PC and decrements numInstInFlight.
		// Returning false here would leave the inst stuck (see execute()).
		return true
	}

	if len(transactions)+len(u.cu.InFlightVectorMemAccess) >
		u.cu.InFlightVectorMemAccessLimit {
		return false
	}

	ovmaBefore := wave.OutstandingVectorMemAccess
	wave.OutstandingVectorMemAccess++
	wave.OutstandingScalarMemAccess++
	// Record issue order so the counters retire in order even though the
	// memory system may complete these transactions out of order.
	wave.EnqueueVMemInst(wave.DynamicInst())

	sp := wave.Scratchpad().AsFlat()
	dbg := oooDebugOn()
	for i, t := range transactions {
		if dbg {
			addr := t.Read.Address
			// OVMA shown before this instruction's increment (ovmaBefore = prior in-flight count)
			fmt.Printf("[ooo-dbg] FlatLoad ISSUE: wf=%s PC=0x%x OVMA_before=%d tx=%d/%d addr=0x%x\n",
				wave.UID, wave.PC, ovmaBefore, i+1, len(transactions), addr)
			// Print lane addresses only for first transaction to keep output manageable
			if i == 0 {
				for lane := 0; lane < 64; lane++ {
					if sp.ADDR[lane] != 0 {
						fmt.Printf("[ooo-dbg]   lane%d=0x%x\n", lane, sp.ADDR[lane])
					}
				}
			}
		}
		if dbg {
			if t.Read.Address < oooVaSanityMin || t.Read.Address >= oooVaSanityMax {
				// Dump where the address VGPRs came from before trapping, so
				// the root cause (stale/NULL register vs in-flight load) is
				// captured at the source.
				u.cu.dumpAddrVGPRProvenance(now, wave)
			}
			// checkIssueAddrSanity(wave, "FlatLoad", t.Read.Address, i, len(transactions))
		}
		t.IssueTime = now
		u.cu.InFlightVectorMemAccess = append(u.cu.InFlightVectorMemAccess, t)
		if i != len(transactions)-1 {
			t.Read.CanWaitForCoalesce = true
		}
		lowModule := u.cu.VectorMemModules.Find(t.Read.Address)
		t.Read.Dst = lowModule
		t.Read.Src = u.cu.ToVectorMem
		t.Read.PID = wave.PID()
		u.transactionsWaiting = append(u.transactionsWaiting, t)
		u.numTransactionInFlight++ // balances the decrement at sendRequest
	}

	return true
}

func (u *VectorMemoryUnit) executeFlatStore(
	now akita.VTimeInSec,
	wave *wavefront.Wavefront,
) bool {
	u.scratchpadPreparer.Prepare(wave, wave)
	transactions := u.coalescer.generateMemTransactions(wave)

	if len(transactions) == 0 {
		u.cu.logInstTask(
			now,
			wave,
			wave.DynamicInst(),
			true,
		)
		// No memory work for this inst — treat as complete (see executeFlatLoad).
		return true
	}

	if len(transactions)+len(u.cu.InFlightVectorMemAccess) >
		u.cu.InFlightVectorMemAccessLimit {
		return false
	}

	ovmaBefore := wave.OutstandingVectorMemAccess
	wave.OutstandingVectorMemAccess++
	wave.OutstandingScalarMemAccess++
	// Record issue order so the counters retire in order even though the
	// memory system may complete these transactions out of order.
	wave.EnqueueVMemInst(wave.DynamicInst())

	sp := wave.Scratchpad().AsFlat()
	dbg := oooDebugOn()
	for i, t := range transactions {
		if dbg {
			addr := t.Write.Address
			fmt.Printf("[ooo-dbg] FlatStore ISSUE: wf=%s PC=0x%x OVMA_before=%d tx=%d/%d addr=0x%x\n",
				wave.UID, wave.PC, ovmaBefore, i+1, len(transactions), addr)
			if i == 0 {
				for lane := 0; lane < 64; lane++ {
					if sp.ADDR[lane] != 0 {
						fmt.Printf("[ooo-dbg]   lane%d addr=0x%x\n", lane, sp.ADDR[lane])
					}
				}
			}
			// Preview the first 32 bytes of the write payload so we can
			// see what pointer/value is being committed to memory.
			dumpLen := len(t.Write.Data)
			if dumpLen > 32 {
				dumpLen = 32
			}
			fmt.Printf("[ooo-dbg] FlatStore DATA : wf=%s PC=0x%x addr=0x%x len=%d data[0..%d]=%x\n",
				wave.UID, wave.PC, addr, len(t.Write.Data), dumpLen, t.Write.Data[:dumpLen])
			// checkIssueAddrSanity(wave, "FlatStore", addr, i, len(transactions))
			// checkStoreDataSanity(wave, addr, t.Write.Data, t.Write.DirtyMask)
		}
		t.IssueTime = now
		u.cu.InFlightVectorMemAccess = append(u.cu.InFlightVectorMemAccess, t)
		if i != len(transactions)-1 {
			t.Write.CanWaitForCoalesce = true
		}
		lowModule := u.cu.VectorMemModules.Find(t.Write.Address)
		t.Write.Dst = lowModule
		t.Write.Src = u.cu.ToVectorMem
		t.Write.PID = wave.PID()
		u.transactionsWaiting = append(u.transactionsWaiting, t)
		u.numTransactionInFlight++ // balances the decrement at sendRequest
	}

	return true
}

func (u *VectorMemoryUnit) sendRequest(now akita.VTimeInSec) bool {
	item := u.postTransactionPipelineBuffer.Peek()
	if item == nil {
		return false
	}

	var req akita.Msg
	info := item.(VectorMemAccessInfo)
	if info.Read != nil {
		req = info.Read
		// Tag the issuing wavefront so the ROB can classify head-of-line
		// blocking as cross- vs intra-wavefront (measurement).
		if info.Wavefront != nil {
			info.Read.WfID = info.Wavefront.UID
		}
		if u.withPC {
			req.(*protocol.ReadReq).Info = info.PC
			// pc := info.Wavefront.PC
			// req.(*protocol.ReadReq).Info = pc
		}
	} else {
		req = info.Write
		if info.Wavefront != nil {
			info.Write.WfID = info.Wavefront.UID
		}
		if u.withPC {
			req.(*protocol.WriteReq).Info = info.PC
			// pc := info.Wavefront.PC
			// req.(*protocol.WriteReq).Info = pc
		}
	}

	req.Meta().SendTime = now
	err := u.cu.ToVectorMem.Send(req)
	if err == nil {
		u.postTransactionPipelineBuffer.Pop()
		u.numTransactionInFlight--

		u.cu.noteVMemDispatch(req.Meta().ID, now)

		tracing.TraceReqInitiate(req, now, u.cu, info.Inst.ID)

		// if info.Read != nil {
		// 	fmt.Println("cta-page-map ", info.Wavefront.WG.IDX, info.Wavefront.WG.IDY, req.(*mem.ReadReq).Address)
		// } else {
		// 	fmt.Println("cta-page-map ", info.Wavefront.WG.IDX, info.Wavefront.WG.IDY, req.(*mem.WriteReq).Address)
		// }

		return true
	}

	return false
}

// Flush flushes
func (u *VectorMemoryUnit) Flush() {
	u.instructionPipeline.Clear()
	u.transactionPipeline.Clear()
	u.postInstructionPipelineBuffer.Clear()
	u.postTransactionPipelineBuffer.Clear()
	u.transactionsWaiting = nil
	u.numInstInFlight = 0
	u.numTransactionInFlight = 0
}
