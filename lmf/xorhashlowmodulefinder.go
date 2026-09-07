// XOR-hash family of LowModuleFinders. These replace the modulo-based
// striped finders to break stride-aliased access patterns (e.g. column-
// major sweep of a matrix whose row size is a multiple of the bank
// stripe period). All three finders use the SAME prime-multiply
// (Fibonacci) hash on the bank-within-chiplet selection so that a given
// address always lands on the same L2 bank — whether routed by L1,
// RDMA, or MMU.
//
// Chiplet-level routing (which chiplet owns the page) is unchanged —
// it stays as plain modulo by ChipletStripe, matching the LASP
// allocator's per-chiplet page placement.
package lmf

import (
	"math/bits"

	"gitlab.com/akita/akita"
)

// xorHashMagic = floor(2^64 / phi). Standard Fibonacci-hash constant —
// coprime to 2^64 with a well-distributed bit pattern, so its product
// with any input mixes all input bits into every output bit.
const xorHashMagic = uint64(0x9E3779B97F4A7C15)

// hashBank returns the within-chiplet bank index for the given bank-
// stripe-quantized address. Uses prime-multiply + top-bits extraction
// for power-of-2 numBanks, modulo fallback otherwise.
func hashBank(bankOffset, numBanks uint64) uint64 {
	hash := bankOffset * xorHashMagic
	if numBanks&(numBanks-1) == 0 {
		shift := uint(64 - bits.TrailingZeros64(numBanks))
		return hash >> shift
	}
	return hash % numBanks
}

// ---------------------------------------------------------------------
// XORHashLowModuleFinder — drop-in for StripedLowModuleFinder.
// ---------------------------------------------------------------------

