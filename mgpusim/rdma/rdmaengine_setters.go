// Package rdma provides the implementation of an RDMA engine.
package rdma

import (
	"gitlab.com/akita/akita"
	"gitlab.com/akita/lmf"
)

func (r *RDMAEngineImpl) SetPwPort(PwPort akita.Port) {
	r.PwPort = PwPort
}

func (r *RDMAEngineImpl) SetToL2RDMA(ToL2RDMA akita.Port) {
	r.ToL2RDMA = ToL2RDMA
}

func (r *RDMAEngineImpl) SetToL2Bottom(ToL2Bottom akita.Port) {
	r.ToL2Bottom = ToL2Bottom
}

func (r *RDMAEngineImpl) SetRequestPort(RequestPort akita.Port) {
	r.RequestPort = RequestPort
}

func (r *RDMAEngineImpl) SetResponsePort(ResponsePort akita.Port) {
	r.ResponsePort = ResponsePort
}

func (r *RDMAEngineImpl) SetCtrlPort(CtrlPort akita.Port) {
	r.CtrlPort = CtrlPort
}

func (r *RDMAEngineImpl) SetIsDraining(isDraining bool) {
	r.isDraining = isDraining
}

func (r *RDMAEngineImpl) SetPauseIncomingReqsFromL1(pauseIncomingReqsFromL1 bool) {
	r.pauseIncomingReqsFromL1 = pauseIncomingReqsFromL1
}

func (r *RDMAEngineImpl) SetCurrentDrainReq(currentDrainReq *DrainReq) {
	r.currentDrainReq = currentDrainReq
}

func (r *RDMAEngineImpl) SetLocalModuleFinder(localModuleFinder lmf.LowModuleFinder) {
	r.localModuleFinder = localModuleFinder
}

func (r *RDMAEngineImpl) SetTranslationModuleFinder(translationModuleFinder lmf.LowModuleFinder) {
	r.translationModuleFinder = translationModuleFinder
}

func (r *RDMAEngineImpl) SetRdmaReqAddressTable(RdmaReqAddressTable lmf.LowModuleFinder) {
	r.RdmaReqAddressTable = RdmaReqAddressTable
}

func (r *RDMAEngineImpl) SetRdmaRspAddressTable(RdmaRspAddressTable map[akita.Port]akita.Port) {
	r.RdmaRspAddressTable = RdmaRspAddressTable
}

func (r *RDMAEngineImpl) SetTransactionsFromOutside(transactionsFromOutside []transaction) {
	r.transactionsFromOutside = transactionsFromOutside
}

func (r *RDMAEngineImpl) SetTransactionsFromInside(transactionsFromInside []transaction) {
	r.transactionsFromInside = transactionsFromInside
}

func (r *RDMAEngineImpl) SetNumTransPerCycle(numTransPerCycle int) {
	r.numTransPerCycle = numTransPerCycle
}

func (e *RDMAEngineImpl) SetChipletId(id int) {
	e.ChipletId = id
}

func (e *RDMAEngineImpl) SetY(y int) {
	e.y = y
}

func (e *RDMAEngineImpl) SetX(x int) {
	e.x = x
}

func (e *RDMAEngineImpl) SetYDim(yDim int) {
	e.yDim = yDim
}

func (e *RDMAEngineImpl) SetXDim(xDim int) {
	e.xDim = xDim
}
