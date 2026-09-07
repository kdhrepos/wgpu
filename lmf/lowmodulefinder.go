package lmf

import (
	"log"

	"gitlab.com/akita/akita"
)

// LowModuleFinder helps a cache unit or a akita to find the low module that
// should hold the data at a certain address
type LowModuleFinder interface {
	Find(address uint64) akita.Port
}

// SingleLowModuleFinder is used when a unit is connected with only one
// low module
type SingleLowModuleFinder struct {
	LowModule akita.Port
}

// Find simply returns the solo unit that it connects to
func (f *SingleLowModuleFinder) Find(address uint64) akita.Port {
	return f.LowModule
}

// SingleVRemoteLowModuleFinder is used when a unit is connected
// with only one local module and remote module.
//
// Supports two configurations:
//
//  1. Single-stripe (legacy): set Striping and NumBanks. BankId is the global
//     bank index. Find uses `(addr/Striping) % NumBanks`.
//  2. Dual-stripe: set ChipletStripe + BankStripe + NumChiplets +
//     NumBanksPerChiplet. BankId is still the global bank index
//     (= chipletId * NumBanksPerChiplet + localBankId). Find composes
//     globalBank from the two-axis decomposition.
//
// Dual-stripe is selected when ChipletStripe != 0. Single-stripe behavior is
// preserved exactly when ChipletStripe == BankStripe * NumBanksPerChiplet.
type SingleVRemoteLowModuleFinder struct {
	LowModule     akita.Port
	MemAddrOffset uint64
	NumBanks      uint64
	BankId        uint64
	Striping      uint64
	RemoteModule  akita.Port

	// Dual-stripe fields. Active when ChipletStripe != 0.
	ChipletStripe      uint64
	BankStripe         uint64
	NumChiplets        uint64
	NumBanksPerChiplet uint64
}

// Find simply returns the solo unit that it connects to
func (f *SingleVRemoteLowModuleFinder) Find(address uint64) akita.Port {
	if f.MemAddrOffset != 0 && address < f.MemAddrOffset {
		log.Panicf("[debug] SingleVRemoteLowModuleFinder.Find: addr=0x%x < offset=0x%x (underflow; address is below GPU memory base)",
			address, f.MemAddrOffset)
	}
	address = address - f.MemAddrOffset

	var i uint64
	if f.ChipletStripe == 0 {
		// Legacy single-stripe.
		i = (address % (f.NumBanks * f.Striping)) / f.Striping
	} else {
		// Dual-stripe: chiplet by ChipletStripe, bank by BankStripe.
		chiplet := (address / f.ChipletStripe) % f.NumChiplets
		bank := (address / f.BankStripe) % f.NumBanksPerChiplet
		i = chiplet*f.NumBanksPerChiplet + bank
	}

	if i == f.BankId {
		return f.LowModule
	}
	return f.RemoteModule // this must return port from RDMA engine
}

// NewSingleVRemoteLowModuleFinder constructs the legacy single-stripe variant.
func NewSingleVRemoteLowModuleFinder(
	lowModule akita.Port,
	remoteModule akita.Port,
	memAddrOffset, numBanks, striping, bankId uint64,
) *SingleVRemoteLowModuleFinder {
	f := new(SingleVRemoteLowModuleFinder)
	f.LowModule = lowModule
	f.RemoteModule = remoteModule
	f.MemAddrOffset = memAddrOffset
	f.NumBanks = numBanks
	f.Striping = striping
	f.BankId = bankId
	return f
}

// NewDualStripeSingleVRemoteLowModuleFinder constructs the dual-stripe variant.
// chipletStripe is the address granularity at which chiplet ownership rotates
// (typically the page size). bankStripe is the within-chiplet bank
// interleaving granularity (typically the cacheline size). bankId is the
// global bank index = chipletId * numBanksPerChiplet + localBankId.
func NewDualStripeSingleVRemoteLowModuleFinder(
	lowModule akita.Port,
	remoteModule akita.Port,
	memAddrOffset uint64,
	numChiplets uint64,
	numBanksPerChiplet uint64,
	chipletStripe uint64,
	bankStripe uint64,
	bankId uint64,
) *SingleVRemoteLowModuleFinder {
	f := new(SingleVRemoteLowModuleFinder)
	f.LowModule = lowModule
	f.RemoteModule = remoteModule
	f.MemAddrOffset = memAddrOffset
	f.NumBanks = numChiplets * numBanksPerChiplet
	f.NumChiplets = numChiplets
	f.NumBanksPerChiplet = numBanksPerChiplet
	f.ChipletStripe = chipletStripe
	f.BankStripe = bankStripe
	f.Striping = bankStripe // keep legacy field in sync (some callers may read)
	f.BankId = bankId
	return f
}

