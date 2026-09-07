package caches

import (
	"gitlab.com/akita/akita"
	"gitlab.com/akita/lmf"
	"gitlab.com/akita/util/tracing"
)

type L1Cache interface {
	akita.Component
	tracing.NamedHookable

	Tick(now akita.VTimeInSec) bool

	GetTopPort() akita.Port
	GetBottomToL2Port() akita.Port
	GetBottomToRdmaPort() akita.Port
	GetBottomPort() akita.Port
	GetControlPort() akita.Port

	SetLowModuleFinder(lowModuleFinder lmf.LowModuleFinder)
	GetLowModuleFinder() lmf.LowModuleFinder
}

type L1CacheBuilder interface {
	WithEngine(engine akita.Engine) L1CacheBuilder
	WithFreq(freq akita.Freq) L1CacheBuilder
	WithWayAssocitivity(wayAssocitivity int) L1CacheBuilder
	WithNumMSHREntry(numMSHREntry int) L1CacheBuilder
	WithNumBanks(numBank int) L1CacheBuilder
	WithBankLatency(bankLatency int) L1CacheBuilder
	WithNumReqsPerCycle(numReqPerCycle int) L1CacheBuilder
	WithLog2BlockSize(log2BlockSize uint64) L1CacheBuilder
	WithTotalByteSize(totalByteSize uint64) L1CacheBuilder
	WithLowModuleFinder(lowModuleFinder lmf.LowModuleFinder) L1CacheBuilder
	WithChipletId(chipletId uint64) L1CacheBuilder

	Build(name string) L1Cache
}
