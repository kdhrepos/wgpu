// package device provides support for the driver implementation.
package device

import (
	"math"

	"fmt"

	"gitlab.com/akita/util/ca"
	"gitlab.com/akita/util/global"
)

// A MemoryAllocator can allocate memory on the CPU and GPUs
type MemoryAllocator interface {
	RegisterDevice(device *Device)
	GetDeviceIDByPAddr(pAddr uint64) int
	AllocateVirtualChunk(pid ca.PID, byteSize uint64, deviceID int) uint64
	allocatePageTablePage(pid ca.PID, deviceID int, vAddr, pAddr uint64) uint64
	AllocateUnified(pid ca.PID, byteSize uint64) uint64
	AllocateVirtualChunkLASP(pid ca.PID, byteSize uint64, deviceID int, partition string) uint64
	Free(pid ca.PID, vAddr uint64)
	Remap(pid ca.PID, pageVAddr, byteSize uint64, deviceID int)
	RemovePage(pid ca.PID, vAddr uint64)
	AllocatePageWithGivenVAddr(
		pid ca.PID,
		deviceID int,
		vAddr uint64,
		unified bool,
	) Page
	GetPageAllocatedPerChiplet() []int
}

// NewMemoryAllocator creates a new memory allocator.
func NewMemoryAllocator(
	pageTable PageTable,
	log2PageSize uint64,
	numChiplets uint64,
	dataPlacementPartition string,
) MemoryAllocator {
	a := &memoryAllocatorImpl{
		pageTable:               pageTable,
		totalStorageByteSize:    1 << log2PageSize, // Starting with a page to avoid 0 address.
		log2PageSize:            log2PageSize,
		processMemoryStates:     make(map[ca.PID]*processMemoryState),
		devices:                 make(map[int]*Device),
		numChiplets:             numChiplets,
		dataPlacementPartition:  dataPlacementPartition,
		PageAllocatedPerChiplet: make([]int, numChiplets),
	}
	return a
}

type processMemoryState struct {
	pid       ca.PID
	nextVAddr uint64
}

// A memoryAllocatorImpl provides the default implementation for
// memoryAllocator
type memoryAllocatorImpl struct {
	pageTable              PageTable
	log2PageSize           uint64
	processMemoryStates    map[ca.PID]*processMemoryState
	devices                map[int]*Device
	totalStorageByteSize   uint64
	numChiplets            uint64
	dataPlacementPartition string
	dataPlacementStride    int

	PageAllocatedPerChiplet []int

	dataAllocated int
}

func (a *memoryAllocatorImpl) GetPageAllocatedPerChiplet() []int {
	return a.PageAllocatedPerChiplet
}

func (a *memoryAllocatorImpl) RegisterDevice(device *Device) {

	state := device.MemState
	state.setNumChiplets(a.numChiplets)
	state.setInitialAddress(a.totalStorageByteSize)

	a.totalStorageByteSize += state.getStorageSize()

	a.devices[device.ID] = device
}

func (a *memoryAllocatorImpl) GetDeviceIDByPAddr(pAddr uint64) int {

	return a.deviceIDByPAddr(pAddr)
}

func (a *memoryAllocatorImpl) deviceIDByPAddr(pAddr uint64) int {
	for id, dev := range a.devices {
		state := dev.MemState
		if isPAddrOnDevice(pAddr, state) {
			return id
		}
	}

	panic("device not found")
}

func isPAddrOnDevice(
	pAddr uint64,
	state DeviceMemoryState,
) bool {
	return pAddr >= state.getInitialAddress() &&
		pAddr < state.getInitialAddress()+state.getStorageSize()
}

func (a *memoryAllocatorImpl) AllocateVirtualChunk(
	pid ca.PID,
	byteSize uint64,
	deviceID int,
) uint64 {
	pageSize := uint64(1 << a.log2PageSize)
	numPages := (byteSize-1)/pageSize + 1
	return a.allocatePages(int(numPages), pid, deviceID, false)
}

func (a *memoryAllocatorImpl) AllocateVirtualChunkLASP(
	pid ca.PID,
	byteSize uint64,
	deviceID int,
	partition string,
) uint64 {
	pageSize := uint64(1 << a.log2PageSize)
	numPages := (byteSize-1)/pageSize + 1
	return a.allocatePagesLASP(
		int(numPages),
		pid,
		deviceID,
		false)
}

