package cu

import (
	"math"

	"gitlab.com/akita/akita"
	"gitlab.com/akita/mgpusim/timing/wavefront"
)

// A FetchArbiter can decide which wavefront in a scheduler can fetch
// instructions
type FetchArbiter struct {
	InstBufByteSize int
}

// Arbitrate decide which wavefront can fetch the next instruction
func (a *FetchArbiter) Arbitrate(
	wfPools []*WavefrontPool,
) []*wavefront.Wavefront {
	list := make([]*wavefront.Wavefront, 0, 1)

	oldestTime := akita.VTimeInSec(math.MaxFloat64)
	var toFetch *wavefront.Wavefront
	for _, wfPool := range wfPools {
		for _, wf := range wfPool.wfs {

			if wf.IsFetching {
				continue
			}

			if len(wf.InstBuffer) >= a.InstBufByteSize {
				continue
			}

			if wf.LastFetchTime < oldestTime {
				toFetch = wf
				oldestTime = wf.LastFetchTime
			}
		}
	}

	if toFetch != nil {
		list = append(list, toFetch)
	}

	return list
}
