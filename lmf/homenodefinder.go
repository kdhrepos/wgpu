package lmf

import "gitlab.com/akita/akita"

// HomeNodeFinder maps an address to its owning chiplet ID. `Striping` is the
// chiplet stripe — the address granularity at which chiplet ownership
// rotates. Under dual-stripe this MUST equal
// `numBanksPerChiplet × pageSize` so that it agrees with both the routing
// layer's chiplet check and the LASP allocator's per-chiplet chunk size.
// With defaults (16 banks × 4KB page) this is 64KB.
type HomeNodeFinder struct {
	MemAddrOffset uint64
	NumChiplets   uint64
	Striping      uint64 // chiplet stripe (= numBanksPerChiplet × pageSize)
}

// Find returns the port that can provide the data.
func (f *HomeNodeFinder) Find(address uint64) akita.Port {
	panic("Do not used Find!")
}

func (f *HomeNodeFinder) GetChipletId(address uint64) uint64 {
	address = address - f.MemAddrOffset
	i := (address % (f.NumChiplets * f.Striping)) / f.Striping
	return i
}

// NewHomeNodeFinder constructs a HomeNodeFinder. `striping` is the chiplet
// stripe — pass `numBanksPerChiplet × pageSize` to match the routing layer
// and the LASP allocator (in mgpusim builders this is exposed as
// CommonBuilder.chipletInterleavingSize()).
func NewHomeNodeFinder(
	memAddrOffset uint64,
	numChiplets uint64,
	striping uint64,
) *HomeNodeFinder {
	f := new(HomeNodeFinder)
	f.MemAddrOffset = memAddrOffset
	f.NumChiplets = numChiplets
	f.Striping = striping
	return f
}
