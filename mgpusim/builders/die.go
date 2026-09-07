package builders

import (
	"gitlab.com/akita/akita"
	"gitlab.com/akita/lmf"
	"gitlab.com/akita/mem/cache"
	"gitlab.com/akita/mem/idealmemcontroller"
	"gitlab.com/akita/mem/vm/addresstranslator"
	"gitlab.com/akita/mem/vm/mmu"
	"gitlab.com/akita/mem/vm/tlb"
	"gitlab.com/akita/mgpusim/rdma"
	"gitlab.com/akita/mgpusim/timing/caches"
	"gitlab.com/akita/mgpusim/timing/caches/rob"
	"gitlab.com/akita/mgpusim/timing/cu"
)

type Chiplet struct {
	Dispatchers []akita.Component
	CUs         []*cu.ComputeUnit
	L1VCaches   []caches.L1Cache
	L1ICaches   []caches.L1Cache
	L1SCaches   []caches.L1Cache
	L2Caches    []cache.Cache
	// L2Caches []*writeback.Cache
	// L2Caches          []*writethrough.Cache
	L2CacheFinder     lmf.LowModuleFinder
	L2TLBs            []tlb.L2TLB
	L1VTLBs           []*tlb.TLB
	L1STLBs           []*tlb.TLB
	L1ITLBs           []*tlb.TLB
	L1VROBs           []*rob.ReorderBuffer
	L1IROBs           []*rob.ReorderBuffer
	L1SROBs           []*rob.ReorderBuffer
	ReorderBuffers    []*rob.ReorderBuffer
	L1VAddrTranslator []addresstranslator.AddressTranslator
	L1IAddrTranslator []addresstranslator.AddressTranslator
	L1SAddrTranslator []addresstranslator.AddressTranslator
	DRAMs             []*idealmemcontroller.Comp
	// DRAMs []*dram.MemController
	MMU *mmu.MMUImpl

	chipRdmaEngine       rdma.RDMAEngine
	pageRdmaEngine       rdma.RDMAEngine
	lowModuleFinderForL1 lmf.LowModuleFinder // this returns ports for L2 cache

	InternalConnection     akita.Connection
	L1TLBToL2TLBConnection *akita.DirectConnection
	TLBToMMUConnection     *akita.DirectConnection
	L1ToL2Connection       *akita.DirectConnection
	L2ToDramConnection     *akita.DirectConnection
	L1ToRdmaConnection     *akita.DirectConnection
	L2ToRdmaConnection     *akita.DirectConnection

	name      string
	ChipletID uint64
}

// NewChiplet return a new Chiplet instance with only name and ID set.
func NewChiplet(name string, id uint64) *Chiplet {
	chiplet := new(Chiplet)
	chiplet.name = name
	chiplet.ChipletID = id

	return chiplet
}