// only used for L1 cache
type LocalRemoteLowModuleFinder struct {
	LowModules              []akita.Port
	MemAddrOffset           uint64
	NumBanks                uint64
	NumMemoryBankPerChiplet uint64
	Striping                uint64
}

// Find simply returns the solo unit that it connects to
func (f *LocalRemoteLowModuleFinder) Find(address uint64) akita.Port {
	address = address - f.MemAddrOffset
	i := ((address % (f.NumBanks * f.Striping)) / f.Striping) % f.NumMemoryBankPerChiplet

	return f.LowModules[i]
}

func NewLocalRemoteLowModuleFinder(
	memAddrOffset,
	numBanks,
	numMemoryBankPerChiplet,
	striping uint64,
) *LocalRemoteLowModuleFinder {
	f := new(LocalRemoteLowModuleFinder)
	f.LowModules = make([]akita.Port, 0)
	f.MemAddrOffset = memAddrOffset
	f.NumBanks = numBanks
	f.NumMemoryBankPerChiplet = numMemoryBankPerChiplet
	f.Striping = striping
	return f
}

// InterleavedLowModuleFinder helps find the low module when the low modules
// maintains interleaved address space
type InterleavedLowModuleFinder struct {
	UseAddressSpaceLimitation bool
	LowAddress                uint64
	HighAddress               uint64
	InterleavingSize          uint64
	LowModules                []akita.Port
	ModuleForOtherAddresses   akita.Port
}

// Find returns the low module that has the data at provided address
func (f *InterleavedLowModuleFinder) Find(address uint64) akita.Port {
	if f.UseAddressSpaceLimitation &&
		(address >= f.HighAddress || address < f.LowAddress) {
		return f.ModuleForOtherAddresses
	}

	number := address / f.InterleavingSize % uint64(len(f.LowModules))
	return f.LowModules[number]
}

// NewInterleavedLowModuleFinder creates a new finder for interleaved lower
// modules
func NewInterleavedLowModuleFinder(interleavingSize uint64) *InterleavedLowModuleFinder {
	f := new(InterleavedLowModuleFinder)
	f.LowModules = make([]akita.Port, 0)
	f.InterleavingSize = interleavingSize
	return f
}

// BankedLowModuleFinder defines the lower level modules by address banks
type BankedLowModuleFinder struct {
	MemAddrOffset uint64
	BankSize      uint64
	LowModules    []akita.Port
}

// Find returns the port that can provide the data.
func (f *BankedLowModuleFinder) Find(address uint64) akita.Port {
	address = address - f.MemAddrOffset
	i := address / f.BankSize % f.BankSize

	return f.LowModules[i]
}

// func (f *BankedLowModuleFinder) GetModule(i uint64) akita.Port {
// 	return f.LowModules[i]
// }

// NewBankedLowModuleFinder returns a new BankedLowModuleFinder.
func NewBankedLowModuleFinder(memAddrOffset, bankSize uint64) *BankedLowModuleFinder {
	f := new(BankedLowModuleFinder)
	f.MemAddrOffset = memAddrOffset
	f.BankSize = bankSize
	f.LowModules = make([]akita.Port, 0)
	return f
}

type XORLowModuleFinder struct {
	NumLowModules  int
	NumTerms       int
	NumBitsPerTerm int
	OffsetBits     int
	LowModules     []akita.Port
}

func (f *XORLowModuleFinder) Find(address uint64) akita.Port {
	index := uint64(0)
	address = address >> f.OffsetBits
	mask := (uint64(1) << f.NumBitsPerTerm) - 1
	for i := 0; i < f.NumTerms; i++ {
		index = index ^ (address & mask)
		address = address >> f.NumBitsPerTerm
	}
	return f.LowModules[index]
}