// XORHashLowModuleFinder routes addresses to LowModules. Mirrors
// StripedLowModuleFinder's two modes (single-stripe / dual-stripe) but
// applies xorHashMagic on the bank index to break stride aliasing.
type XORHashLowModuleFinder struct {
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
func (f *XORHashLowModuleFinder) Find(address uint64) akita.Port {
	address = address - f.MemAddrOffset

	if f.ChipletStripe == 0 {
		i := hashBank(address/f.Striping, f.NumMemoryBankPerChiplet)
		return f.LowModules[i]
	}

	chiplet := (address / f.ChipletStripe) % f.NumChiplets
	bank := hashBank(address/f.BankStripe, f.NumBanksPerChiplet)
	return f.LowModules[chiplet*f.NumBanksPerChiplet+bank]
}

// NewXORHashLowModuleFinder constructs the legacy single-stripe variant.
// Drop-in replacement for NewStripedLowModuleFinder.
func NewXORHashLowModuleFinder(
	memAddrOffset uint64,
	numMemoryBankPerChiplet uint64,
	striping uint64,
) *XORHashLowModuleFinder {
	f := new(XORHashLowModuleFinder)
	f.MemAddrOffset = memAddrOffset
	f.NumMemoryBankPerChiplet = numMemoryBankPerChiplet
	f.Striping = striping
	f.LowModules = make([]akita.Port, 0)
	return f
}

// NewDualStripeXORHashLowModuleFinder constructs the dual-stripe variant
// for global fan-out across all chiplets. The caller appends LowModules
// in chiplet-major order: entry [c*numBanksPerChiplet + b] is chiplet c's
// local bank b. Drop-in replacement for NewDualStripeStripedLowModuleFinder.
func NewDualStripeXORHashLowModuleFinder(
	memAddrOffset uint64,
	numChiplets uint64,
	numBanksPerChiplet uint64,
	chipletStripe uint64,
	bankStripe uint64,
) *XORHashLowModuleFinder {
	f := new(XORHashLowModuleFinder)
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

// ---------------------------------------------------------------------
// XORHashSingleVRemoteLowModuleFinder — drop-in for SingleVRemoteLowModuleFinder.
// ---------------------------------------------------------------------

// XORHashSingleVRemoteLowModuleFinder routes a single bank's traffic
// to either its local LowModule (DRAM) or to RemoteModule (RDMA),
// based on whether the hash-derived global bank id matches BankId.
type XORHashSingleVRemoteLowModuleFinder struct {
	LowModule     akita.Port
	RemoteModule  akita.Port
	MemAddrOffset uint64
	NumBanks      uint64
	BankId        uint64
	Striping      uint64

	// Dual-stripe fields. Active when ChipletStripe != 0.
	ChipletStripe      uint64
	BankStripe         uint64
	NumChiplets        uint64
	NumBanksPerChiplet uint64
}

// Find returns the local module if the hashed global bank index equals
// BankId, otherwise the remote module (RDMA).
func (f *XORHashSingleVRemoteLowModuleFinder) Find(address uint64) akita.Port {
	address = address - f.MemAddrOffset

	var i uint64
	if f.ChipletStripe == 0 {
		i = hashBank(address/f.Striping, f.NumBanks)
	} else {
		chiplet := (address / f.ChipletStripe) % f.NumChiplets
		bank := hashBank(address/f.BankStripe, f.NumBanksPerChiplet)
		i = chiplet*f.NumBanksPerChiplet + bank
	}

	if i == f.BankId {
		return f.LowModule
	}
	return f.RemoteModule
}

// NewXORHashSingleVRemoteLowModuleFinder is the legacy single-stripe
// variant. Drop-in for NewSingleVRemoteLowModuleFinder.
func NewXORHashSingleVRemoteLowModuleFinder(
	lowModule akita.Port,
	remoteModule akita.Port,
	memAddrOffset, numBanks, striping, bankId uint64,
) *XORHashSingleVRemoteLowModuleFinder {
	f := new(XORHashSingleVRemoteLowModuleFinder)
	f.LowModule = lowModule
	f.RemoteModule = remoteModule
	f.MemAddrOffset = memAddrOffset
	f.NumBanks = numBanks
	f.Striping = striping
	f.BankId = bankId
	return f
}

// NewDualStripeXORHashSingleVRemoteLowModuleFinder is the dual-stripe
// variant. bankId is the global bank index. Drop-in for
// NewDualStripeSingleVRemoteLowModuleFinder.
func NewDualStripeXORHashSingleVRemoteLowModuleFinder(
	lowModule akita.Port,
	remoteModule akita.Port,
	memAddrOffset uint64,
	numChiplets uint64,
	numBanksPerChiplet uint64,
	chipletStripe uint64,
	bankStripe uint64,
	bankId uint64,
) *XORHashSingleVRemoteLowModuleFinder {
	f := new(XORHashSingleVRemoteLowModuleFinder)
	f.LowModule = lowModule
	f.RemoteModule = remoteModule
	f.MemAddrOffset = memAddrOffset
	f.NumBanks = numChiplets * numBanksPerChiplet
	f.NumChiplets = numChiplets
	f.NumBanksPerChiplet = numBanksPerChiplet
	f.ChipletStripe = chipletStripe
	f.BankStripe = bankStripe
	f.Striping = bankStripe
	f.BankId = bankId
	return f
}

// ---------------------------------------------------------------------
// XORHashLocalVRemoteLowModuleFinder — drop-in for StripedLocalVRemoteLowModuleFinder.
// ---------------------------------------------------------------------

// XORHashLocalVRemoteLowModuleFinder routes to one of LowModules
// (when the hashed global bank index falls in [LocalBankStart,
// LocalBankEnd]) or to RemoteModule otherwise. Used by the MMU to
// route page-walk requests to local DRAM versus remote RDMA.
type XORHashLocalVRemoteLowModuleFinder struct {
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

// Find returns LowModules[i-LocalBankStart] when i is in the local
// range, otherwise RemoteModule.
func (f *XORHashLocalVRemoteLowModuleFinder) Find(address uint64) akita.Port {
	address = address - f.MemAddrOffset

	var i uint64
	if f.ChipletStripe == 0 {
		i = hashBank(address/f.Striping, f.NumBanks)
	} else {
		chiplet := (address / f.ChipletStripe) % f.NumChiplets
		bank := hashBank(address/f.BankStripe, f.NumBanksPerChiplet)
		i = chiplet*f.NumBanksPerChiplet + bank
	}

	if f.LocalBankStart <= i && i <= f.LocalBankEnd {
		return f.LowModules[i-f.LocalBankStart]
	}
	return f.RemoteModule
}

// NewXORHashLocalVRemoteLowModuleFinder is the legacy single-stripe variant.
func NewXORHashLocalVRemoteLowModuleFinder(
	memAddrOffset, numBanks, striping, start, end uint64,
) *XORHashLocalVRemoteLowModuleFinder {
	f := new(XORHashLocalVRemoteLowModuleFinder)
	f.MemAddrOffset = memAddrOffset
	f.NumBanks = numBanks
	f.Striping = striping
	f.LocalBankStart = start
	f.LocalBankEnd = end
	f.LowModules = make([]akita.Port, 0)
	return f
}

// NewDualStripeXORHashLocalVRemoteLowModuleFinder is the dual-stripe
// variant. start/end are global bank indices.
func NewDualStripeXORHashLocalVRemoteLowModuleFinder(
	memAddrOffset uint64,
	numChiplets uint64,
	numBanksPerChiplet uint64,
	chipletStripe uint64,
	bankStripe uint64,
	start uint64,
	end uint64,
) *XORHashLocalVRemoteLowModuleFinder {
	f := new(XORHashLocalVRemoteLowModuleFinder)
	f.MemAddrOffset = memAddrOffset
	f.NumBanks = numChiplets * numBanksPerChiplet
	f.NumChiplets = numChiplets
	f.NumBanksPerChiplet = numBanksPerChiplet
	f.ChipletStripe = chipletStripe
	f.BankStripe = bankStripe
	f.Striping = bankStripe
	f.LocalBankStart = start
	f.LocalBankEnd = end
	f.LowModules = make([]akita.Port, 0)
	return f
}
