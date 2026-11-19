package mgpusim

import (
	"gitlab.com/akita/akita"
	"gitlab.com/akita/lmf"
	"gitlab.com/akita/mem"
	"gitlab.com/akita/mem/cache"
	"gitlab.com/akita/mem/idealmemcontroller"

	// "gitlab.com/akita/mem/dram"

	"gitlab.com/akita/mem/vm/addresstranslator"
	"gitlab.com/akita/mem/vm/mmu"
	"gitlab.com/akita/mem/vm/tlb"
	"gitlab.com/akita/mgpusim/pagemigrationcontroller"
	"gitlab.com/akita/mgpusim/rdma"
	"gitlab.com/akita/mgpusim/timing/caches"
	"gitlab.com/akita/mgpusim/timing/caches/rob"
	"gitlab.com/akita/mgpusim/timing/cp"
	"gitlab.com/akita/noc/networking/chipnetwork"
	"gitlab.com/akita/noc/networking/switching"
)

// A GPU is a wrapper that holds all the subcomponent of a GPU.
type GPU struct {
	CommandProcessor   *cp.CommandProcessor
	RDMAEngine         rdma.RDMAEngine
	PMC                *pagemigrationcontroller.PageMigrationController
	Dispatchers        []akita.Component
	CUs                []akita.Component
	L1VCaches          []caches.L1Cache
	L1ICaches          []caches.L1Cache
	L1SCaches          []caches.L1Cache
	L2Caches           []cache.Cache
	L2CacheFinder      lmf.LowModuleFinder
	L2TLBs             []tlb.L2TLB
	L1VTLBs            []*tlb.TLB
	L1STLBs            []*tlb.TLB
	L1ITLBs            []*tlb.TLB
	L1VROBs            []*rob.ReorderBuffer
	L1IROBs            []*rob.ReorderBuffer
	L1SROBs            []*rob.ReorderBuffer
	ReorderBuffers     []*rob.ReorderBuffer
	L1VAddrTranslator  []addresstranslator.AddressTranslator
	L1IAddrTranslator  []addresstranslator.AddressTranslator
	L1SAddrTranslator  []addresstranslator.AddressTranslator
	MemoryControllers  []*idealmemcontroller.Comp
	Storage            *mem.Storage
	InternalConnection akita.Connection
	MMUs               []*mmu.MMUImpl

	L1TLBToL2TLBConnection []*akita.DirectConnection
	TLBToMMUConnection     []*akita.DirectConnection
	L1ToL2Connection       []*akita.DirectConnection
	L2ToDramConnection     []*akita.DirectConnection
	L1ToRdmaConnection     []*akita.DirectConnection
	L2ToRdmaConnection     []*akita.DirectConnection
	SwitchConnection       []*switching.MultiFlitSwitchConnection

	ChipRDMAEngines []rdma.RDMAEngine
	PageRDMAEngines []rdma.RDMAEngine

	// InterChipletNetwork holds the chiplet-interconnect network (mesh or
	// ideal) of this GPU.
	InterChipletNetwork      []chipnetwork.InterChipletConnector
	InterChipletMagicNetwork *akita.DirectConnection

	GPUID uint64

	// LocalDataSource maps an address to the owning DRAM module's port.
	// Set by the builder at the same time as DMA's localDataSource so that
	// DirectWriteH2D routes identically to the simulated DMA path.
	LocalDataSource lmf.LowModuleFinder
	// portToMemCtrl is built lazily on first DirectWriteH2D call from
	// MemoryControllers, indexed by each controller's ToTop port (the same
	// port stored in LocalDataSource).
	portToMemCtrl map[akita.Port]*idealmemcontroller.Comp
}

// ExternalPorts returns external ports
func (g *GPU) ExternalPorts() []akita.Port {
	ports := []akita.Port{
		g.CommandProcessor.ToDriver,
		// g.RDMAEngine.ToOutside,
		g.RDMAEngine.GetRequestPort(),
		g.RDMAEngine.GetResponsePort(),
		g.PMC.RemotePort,
	}

	// for _, l2tlb := range g.L2TLBs {
	// 	ports = append(ports, l2tlb.BottomPort)
	// }

	return ports
}

// NewGPU returns a newly created GPU
func NewGPU(name string) *GPU {
	g := new(GPU)

	return g
}

// directWriteLog2AccessSize mirrors DMAEngine.Log2AccessSize (cp/dma.go:282).
// Keeping it equal guarantees DirectWriteH2D partitions writes the same way
// the simulated DMA path does, so multi-bank interleaving boundaries are
// respected. Declared as var (not const) so the shift below stays runtime
// and doesn't trip Go's compile-time constant overflow check.
var directWriteLog2AccessSize uint64 = 6

// DirectWriteH2D writes data to physical address addr by indexing the owning
// DRAM controller's Storage directly. No events are scheduled and no messages
// are sent. Used by Driver when -fast-h2d is enabled to skip CP/DMA/DRAM
// simulation for Host-to-Device transfers.
//
// The chunking and address-routing logic mirrors DMAEngine.parseMemCopyH2D
// (cp/dma.go) so the bypass and the simulated path are functionally
// equivalent.
func (g *GPU) DirectWriteH2D(addr uint64, data []byte) {
	if g.portToMemCtrl == nil {
		g.portToMemCtrl = make(map[akita.Port]*idealmemcontroller.Comp, len(g.MemoryControllers))
		for _, ctrl := range g.MemoryControllers {
			g.portToMemCtrl[ctrl.ToTop] = ctrl
		}
	}

	offset := uint64(0)
	lengthLeft := uint64(len(data))
	for lengthLeft > 0 {
		addrUnitFirstByte := addr & (^uint64(0) << directWriteLog2AccessSize)
		unitOffset := addr - addrUnitFirstByte
		lengthInUnit := uint64(1<<directWriteLog2AccessSize) - unitOffset

		length := lengthLeft
		if lengthInUnit < length {
			length = lengthInUnit
		}

		port := g.LocalDataSource.Find(addr)
		ctrl, ok := g.portToMemCtrl[port]
		if !ok {
			panic("DirectWriteH2D: LocalDataSource returned a port not owned by any of this GPU's MemoryControllers")
		}

		internalAddr := addr
		if ctrl.AddressConverter != nil {
			internalAddr = ctrl.AddressConverter.ConvertExternalToInternal(addr)
		}

		if err := ctrl.Storage.Write(internalAddr, data[offset:offset+length]); err != nil {
			panic(err)
		}

		addr += length
		lengthLeft -= length
		offset += length
	}
}