func NewXORLowModuleFinder(numModules int, numTerms int, numBitsPerTerm int,
	offsetBits int) *XORLowModuleFinder {
	f := new(XORLowModuleFinder)
	f.NumLowModules = numModules
	f.NumTerms = numTerms
	f.NumBitsPerTerm = numBitsPerTerm
	f.OffsetBits = offsetBits

	return f
}

// StripedLowModuleFinder distributes addresses across a fan-out of low
// modules using a single stripe (`Striping`).
//
// Two usage modes:
//
//  1. Local fan-out (NumMemoryBankPerChiplet entries): used by L1→L2 within a
//     chiplet, RDMA→L2 on receive side, MMU→L2 for PTE caching, etc. Only
//     needs the bank stripe — chiplet identity is not represented here.
//     Construct with NewStripedLowModuleFinder.
//
//  2. Global fan-out across all chiplets (NumChiplets * NumBanksPerChiplet
//     entries, ordered chiplet-major): used by DMA. Under dual-stripe routing,
//     the routing key is (chiplet, bank) decomposed by two stripes.
//     Construct with NewDualStripeStripedLowModuleFinder.
//
// Dual-stripe mode is selected when ChipletStripe != 0.
type StripedLowModuleFinder struct {
	MemAddrOffset           uint64
	NumMemoryBankPerChiplet uint64
	Striping                uint64
	LowModules              []akita.Port

	// Dual-stripe fields. Active when ChipletStripe != 0.
	ChipletStripe      uint64
	BankStripe         uint64
	NumChiplets        uint64
	NumBanksPerChiplet uint64
}

// Find returns the port that can provide the data.
func (f *StripedLowModuleFinder) Find(address uint64) akita.Port {
	if f.MemAddrOffset != 0 && address < f.MemAddrOffset {
		log.Panicf("[debug] StripedLowModuleFinder.Find: addr=0x%x < offset=0x%x (underflow; address is below GPU memory base)",
			address, f.MemAddrOffset)
	}
	address = address - f.MemAddrOffset

	if f.ChipletStripe == 0 {
		// Legacy single-stripe.
		i := (address % (f.NumMemoryBankPerChiplet * f.Striping)) / f.Striping
		return f.LowModules[i]
	}

	// Dual-stripe global fan-out (chiplet-major ordering).
	chiplet := (address / f.ChipletStripe) % f.NumChiplets
	bank := (address / f.BankStripe) % f.NumBanksPerChiplet
	i := chiplet*f.NumBanksPerChiplet + bank
	return f.LowModules[i]
}

// NewStripedLowModuleFinder constructs the legacy single-stripe variant.
func NewStripedLowModuleFinder(
	memAddrOffset uint64,
	numMemoryBankPerChiplet uint64,
	striping uint64,
) *StripedLowModuleFinder {
	f := new(StripedLowModuleFinder)
	f.MemAddrOffset = memAddrOffset
	f.NumMemoryBankPerChiplet = numMemoryBankPerChiplet
	f.Striping = striping
	f.LowModules = make([]akita.Port, 0)
	return f
}

// NewDualStripeStripedLowModuleFinder constructs the dual-stripe variant for
// global fan-out across all chiplets. The caller must append LowModules in
// chiplet-major order: for chiplet C, the entries at indices
// [C*numBanksPerChiplet, (C+1)*numBanksPerChiplet) correspond to that
// chiplet's local banks 0..numBanksPerChiplet-1.
func NewDualStripeStripedLowModuleFinder(
	memAddrOffset uint64,
	numChiplets uint64,
	numBanksPerChiplet uint64,
	chipletStripe uint64,
	bankStripe uint64,
) *StripedLowModuleFinder {
	f := new(StripedLowModuleFinder)
	f.MemAddrOffset = memAddrOffset
	f.NumMemoryBankPerChiplet = numChiplets * numBanksPerChiplet
	f.NumChiplets = numChiplets
	f.NumBanksPerChiplet = numBanksPerChiplet
	f.ChipletStripe = chipletStripe
	f.BankStripe = bankStripe
	f.Striping = bankStripe
	f.LowModules = make([]akita.Port, 0)
	return f
}

