package cache

import (
	"gitlab.com/akita/akita"
	"gitlab.com/akita/lmf"
	"gitlab.com/akita/util/tracing"
)

// A Cache is a generic cache that can be configured to different cache policies.
type Cache interface {
	akita.Component
	tracing.NamedHookable

	Tick(now akita.VTimeInSec) bool

	GetTopPort() akita.Port
	GetBottomToDramPort() akita.Port
	GetBottomToRdmaPort() akita.Port
	// GetBottomPort() akita.Port
	GetRDMAPort() akita.Port
	GetControlPort() akita.Port

	SetLowModuleFinder(lmf lmf.LowModuleFinder)

	GetNumReqPerCycle() int
}
