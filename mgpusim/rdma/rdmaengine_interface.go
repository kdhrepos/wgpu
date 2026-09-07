package rdma

import (
	"gitlab.com/akita/akita"
	"gitlab.com/akita/lmf"
	"gitlab.com/akita/util/tracing"
)

var maxOutstandingReqs int = 1 << 31

type RDMAEngine interface {
	akita.Component
	tracing.NamedHookable

	GetPwPort() akita.Port
	GetToL2RDMA() akita.Port
	GetToL2Bottom() akita.Port
	GetToDramTop() akita.Port
	GetRequestPort() akita.Port
	GetResponsePort() akita.Port
	GetCtrlPort() akita.Port
	GetIsDraining() bool
	GetPauseIncomingReqsFromL1() bool
	GetCurrentDrainReq() *DrainReq
	GetLocalModuleFinder() lmf.LowModuleFinder
	GetTranslationModuleFinder() lmf.LowModuleFinder
	GetRdmaReqAddressTable() lmf.LowModuleFinder
	GetRdmaRspAddressTable() map[akita.Port]akita.Port
	GetTransactionsFromOutside() []transaction
	GetTransactionsFromInside() []transaction
	GetNumTransPerCycle() int
	GetChipletId() int
	GetY() int
	GetX() int
	GetYDim() int
	GetXDim() int

	SetPwPort(PwPort akita.Port)
	SetToL2RDMA(ToL2RDMA akita.Port)
	SetToL2Bottom(ToL2Bottom akita.Port)
	SetRequestPort(RequestPort akita.Port)
	SetResponsePort(ResponsePort akita.Port)
	SetCtrlPort(CtrlPort akita.Port)
	SetIsDraining(isDraining bool)
	SetPauseIncomingReqsFromL1(pauseIncomingReqsFromL1 bool)
	SetCurrentDrainReq(currentDrainReq *DrainReq)
	SetTranslationModuleFinder(tmf lmf.LowModuleFinder)
	SetLocalModuleFinder(lmf lmf.LowModuleFinder)
	SetRdmaReqAddressTable(RdmaReqAddressTable lmf.LowModuleFinder)
	SetRdmaRspAddressTable(RdmaRspAddressTable map[akita.Port]akita.Port)
	SetTransactionsFromOutside(transFromOutside []transaction)
	SetTransactionsFromInside(transFromInside []transaction)
	SetNumTransPerCycle(numTransPerCycle int)
	SetChipletId(chipletId int)
	SetY(y int)
	SetX(x int)
	SetYDim(yDim int)
	SetXDim(xDim int)

	PushToRetryPort(msg akita.Msg)
}
