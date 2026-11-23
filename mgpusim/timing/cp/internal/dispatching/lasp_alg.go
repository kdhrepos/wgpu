package dispatching

import (
	"fmt"
	"strconv"
	"strings"

	"gitlab.com/akita/mgpusim/coreprotocol"
	"gitlab.com/akita/mgpusim/kernels"
	"gitlab.com/akita/mgpusim/timing/cp/internal/resource"
	"gitlab.com/akita/util/global"
	"gitlab.com/akita/util/tracing"
)

type chipletLASPAlgorithm struct {
	dispatcher  *DispatcherImpl
	gridBuilder kernels.GridBuilder
	cuPool      resource.CUResourcePool
	numChiplets uint64

	currWG           *kernels.WorkGroup
	nextCU           []int
	nextChipletID    int
	numDispatchedWGs int

	perChipletListOfWG [][]*kernels.WorkGroup

	partition string

	kernelDispatched int
}

func (a *chipletLASPAlgorithm) RegisterCU(cu resource.DispatchableCU) {
	a.cuPool.RegisterCU(cu)
}

func (a *chipletLASPAlgorithm) StartNewKernel(info kernels.KernelLaunchInfo) {
	a.numDispatchedWGs = 0
	a.gridBuilder.SetKernel(info)

	a.nextCU = make([]int, a.numChiplets)

	a.perChipletListOfWG = make([][]*kernels.WorkGroup, a.numChiplets)
	for i := 0; i < int(a.numChiplets); i++ {
		a.perChipletListOfWG[i] = make([]*kernels.WorkGroup, 0)
		a.nextCU[i] = 0
	}

	wgPartition := global.GetInstance().GetWgPartition(a.kernelDispatched)
	isXdiv := strings.Contains(wgPartition, "Xdiv")
	isYdiv := strings.Contains(wgPartition, "Ydiv")

	N := 1
	if isXdiv {
		N = a.gridBuilder.NumWGX()
	} else if isYdiv {
		N = a.gridBuilder.NumWGY()
	}

	P := int(a.numChiplets)
	base := N / P
	remained := N % P
	cutoff := remained * (base + 1)

	// This is a do-while loop
	a.currWG = a.gridBuilder.NextWG()

	workgroupsPerChiplet := make([]uint64, a.numChiplets)
	workgroupsPerKernel := uint64(a.gridBuilder.NumWG())

	a.TraceWorkGroupDimension()

	if global.GetInstance().GetVerbose() {
		fmt.Println("Total workgroups",
			a.currWG.Packet.GridSizeX,
			a.currWG.Packet.GridSizeY,
			a.currWG.Packet.WorkgroupSizeX,
			a.currWG.Packet.WorkgroupSizeY,
			a.gridBuilder.NumWGX(),
			a.gridBuilder.NumWGY(),
			a.numChiplets)
	}

	for a.currWG != nil {
		id := 0
		if isXdiv {
			id = a.currWG.IDX
		} else if isYdiv {
			id = a.currWG.IDY
		}

		chiplet := 0
		if P > 0 {
			if id < cutoff {
				chiplet = id / (base + 1)
			} else if base > 0 {
				chiplet = remained + (id-cutoff)/base
			} else {
				chiplet = id
			}
		}

		workgroupsPerChiplet[chiplet]++
		a.perChipletListOfWG[chiplet] = append(a.perChipletListOfWG[chiplet], a.currWG)
		if global.GetInstance().GetVerbose() {
			fmt.Println("Workgroup distribution", a.currWG.IDX, a.currWG.IDY, chiplet)
		}
		a.currWG = a.gridBuilder.NextWG()
	}

	for id, n := range workgroupsPerChiplet {
		tracing.StartTask(
			0,
			0,
			0,
			a.dispatcher,
			"workgroup-per-chiplet",
			strconv.Itoa(id),
			n)

		if global.GetInstance().GetVerbose() {
			fmt.Printf("Chiplet %d: %d\n", id, n)
		}
	}

	tracing.StartTask(
		0,
		0,
		0,
		a.dispatcher,
		"workgroup-per-kernel",
		strconv.Itoa(a.kernelDispatched),
		workgroupsPerKernel)

	a.kernelDispatched++
}

// NumWG returns the number of work-groups in the currently-dispatching
// work-group.
func (a *chipletLASPAlgorithm) NumWG() int {
	return a.gridBuilder.NumWG()
}

// HasNext check if there are more work-groups to dispatch.
func (a *chipletLASPAlgorithm) HasNext() bool {
	return a.numDispatchedWGs < a.gridBuilder.NumWG()
}

