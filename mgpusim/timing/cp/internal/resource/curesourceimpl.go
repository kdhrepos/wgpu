package resource

import (
	"gitlab.com/akita/akita"
	"gitlab.com/akita/mgpusim/kernels"
)

// CUResourceImpl implements CUResource
type CUResourceImpl struct {
	port akita.Port

	wfPoolFreeCount []int

	sregCount       int
	sregGranularity int
	sregMask        resourceMask

	vregCounts      []int
	vregGranularity int
	vregMasks       []resourceMask

	ldsByteSize    int
	ldsGranularity int
	ldsMask        resourceMask

	nextSIMD    int
	reservedWGs map[*kernels.WorkGroup][]WfLocation

	tempReserves []tempReservation
}

// tempReservation records a region marked allocStatusToReserve during one
// ReserveResourceForWG attempt, so commit/rollback touch only those regions
// instead of scanning the whole mask.
type tempReservation struct {
	mask   resourceMask
	offset int
	length int
}

func (r *CUResourceImpl) markToReserve(m resourceMask, offset, length int) {
	m.setStatus(offset, length, allocStatusToReserve)
	r.tempReserves = append(r.tempReserves,
		tempReservation{mask: m, offset: offset, length: length})
}

// DispatchingPort returns the port that the dispatcher send message to.
func (r *CUResourceImpl) DispatchingPort() akita.Port {
	return r.port
}

// ReserveResourceForWG checks if there is space to hold the work-group. If so,
// this function reserves the resouces for the work-group and returns how the
// resources are allocated.
func (r *CUResourceImpl) ReserveResourceForWG(wg *kernels.WorkGroup) (
	locations []WfLocation,
	ok bool,
) {
	r.tempReserves = r.tempReserves[:0]
	ok = true
	locations = make([]WfLocation, len(wg.Wavefronts))

	for i, wf := range wg.Wavefronts {
		locations[i].Wavefront = wf
	}

	if !r.withinSGPRLimitation(wg, locations) {
		ok = false
	}

	if ok && !r.withinLDSLimitation(wg, locations) {
		ok = false
	}

	if ok && !r.matchWfWithSIMDs(wg, locations) {
		ok = false
	}

	if ok {
		r.reserveResources(wg, locations)
		return locations, true
	}

	r.clearTempReservation(wg)
	return nil, ok
}

func (r *CUResourceImpl) withinSGPRLimitation(
	wg *kernels.WorkGroup,
	locations []WfLocation,
) bool {
	co := wg.CodeObject
	required := r.unitsOccupy(int(co.WFSgprCount), r.sregGranularity)

	for i := range wg.Wavefronts {
		location := &locations[i]
		offset, ok := r.sregMask.nextRegion(required, allocStatusFree)
		if !ok {
			return false
		}
		location.SGPROffset = offset * 16 * 4 // 16 reg, 4 byte each
		r.markToReserve(r.sregMask, offset, required)
	}

	return true
}

func (r *CUResourceImpl) withinLDSLimitation(
	wg *kernels.WorkGroup,
	locations []WfLocation,
) bool {
	co := wg.CodeObject
	required := r.unitsOccupy(int(co.WGGroupSegmentByteSize), r.ldsGranularity)
	offset, ok := r.ldsMask.nextRegion(required, allocStatusFree)
	if !ok {
		return false
	}

	for i := range wg.Wavefronts {
		location := &locations[i]
		location.LDSOffset = offset * r.ldsGranularity
	}

	r.markToReserve(r.ldsMask, offset, required)
	return true
}

// Maps the wfs of a work-group to the SIMDs in the compute unit
// This function sets the value of req.WfDispatchMap, to keep the information
// about which SIMD should a wf dispatch to. This function also returns
// a boolean value for if the matching is successful.
func (r *CUResourceImpl) matchWfWithSIMDs(
	wg *kernels.WorkGroup,
	locations []WfLocation,
) bool {
	vgprToUse := make([]int, len(r.wfPoolFreeCount))
	wfPoolEntryUsed := make([]int, len(r.wfPoolFreeCount))
	co := wg.CodeObject

	for i := range wg.Wavefronts {
		location := &locations[i]
		firstSIMDTested := r.nextSIMD
		firstTry := true
		found := false
		required := r.unitsOccupy(int(co.WIVgprCount), r.vregGranularity)

		for firstTry || r.nextSIMD != firstSIMDTested {
			firstTry = false
			offset, ok := r.vregMasks[r.nextSIMD].
				nextRegion(required, allocStatusFree)

			if ok && r.wfPoolFreeCount[r.nextSIMD]-wfPoolEntryUsed[r.nextSIMD] > 0 {
				found = true
				vgprToUse[r.nextSIMD] += required
				wfPoolEntryUsed[r.nextSIMD]++
				location.SIMDID = r.nextSIMD
				location.VGPROffset = offset * r.vregGranularity * 4 //  4 bytes per register
				r.markToReserve(r.vregMasks[r.nextSIMD], offset, required)
			}

			r.nextSIMD++
			if r.nextSIMD >= len(r.wfPoolFreeCount) {
				r.nextSIMD = 0
			}

			if found {
				break
			}
		}

		if !found {
			return false
		}
	}

	return true
}

func (r *CUResourceImpl) unitsOccupy(amount, granularity int) int {
	if amount%granularity == 0 {
		return amount / granularity
	}
	return amount/granularity + 1
}

func (r *CUResourceImpl) reserveResources(
	wg *kernels.WorkGroup,
	locations []WfLocation,
) {
	for _, location := range locations {
		r.wfPoolFreeCount[location.SIMDID]--
	}

	for _, t := range r.tempReserves {
		t.mask.setStatus(t.offset, t.length, allocStatusReserved)
	}
	r.tempReserves = r.tempReserves[:0]

	r.neverReserveTwice(wg)
	r.reservedWGs[wg] = locations
}

func (r *CUResourceImpl) neverReserveTwice(wg *kernels.WorkGroup) {
	if _, found := r.reservedWGs[wg]; found {
		panic("reserving a work-group twice")
	}
}

func (r *CUResourceImpl) clearTempReservation(wg *kernels.WorkGroup) {
	for _, t := range r.tempReserves {
		t.mask.setStatus(t.offset, t.length, allocStatusFree)
	}
	r.tempReserves = r.tempReserves[:0]
}

// FreeResourcesForWG marks all the resources used by a work-group available.
func (r *CUResourceImpl) FreeResourcesForWG(wg *kernels.WorkGroup) {
	locations, found := r.reservedWGs[wg]
	if !found {
		panic("work-group not found")
	}

	co := wg.CodeObject
	for _, location := range locations {
		r.wfPoolFreeCount[location.SIMDID]++

		ldsUnits := r.unitsOccupy(int(co.WGGroupSegmentByteSize),
			r.ldsGranularity)
		r.ldsMask.setStatus(location.LDSOffset/r.ldsGranularity, ldsUnits,
			allocStatusFree)

		sgprUnits := r.unitsOccupy(int(co.WFSgprCount), r.sregGranularity)
		r.sregMask.setStatus(location.SGPROffset/4/r.sregGranularity,
			sgprUnits, allocStatusFree)

		vgprUnits := r.unitsOccupy(int(co.WIVgprCount), r.vregGranularity)
		r.vregMasks[location.SIMDID].setStatus(
			location.VGPROffset/4/r.vregGranularity, vgprUnits,
			allocStatusFree)
	}

	delete(r.reservedWGs, wg)
}