// StripedLocalVRemoteLowModuleFinder defines the lower level modules by
// address banks. Used by the MMU on the bottom side: if an address belongs to
// this chiplet (bank index ∈ [LocalBankStart, LocalBankEnd]) route to the
// corresponding local DRAM, otherwise route to the remote module (RDMA pw
// port).
//
// Supports two configurations like SingleVRemoteLowModuleFinder:
//
//  1. Single-stripe (legacy): set NumBanks + Striping.
//  2. Dual-stripe: set ChipletStripe + BankStripe + NumChiplets +
//     NumBanksPerChiplet.
//
// LocalBankStart/LocalBankEnd remain global bank indices in both modes
// (= chipletId * NumBanksPerChiplet + localBankId).
type StripedLocalVRemoteLowModuleFinder struct {
	MemAddrOffset  uint64
	NumBanks       uint64
	Striping       uint64
	LocalBankStart uint64
	LocalBankEnd   uint64
	LowModules     []akita.Port
	RemoteModule   akita.Port

	// Dual-stripe fields. Active when ChipletStripe != 0.
	ChipletStripe      uint64
	BankStripe         uint64
	NumChiplets        uint64
	NumBanksPerChiplet uint64
}

// Find returns the port that can provide the data.
func (f *StripedLocalVRemoteLowModuleFinder) Find(address uint64) akita.Port {
	if f.MemAddrOffset != 0 && address < f.MemAddrOffset {
		log.Panicf("[debug] StripedLocalVRemoteLowModuleFinder.Find: addr=0x%x < offset=0x%x (underflow; address is below GPU memory base)",
			address, f.MemAddrOffset)
	}
	address = address - f.MemAddrOffset

	var i uint64
	if f.ChipletStripe == 0 {
		// Legacy single-stripe.
		i = (address % (f.NumBanks * f.Striping)) / f.Striping
	} else {
		// Dual-stripe: compose global bank from (chiplet, localBank).
		chiplet := (address / f.ChipletStripe) % f.NumChiplets
		bank := (address / f.BankStripe) % f.NumBanksPerChiplet
		i = chiplet*f.NumBanksPerChiplet + bank
	}

	if f.LocalBankStart <= i && i <= f.LocalBankEnd {
		return f.LowModules[i-f.LocalBankStart]
	}
	return f.RemoteModule
}

// NewStripedLocalVRemoteLowModuleFinder returns a new
// StripedLocalVRemoteLowModuleFinder in the legacy single-stripe mode.
func NewStripedLocalVRemoteLowModuleFinder(
	memAddrOffset,
	numBanks,
	striping,
	start,
	end uint64,
) *StripedLocalVRemoteLowModuleFinder {
	f := new(StripedLocalVRemoteLowModuleFinder)
	f.MemAddrOffset = memAddrOffset
	f.NumBanks = numBanks
	f.Striping = striping
	f.LocalBankStart = start
	f.LocalBankEnd = end
	f.LowModules = make([]akita.Port, 0)
	return f
}

// NewDualStripeStripedLocalVRemoteLowModuleFinder returns a finder in the
// dual-stripe mode. start/end are global bank indices.
func NewDualStripeStripedLocalVRemoteLowModuleFinder(
	memAddrOffset uint64,
	numChiplets uint64,
	numBanksPerChiplet uint64,
	chipletStripe uint64,
	bankStripe uint64,
	start uint64,
	end uint64,
) *StripedLocalVRemoteLowModuleFinder {
	f := new(StripedLocalVRemoteLowModuleFinder)
	f.MemAddrOffset = memAddrOffset
	f.NumBanks = numChiplets * numBanksPerChiplet
	f.NumChiplets = numChiplets
	f.NumBanksPerChiplet = numBanksPerChiplet
	f.ChipletStripe = chipletStripe
	f.BankStripe = bankStripe
	f.Striping = bankStripe // keep legacy field in sync
	f.LocalBankStart = start
	f.LocalBankEnd = end
	f.LowModules = make([]akita.Port, 0)
	return f
}
