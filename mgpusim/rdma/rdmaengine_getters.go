// Package rdma provides the implementation of an RDMA engine.
package rdma

import (
	"gitlab.com/akita/akita"
	"gitlab.com/akita/lmf"
)

// ======================================================================================
// Getter Implementations
// ======================================================================================

func (r *RDMAEngineImpl) GetPwPort() akita.Port {
	return r.PwPort
}

func (r *RDMAEngineImpl) GetToL2RDMA() akita.Port {
	return r.ToL2RDMA
}

// GetToL2RDMAs returns all per-bank outbound ports to L2. For platforms that
// do not use the per-bank wiring this slice has a single element identical
// to ToL2RDMA.
func (r *RDMAEngineImpl) GetToL2RDMAs() []akita.Port {
	return r.ToL2RDMAs
}

func (r *RDMAEngineImpl) GetToL2Bottom() akita.Port {
	return r.ToL2Bottom
}

func (r *RDMAEngineImpl) GetToDramTop() akita.Port {
	return r.ToDramTop
}

func (r *RDMAEngineImpl) GetRequestPort() akita.Port {
	return r.RequestPort
}

func (r *RDMAEngineImpl) GetResponsePort() akita.Port {
	return r.ResponsePort
}

func (r *RDMAEngineImpl) GetCtrlPort() akita.Port {
	return r.CtrlPort
}

func (r *RDMAEngineImpl) GetIsDraining() bool {
	return r.isDraining
}

func (r *RDMAEngineImpl) GetPauseIncomingReqsFromL1() bool {
	return r.pauseIncomingReqsFromL1
}

func (r *RDMAEngineImpl) GetCurrentDrainReq() *DrainReq {
	return r.currentDrainReq
}

func (r *RDMAEngineImpl) GetLocalModuleFinder() lmf.LowModuleFinder {
	return r.localModuleFinder
}

func (r *RDMAEngineImpl) GetTranslationModuleFinder() lmf.LowModuleFinder {
	return r.translationModuleFinder
}

func (r *RDMAEngineImpl) GetRdmaReqAddressTable() lmf.LowModuleFinder {
	return r.RdmaReqAddressTable
}

func (r *RDMAEngineImpl) GetRdmaRspAddressTable() map[akita.Port]akita.Port {
	return r.RdmaRspAddressTable
}

func (r *RDMAEngineImpl) GetTransactionsFromOutside() []transaction {
	return r.transactionsFromOutside
}

func (r *RDMAEngineImpl) GetTransactionsFromInside() []transaction {
	return r.transactionsFromInside
}

func (r *RDMAEngineImpl) GetNumTransPerCycle() int {
	return r.numTransPerCycle
}

func (e *RDMAEngineImpl) GetChipletId() int {
	return e.ChipletId
}

func (e *RDMAEngineImpl) GetY() int {
	return e.y
}

func (e *RDMAEngineImpl) GetX() int {
	return e.x
}

func (e *RDMAEngineImpl) GetYDim() int {
	return e.yDim
}

func (e *RDMAEngineImpl) GetXDim() int {
	return e.xDim
}
