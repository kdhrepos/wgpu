package idealmemcontroller

import (
	"fmt"
	"log"
	"math/bits"
)

// xorHashMagic — keep in sync with lmf/xorhashlowmodulefinder.go.
// Used by XORHashDualStripeConverter for bank verification.
const xorHashMagic = uint64(0x9E3779B97F4A7C15)

// hashBank returns the bank index for the given bank-stripe-quantized
// address under the prime-multiply hash. Mirrors lmf.hashBank exactly.
func hashBank(bankOffset, numBanks uint64) uint64 {
	hash := bankOffset * xorHashMagic
	if numBanks&(numBanks-1) == 0 {
		shift := uint(64 - bits.TrailingZeros64(numBanks))
		return hash >> shift
	}
	return hash % numBanks
}

// XORHashDualStripeConverter mirrors DualStripeConverter but verifies
// bank ownership using the SAME prime-multiply hash used by the
// XORHashLowModuleFinder family in lmf/. Chiplet ownership stays
// modulo (matches LASP allocator's chiplet placement).
//
// Internal offset is the identity mapping (external - Offset). This
// trades compact storage layout for a clean bijection — the backing
// Storage uses small sparse units so total memory usage is still
// bounded by the workload's actual footprint.
type XORHashDualStripeConverter struct {
	ChipletStripe      uint64
	BankStripe         uint64
	NumChiplets        uint64
	NumBanksPerChiplet uint64
	CurrentChiplet     uint64
	CurrentLocalBank   uint64
	Offset             uint64
}

// ConvertExternalToInternal verifies (chiplet via modulo, bank via
// hash) and returns the address minus the GPU base as internal offset.
func (c XORHashDualStripeConverter) ConvertExternalToInternal(
	external uint64,
) uint64 {
	if external < c.Offset {
		fmt.Println(external, c.Offset)
		log.Panic("address is smaller than offset")
	}

	addr := external - c.Offset

	chiplet := (addr / c.ChipletStripe) % c.NumChiplets
	bank := hashBank(addr/c.BankStripe, c.NumBanksPerChiplet)

	if chiplet != c.CurrentChiplet || bank != c.CurrentLocalBank {
		log.Panicf(
			"address 0x%x does not belong to DRAM (chiplet=%d, bank=%d); "+
				"hash-computed (chiplet=%d, bank=%d) under chipletStripe=0x%x "+
				"bankStripe=0x%x numChiplets=%d numBanksPerChiplet=%d",
			external, c.CurrentChiplet, c.CurrentLocalBank,
			chiplet, bank,
			c.ChipletStripe, c.BankStripe,
			c.NumChiplets, c.NumBanksPerChiplet,
		)
	}

	return addr
}

// ConvertInternalToExternal is unused under hash routing.
func (c XORHashDualStripeConverter) ConvertInternalToExternal(
	internal uint64,
) uint64 {
	panic("this function should never be called")
}

// AddressConverter can translate the address between two domains
type AddressConverter interface {
	ConvertExternalToInternal(external uint64) uint64
	ConvertInternalToExternal(internal uint64) uint64
}

// InterleavingConverter is an address converter that can converts the address
// from an continuous address space to interleaved addresses.

// For example, DRAM is divided into multiple banks, the internal address
// of each bank starts from 0, while the global address is continuous. In
// this case, we can use the InterleavingConverter to convert the
// external addresses from/ to internal addresses.
type InterleavingConverter struct {
	InterleavingSize    uint64
	TotalNumOfElements  int
	CurrentElementIndex int
	Offset              uint64
}

// ConvertExternalToInternal converts from external address to internal address
func (c InterleavingConverter) ConvertExternalToInternal(external uint64) uint64 {
	if external < c.Offset {
		fmt.Println(external, c.Offset)
		log.Panic("address is smaller than offset")
	}

	addr := external - c.Offset
	roundSize := c.InterleavingSize * uint64(c.TotalNumOfElements)
	belongsTo := int(addr % roundSize / c.InterleavingSize)

	if belongsTo != c.CurrentElementIndex {
		fmt.Println(
			fmt.Sprintf("%x", roundSize),
			c.InterleavingSize,
			c.TotalNumOfElements,
			c.CurrentElementIndex,
			c.Offset,
			belongsTo,
			fmt.Sprintf("%x", addr),
			fmt.Sprintf("%x", addr%roundSize))
		log.Panicf("address 0x%x does not belongs to current element %d",
			external, c.CurrentElementIndex)
	}

	internal := addr/(roundSize)*c.InterleavingSize +
		external%c.InterleavingSize
	return internal
}