// I only use this function for allocating page table pages
func (a *memoryAllocatorImpl) allocatePageTablePage(
	pid ca.PID,
	deviceID int,
	vAddr, pAddr uint64,
) uint64 {
	// no locks here to prevent deadlocks
	device := a.devices[deviceID]
	return device.allocatePageTablePage(vAddr, pAddr)
}

func (a *memoryAllocatorImpl) AllocateUnified(
	pid ca.PID,
	byteSize uint64,
) uint64 {

	pageSize := uint64(1 << a.log2PageSize)
	numPages := (byteSize-1)/pageSize + 1
	return a.allocatePages(int(numPages), pid, 1, true)
}

func (a *memoryAllocatorImpl) allocatePages(
	numPages int, pid ca.PID, deviceID int, unified bool,
) (firstPageVAddr uint64) {
	fmt.Println("num pages", numPages)
	pState, found := a.processMemoryStates[pid]
	if !found {
		a.processMemoryStates[pid] = &processMemoryState{
			pid:       pid,
			nextVAddr: uint64(1 << a.log2PageSize),
		}
		pState = a.processMemoryStates[pid]
	}
	device := a.devices[deviceID]

	pageSize := uint64(1 << a.log2PageSize)
	nextVAddr := pState.nextVAddr
	pAddrs := device.allocateMultiplePages(numPages)
	for i := 0; i < numPages; i++ {
		pAddr := pAddrs[i]
		vAddr := nextVAddr + uint64(i)*pageSize
		page := Page{
			PID:      pid,
			VAddr:    vAddr,
			PAddr:    pAddr,
			PageSize: pageSize,
			Valid:    true,
			Unified:  unified,
			DeviceID: uint64(a.deviceIDByPAddr(pAddr)),
		}
		if page.DeviceID != uint64(deviceID) {
			panic("gpuid != deviceid")
		}
		fmt.Println("data page:",
			fmt.Sprintf("%x", vAddr),
			fmt.Sprintf("%x", pAddr))

		a.pageTable.Insert(page)
	}

	pState.nextVAddr += pageSize * uint64(numPages)
	// fmt.Println("#########", nextVAddr, numPages)
	return nextVAddr
}

func (a *memoryAllocatorImpl) findChipletBasedOnPartition(
	pageNum uint64,
	numPages int,
) int {
	numChiplets := int(a.numChiplets)
	pageSize := uint64(1 << a.log2PageSize)
	pageParts := int(math.Ceil(float64(numPages) / float64(numChiplets)))

	dataPartitioning := global.GetInstance().GetDataPartitioning(a.dataAllocated)

	switch dataPartitioning {
	case "col-based":
		dataStride := global.GetInstance().GetDataStride(a.dataAllocated)

		if dataStride <= 0 {
			panic("Stride must be positive for col-based partition")
		}

		interleavingGranularity := int(math.Ceil((float64(dataStride) / float64(numChiplets)) / float64(pageSize)))
		return (int(pageNum) / interleavingGranularity) % numChiplets
	default:
		// use "numPages", not actual vAddr since page aligning is required.
		chiplet := int(pageNum / uint64(pageParts))
		return chiplet
	}
}

func (a *memoryAllocatorImpl) Remap(
	pid ca.PID,
	pageVAddr, byteSize uint64,
	deviceID int,
) {

	pageSize := uint64(1 << a.log2PageSize)
	addr := pageVAddr
	vAddrs := make([]uint64, 0)
	for addr < pageVAddr+byteSize {
		vAddrs = append(vAddrs, addr)
		addr += pageSize
	}

	a.allocateMultiplePagesWithGivenVAddrs(pid, deviceID, vAddrs, false)
}

func (a *memoryAllocatorImpl) RemovePage(pid ca.PID, vAddr uint64) {

	a.removePage(pid, vAddr)
}

func (a *memoryAllocatorImpl) removePage(pid ca.PID, vAddr uint64) {
	page, ok := a.pageTable.Find(pid, vAddr)

	if !ok {
		panic("page not found")
	}

	deviceID := a.deviceIDByPAddr(page.PAddr)
	dState := a.devices[deviceID].MemState
	dState.addSinglePAddr(page.PAddr)

	a.pageTable.Remove(page.PID, page.VAddr)
}

