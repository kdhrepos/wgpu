package cache

// import (
// 	"gitlab.com/akita/akita"
// )

// // LowModuleFinder helps a cache unit or a akita to find the low module that
// // should hold the data at a certain address
// type LowModuleFinder interface {
// 	Find(address uint64) akita.Port
// }

// // SingleLowModuleFinder is used when a unit is connected with only one
// // low module
// type SingleLowModuleFinder struct {
// 	LowModule akita.Port
// }

// // Find simply returns the solo unit that it connects to
// func (f *SingleLowModuleFinder) Find(address uint64) akita.Port {
// 	return f.LowModule
// }

// // SingleVRemoteLowModuleFinder is used when a unit is connected
// // with only one local module and remote module
// type SingleVRemoteLowModuleFinder struct {
// 	LowModule     akita.Port
// 	MemAddrOffset uint64
// 	NumBanks      uint64
// 	BankId        uint64
// 	Striping      uint64
// 	// LocalBankStart          uint64
// 	// LocalBankEnd            uint64
// 	RemoteModule akita.Port
// }

// // Find simply returns the solo unit that it connects to
// func (f *SingleVRemoteLowModuleFinder) Find(address uint64) akita.Port {
// 	address = address - f.MemAddrOffset
// 	i := (address % (f.NumBanks * f.Striping)) / f.Striping

// 	if i == f.BankId {
// 		return f.LowModule
// 	}
// 	return f.RemoteModule // this must return port from RDMA engine
// }

// func NewSingleVRemoteLowModuleFinder(
// 	lowModule akita.Port,
// 	remoteModule akita.Port,
// 	memAddrOffset, numBanks, striping, bankId uint64,
// ) *SingleVRemoteLowModuleFinder {
// 	f := new(SingleVRemoteLowModuleFinder)
// 	f.LowModule = lowModule
// 	f.RemoteModule = remoteModule
// 	f.MemAddrOffset = memAddrOffset
// 	f.NumBanks = numBanks
// 	f.Striping = striping
// 	f.BankId = bankId
// 	return f
// }

// // only used for L1 cache
// type LocalRemoteLowModuleFinder struct {
// 	LowModules              []akita.Port
// 	MemAddrOffset           uint64
// 	NumBanks                uint64
// 	NumMemoryBankPerChiplet uint64
// 	Striping                uint64
// }

// // Find simply returns the solo unit that it connects to
// func (f *LocalRemoteLowModuleFinder) Find(address uint64) akita.Port {
// 	address = address - f.MemAddrOffset
// 	i := ((address % (f.NumBanks * f.Striping)) / f.Striping) % f.NumMemoryBankPerChiplet

// 	return f.LowModules[i]
// }

// func NewLocalRemoteLowModuleFinder(
// 	memAddrOffset,
// 	numBanks,
// 	numMemoryBankPerChiplet,
// 	striping uint64,
// ) *LocalRemoteLowModuleFinder {
// 	f := new(LocalRemoteLowModuleFinder)
// 	f.LowModules = make([]akita.Port, 0)
// 	f.MemAddrOffset = memAddrOffset
// 	f.NumBanks = numBanks
// 	f.NumMemoryBankPerChiplet = numMemoryBankPerChiplet
// 	f.Striping = striping
// 	return f
// }

// // InterleavedLowModuleFinder helps find the low module when the low modules
// // maintains interleaved address space
// type InterleavedLowModuleFinder struct {
// 	UseAddressSpaceLimitation bool
// 	LowAddress                uint64
// 	HighAddress               uint64
// 	InterleavingSize          uint64
// 	LowModules                []akita.Port
// 	ModuleForOtherAddresses   akita.Port
// }

// // Find returns the low module that has the data at provided address
// func (f *InterleavedLowModuleFinder) Find(address uint64) akita.Port {
// 	if f.UseAddressSpaceLimitation &&
// 		(address >= f.HighAddress || address < f.LowAddress) {
// 		return f.ModuleForOtherAddresses
// 	}

// 	number := address / f.InterleavingSize % uint64(len(f.LowModules))
// 	return f.LowModules[number]
// }

// // NewInterleavedLowModuleFinder creates a new finder for interleaved lower
// // modules
// func NewInterleavedLowModuleFinder(interleavingSize uint64) *InterleavedLowModuleFinder {
// 	f := new(InterleavedLowModuleFinder)
// 	f.LowModules = make([]akita.Port, 0)
// 	f.InterleavingSize = interleavingSize
// 	return f
// }

// // BankedLowModuleFinder defines the lower level modules by address banks
// type BankedLowModuleFinder struct {
// 	MemAddrOffset uint64
// 	BankSize      uint64
// 	LowModules    []akita.Port
// }

