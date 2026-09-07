package cache

import (
	"gitlab.com/akita/akita"
)

func GetCacheLineID(
	addr uint64,
	blockSizeAsPowerOf2 uint64,
) (cacheLineID, offset uint64) {
	mask := uint64(0xffffffffffffffff << blockSizeAsPowerOf2)
	cacheLineID = addr & mask
	offset = addr & ^mask
	return
}

func GetBankID(block *Block, wayAssocitivity, numBanks int) int {
	return (block.SetID*wayAssocitivity + block.WayID) % numBanks
}

func ClearPort(p akita.Port, now akita.VTimeInSec) {
	for {
		item := p.Retrieve(now)
		if item == nil {
			return
		}
	}
}