func (a *memoryAllocatorImpl) AllocatePageWithGivenVAddr(
	pid ca.PID,
	deviceID int,
	vAddr uint64,
	isUnified bool,
) Page {

	return a.allocatePageWithGivenVAddr(pid, deviceID, vAddr, isUnified)
}

func (a *memoryAllocatorImpl) allocatePageWithGivenVAddr(
	pid ca.PID,
	deviceID int,
	vAddr uint64,
	isUnified bool,
) Page {
	pageSize := uint64(1 << a.log2PageSize)

	device := a.devices[deviceID]
	pAddr := device.allocatePage()

	page := Page{
		PID:      pid,
		VAddr:    vAddr,
		PAddr:    pAddr,
		PageSize: pageSize,
		Valid:    true,
		DeviceID: uint64(deviceID),
		Unified:  isUnified,
	}
	a.pageTable.Update(page)

	return page
}

func (a *memoryAllocatorImpl) allocateMultiplePagesWithGivenVAddrs(
	pid ca.PID,
	deviceID int,
	vAddrs []uint64,
	isUnified bool,
) (pages []Page) {
	pageSize := uint64(1 << a.log2PageSize)

	device := a.devices[deviceID]
	pAddrs := device.allocateMultiplePages(len(vAddrs))

	for i, vAddr := range vAddrs {
		page := Page{
			PID:      pid,
			VAddr:    vAddr,
			PAddr:    pAddrs[i],
			PageSize: pageSize,
			Valid:    true,
			DeviceID: uint64(deviceID),
			Unified:  isUnified,
		}
		a.pageTable.Update(page)
		pages = append(pages, page)
	}

	return pages
}

func (a *memoryAllocatorImpl) Free(pid ca.PID, ptr uint64) {

	a.removePage(pid, ptr)
}

func (a *memoryAllocatorImpl) allocatePagesLASP(
	numPages int,
	pid ca.PID,
	deviceID int,
	unified bool,
) (firstPageVAddr uint64) {
	fmt.Println("num pages", numPages)
	pState, found := a.processMemoryStates[pid]
	if !found {
		a.processMemoryStates[pid] = &processMemoryState{
			pid: pid,
			// nextVAddr: uint64(1 << a.log2PageSize),
			nextVAddr: 0,
		}
		pState = a.processMemoryStates[pid]
	}
	device := a.devices[deviceID]

	pageSize := uint64(1 << a.log2PageSize)
	nextVAddr := pState.nextVAddr

	fmt.Println("Data Allocation: ",
		a.dataAllocated,
		global.GetInstance().GetDataPartitioning(a.dataAllocated),
		global.GetInstance().GetDataStride(a.dataAllocated))

	for i := range numPages {
		vAddr := nextVAddr + uint64(i)*pageSize

		// here, function below selects which chiplet the data page resides.
		chiplet := a.findChipletBasedOnPartition(uint64(i), numPages)

		a.PageAllocatedPerChiplet[chiplet]++

		// This pAddr refers physical address where data page stays at.
		pAddr := device.allocatePageOnChiplet(chiplet)

		global.GetInstance().SetDataAllocatedLocation(
			a.dataAllocated,
			chiplet,
			pAddr)

		page := Page{
			PID:      pid,
			VAddr:    vAddr,
			PAddr:    pAddr,
			PageSize: pageSize,
			Valid:    true,
			Unified:  unified,
			DeviceID: uint64(a.deviceIDByPAddr(pAddr)),
		}

		if page.DeviceID != uint64(deviceID) {
			panic("gpuid != deviceid")
		}

		fmt.Printf("Page Num: %d, Chiplet: %d\n", i, chiplet)

		a.pageTable.Insert(page)
	}

	pState.nextVAddr += pageSize * uint64(numPages)

	for c := range a.numChiplets {
		fmt.Printf("Chiplet %d: %d pages allocated\n", c, a.PageAllocatedPerChiplet[c])
	}

	// to specify lasp stride for each of data structures.
	a.dataAllocated++

	return nextVAddr
}