// // Find returns the port that can provide the data.
// func (f *BankedLowModuleFinder) Find(address uint64) akita.Port {
// 	address = address - f.MemAddrOffset
// 	i := address / f.BankSize % f.BankSize

// 	return f.LowModules[i]
// }

// // func (f *BankedLowModuleFinder) GetModule(i uint64) akita.Port {
// // 	return f.LowModules[i]
// // }

// // NewBankedLowModuleFinder returns a new BankedLowModuleFinder.
// func NewBankedLowModuleFinder(memAddrOffset, bankSize uint64) *BankedLowModuleFinder {
// 	f := new(BankedLowModuleFinder)
// 	f.MemAddrOffset = memAddrOffset
// 	f.BankSize = bankSize
// 	f.LowModules = make([]akita.Port, 0)
// 	return f
// }

// type XORLowModuleFinder struct {
// 	NumLowModules  int
// 	NumTerms       int
// 	NumBitsPerTerm int
// 	OffsetBits     int
// 	LowModules     []akita.Port
// }

// func (f *XORLowModuleFinder) Find(address uint64) akita.Port {
// 	index := uint64(0)
// 	address = address >> f.OffsetBits
// 	mask := (uint64(1) << f.NumBitsPerTerm) - 1
// 	for i := 0; i < f.NumTerms; i++ {
// 		index = index ^ (address & mask)
// 		address = address >> f.NumBitsPerTerm
// 	}
// 	return f.LowModules[index]
// }

// func NewXORLowModuleFinder(numModules int, numTerms int, numBitsPerTerm int,
// 	offsetBits int) *XORLowModuleFinder {
// 	f := new(XORLowModuleFinder)
// 	f.NumLowModules = numModules
// 	f.NumTerms = numTerms
// 	f.NumBitsPerTerm = numBitsPerTerm
// 	f.OffsetBits = offsetBits

// 	return f
// }

// // BankedLowModuleFinder defines the lower level modules by address banks
// type StripedLowModuleFinder struct {
// 	MemAddrOffset           uint64
// 	NumMemoryBankPerChiplet uint64
// 	Striping                uint64
// 	LowModules              []akita.Port
// }

// // Find returns the port that can provide the data.
// func (f *StripedLowModuleFinder) Find(address uint64) akita.Port {
// 	address = address - f.MemAddrOffset
// 	i := (address % (f.NumMemoryBankPerChiplet * f.Striping)) / f.Striping
// 	return f.LowModules[i]
// }

// // NewBankedLowModuleFinder returns a new BankedLowModuleFinder.
// func NewStripedLowModuleFinder(
// 	memAddrOffset,
// 	numMemoryBankPerChiplet,
// 	striping uint64,
// ) *StripedLowModuleFinder {
// 	f := new(StripedLowModuleFinder)
// 	f.MemAddrOffset = memAddrOffset
// 	f.NumMemoryBankPerChiplet = numMemoryBankPerChiplet
// 	f.Striping = striping
// 	f.LowModules = make([]akita.Port, 0)
// 	return f
// }

// // StripedLocalVRemoteLowModuleFinder defines the lower level modules by address banks
// type StripedLocalVRemoteLowModuleFinder struct {
// 	MemAddrOffset  uint64
// 	NumBanks       uint64
// 	Striping       uint64
// 	LocalBankStart uint64
// 	LocalBankEnd   uint64
// 	LowModules     []akita.Port
// 	RemoteModule   akita.Port
// }

// // Find returns the port that can provide the data.
// func (f *StripedLocalVRemoteLowModuleFinder) Find(address uint64) akita.Port {
// 	address = address - f.MemAddrOffset
// 	i := (address % (f.NumBanks * f.Striping)) / f.Striping

// 	if f.LocalBankStart <= i && i <= f.LocalBankEnd {
// 		return f.LowModules[i-f.LocalBankStart]
// 	}
// 	return f.RemoteModule
// }

// // NewStripedLocalVRemoteLowModuleFinder returns a new StripedLocalVRemoteLowModuleFinder.
// func NewStripedLocalVRemoteLowModuleFinder(
// 	memAddrOffset, numBanks, striping, start, end uint64,
// ) *StripedLocalVRemoteLowModuleFinder {
// 	f := new(StripedLocalVRemoteLowModuleFinder)
// 	f.MemAddrOffset = memAddrOffset
// 	f.NumBanks = numBanks
// 	f.Striping = striping
// 	f.LocalBankStart = start
// 	f.LocalBankEnd = end
// 	f.LowModules = make([]akita.Port, 0)
// 	return f
// }