// ConvertInternalToExternal converts from internal address to external address
func (c InterleavingConverter) ConvertInternalToExternal(internal uint64) uint64 {
	panic("this function should never be called")
}

// DualStripeConverter maps external addresses to a contiguous internal
// per-bank offset when the address space is interleaved on TWO independent
// stripes:
//
//  1. Chiplet stripe (typically the page size) — at this granularity the
//     address space rotates through chiplets. So consecutive `ChipletStripe`
//     bytes within a chiplet-round are all on the same chiplet.
//  2. Bank stripe (typically the cacheline size) — within each chiplet chunk
//     the cachelines rotate across that chiplet's local banks.
//
// A single physical bank at (chipletId=C, localBankId=K) serves addresses X
// where (X/ChipletStripe) % NumChiplets == C and (X/BankStripe) %
// NumBanksPerChiplet == K. ChipletStripe must be a multiple of
// (BankStripe * NumBanksPerChiplet); the ratio determines how many cachelines
// of one chiplet chunk land on each bank.
//
// Bijection from external X to internal offset:
//
//	chipletRound       = X / (ChipletStripe * NumChiplets)
//	withinChipletChunk = X % ChipletStripe
//	bankRoundInChunk   = withinChipletChunk / (BankStripe * NumBanksPerChiplet)
//	byteInCacheline    = X % BankStripe
//	cachelinesPerChunk = ChipletStripe / (BankStripe * NumBanksPerChiplet)
//	internal = (chipletRound*cachelinesPerChunk + bankRoundInChunk)*BankStripe
//	         + byteInCacheline
//
// Reduces to the single-stripe `InterleavingConverter` exactly when
// ChipletStripe == BankStripe * NumBanksPerChiplet.
type DualStripeConverter struct {
	ChipletStripe      uint64
	BankStripe         uint64
	NumChiplets        uint64
	NumBanksPerChiplet uint64
	CurrentChiplet     uint64
	CurrentLocalBank   uint64
	Offset             uint64
}

// ConvertExternalToInternal converts from external address to internal offset.
func (c DualStripeConverter) ConvertExternalToInternal(external uint64) uint64 {
	if external < c.Offset {
		fmt.Println(external, c.Offset)
		log.Panic("address is smaller than offset")
	}

	addr := external - c.Offset

	chiplet := (addr / c.ChipletStripe) % c.NumChiplets
	bank := (addr / c.BankStripe) % c.NumBanksPerChiplet

	if chiplet != c.CurrentChiplet || bank != c.CurrentLocalBank {
		log.Panicf(
			"address 0x%x does not belong to DRAM (chiplet=%d, bank=%d); "+
				"computed (chiplet=%d, bank=%d) under chipletStripe=0x%x "+
				"bankStripe=0x%x numChiplets=%d numBanksPerChiplet=%d",
			external, c.CurrentChiplet, c.CurrentLocalBank,
			chiplet, bank,
			c.ChipletStripe, c.BankStripe,
			c.NumChiplets, c.NumBanksPerChiplet,
		)
	}

	chipletRoundSize := c.ChipletStripe * c.NumChiplets
	bankRoundSize := c.BankStripe * c.NumBanksPerChiplet
	cachelinesPerChunk := c.ChipletStripe / bankRoundSize

	chipletRound := addr / chipletRoundSize
	withinChunk := addr % c.ChipletStripe
	bankRoundInChunk := withinChunk / bankRoundSize
	byteInCacheline := addr % c.BankStripe

	internal := (chipletRound*cachelinesPerChunk+bankRoundInChunk)*c.BankStripe +
		byteInCacheline
	return internal
}

// ConvertInternalToExternal converts from internal address to external address
func (c DualStripeConverter) ConvertInternalToExternal(internal uint64) uint64 {
	panic("this function should never be called")
}