// Next finds the location to dispatch the next work-group.
// TODO: WORKS ONLY FOR ONE APPLICATION AS OF NOW!!!
func (a *chipletLASPAlgorithm) Next() (location dispatchLocation) {

	cusPerChiplet := a.cuPool.NumCU() / int(a.numChiplets)

	startingChiplet := a.nextChipletID
	for c := 0; c < int(a.numChiplets); c++ {
		chiplet := (startingChiplet + c) % int(a.numChiplets)

		if len(a.perChipletListOfWG[chiplet]) != 0 {
			a.currWG = a.perChipletListOfWG[chiplet][0]
		} else {
			continue
		}

		if a.currWG == nil {
			continue
		}

		for i := 0; i < cusPerChiplet; i++ {
			cuID := chiplet*cusPerChiplet + ((a.nextCU[chiplet] + i) % cusPerChiplet)
			cu := a.cuPool.GetCU(cuID)

			locations, ok := cu.ReserveResourceForWG(a.currWG)
			if ok {
				a.nextCU[chiplet] = (cuID + 1) % cusPerChiplet
				a.nextChipletID = (chiplet + 1) % int(a.numChiplets)
				dispatch := dispatchLocation{
					valid: true,
					cu:    cu.DispatchingPort(),
					cuID:  cuID,
					wg:    a.currWG,
				}
				dispatch.locations =
					make([]coreprotocol.WfDispatchLocation, len(locations))
				for i, localtion := range locations {
					dispatch.locations[i] = coreprotocol.WfDispatchLocation(localtion)
				}

				a.currWG = nil
				a.perChipletListOfWG[chiplet] = a.perChipletListOfWG[chiplet][1:]
				a.numDispatchedWGs++

				return dispatch
			} else {
				//do nothing :)
			}
		}
	}
	return dispatchLocation{}
}

// FreeResources marks the dispatched location to be available.
func (a *chipletLASPAlgorithm) FreeResources(location dispatchLocation) {
	a.cuPool.GetCU(location.cuID).FreeResourcesForWG(location.wg)
}

func (a *chipletLASPAlgorithm) TraceWorkGroupDimension() {
	tracing.StartTask(
		uint64(a.kernelDispatched),
		0,
		0,
		a.dispatcher,
		"workgroup-dimension",
		"workgroup-dimension-grid-size-x",
		int(a.currWG.Packet.GridSizeX))
	tracing.StartTask(
		uint64(a.kernelDispatched),
		0,
		0,
		a.dispatcher,
		"workgroup-dimension",
		"workgroup-dimension-grid-size-y",
		int(a.currWG.Packet.GridSizeY))
	tracing.StartTask(
		uint64(a.kernelDispatched),
		0,
		0,
		a.dispatcher,
		"workgroup-dimension",
		"workgroup-dimension-grid-size-z",
		int(a.currWG.Packet.GridSizeZ))
	tracing.StartTask(
		uint64(a.kernelDispatched),
		0,
		0,
		a.dispatcher,
		"workgroup-dimension",
		"workgroup-size-x",
		int(a.currWG.Packet.WorkgroupSizeX))
	tracing.StartTask(
		uint64(a.kernelDispatched),
		0,
		0,
		a.dispatcher,
		"workgroup-dimension",
		"workgroup-size-y",
		int(a.currWG.Packet.WorkgroupSizeY))
	tracing.StartTask(
		uint64(a.kernelDispatched),
		0,
		0,
		a.dispatcher,
		"workgroup-dimension",
		"workgroup-size-z",
		int(a.currWG.Packet.WorkgroupSizeZ))
	tracing.StartTask(
		uint64(a.kernelDispatched),
		0,
		0,
		a.dispatcher,
		"workgroup-dimension",
		"grid-dimension-x",
		int(a.currWG.Packet.GridSizeX-1)/int(a.currWG.Packet.WorkgroupSizeX)+1)
	tracing.StartTask(
		uint64(a.kernelDispatched),
		0,
		0,
		a.dispatcher,
		"workgroup-dimension",
		"grid-dimension-y",
		int(a.currWG.Packet.GridSizeY-1)/int(a.currWG.Packet.WorkgroupSizeY)+1)
	tracing.StartTask(
		uint64(a.kernelDispatched),
		0,
		0,
		a.dispatcher,
		"workgroup-dimension",
		"grid-dimension-z",
		int(a.currWG.Packet.GridSizeZ-1)/int(a.currWG.Packet.WorkgroupSizeZ)+1)
}
