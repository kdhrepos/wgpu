package chipnetwork

import (
	"fmt"

	"gitlab.com/akita/akita"
	// "gitlab.com/akita/mgpusim"
	"gitlab.com/akita/noc/networking/arbitration"
	"gitlab.com/akita/noc/networking/routing"
	"gitlab.com/akita/noc/networking/switching"
)

type InterChipletDynamicClusteringConnector struct {
	InterChipletMeshConnector

	portTable map[akita.Port]*routing.MeshRoutingTable // location of port

	// chiplet dimension & # of chiplets
	numChiplets int
	gridSize    [2]int

	swReqPorts map[uint64]akita.Port
	swRspPorts map[uint64]akita.Port

	rdmaReqPorts map[uint64]akita.Port // Only contains RDMA Request port
	rdmaRspPorts map[uint64]akita.Port // Only contains RDMA Response port
}

func NewInterChipletDynamicClusteringConnector() *InterChipletDynamicClusteringConnector {
	mc := &InterChipletDynamicClusteringConnector{}
	mc.freq = 1 * akita.GHz
	mc.encodingOverhead = 0
	mc.flitByteSize = 16
	mc.switchLatency = 1
	mc.numReqPerCycle = 1

	mc.swReqPorts = make(map[uint64]akita.Port)
	mc.swRspPorts = make(map[uint64]akita.Port)

	mc.gridSize = [2]int{0, 0}
	mc.numChiplets = 0

	return mc
}

func (mc *InterChipletDynamicClusteringConnector) WithFreq(freq akita.Freq) *InterChipletDynamicClusteringConnector {
	mc.freq = freq
	return mc
}

func (mc *InterChipletDynamicClusteringConnector) WithEngine(engine akita.Engine) *InterChipletDynamicClusteringConnector {
	mc.engine = engine
	return mc
}

func (mc *InterChipletDynamicClusteringConnector) WithSwitchLatency(numCycles int) *InterChipletDynamicClusteringConnector {
	mc.switchLatency = numCycles
	return mc
}

func (mc *InterChipletDynamicClusteringConnector) WithFlitByteSize(flitByteSize int) *InterChipletDynamicClusteringConnector {
	mc.flitByteSize = flitByteSize
	return mc
}

func (mc *InterChipletDynamicClusteringConnector) WithNumReqPerCycle(numReqPerCycle int) *InterChipletDynamicClusteringConnector {
	mc.numReqPerCycle = numReqPerCycle
	return mc
}

// WithNetworkName sets the name of the network and the prefix of all the
// component in the network.
func (mc *InterChipletDynamicClusteringConnector) WithNetworkName(networkName string) *InterChipletDynamicClusteringConnector {
	mc.networkName = networkName
	return mc
}

func (mc *InterChipletDynamicClusteringConnector) WithGridSize(xDim, yDim int) *InterChipletDynamicClusteringConnector {
	mc.gridSize[0] = xDim
	mc.gridSize[1] = yDim
	return mc
}

func (mc *InterChipletDynamicClusteringConnector) WithNumChiplets(n int) *InterChipletDynamicClusteringConnector {
	mc.numChiplets = n
	return mc
}

func (mc *InterChipletDynamicClusteringConnector) WithRDMAReqPorts(p map[uint64]akita.Port) *InterChipletDynamicClusteringConnector {
	mc.rdmaReqPorts = p
	return mc
}

func (mc *InterChipletDynamicClusteringConnector) WithRDMARspPorts(p map[uint64]akita.Port) *InterChipletDynamicClusteringConnector {
	mc.rdmaRspPorts = p
	return mc
}

// WithNoCTracer sets the tracer used to trace NoC-specific metrics, such as the
// traffics and congestions in the channels.
// func (mc *InterChipletDynamicClusteringConnector) WithNoCTracer(t tracing.Tracer) *Connector {
// 	mc.connector = mc.connector.WithNoCTracer(t)
// 	return c
// }

func (mc *InterChipletDynamicClusteringConnector) CreateNetwork() {
	mc.createPortTable()

	// switch connector to connect devices & switches
	mc.createSwitchConnector()

	// create switches for each chiplet and connect each other
	mc.createSwitches()
	mc.createLinks()

	// setup the mesh routing table for each of switch
	mc.createMeshRoutingTable()

	// sort ports as response ports come first to prevent deadlock.
	mc.sortSwitchPorts()

	mc.SwitchConnection = mc.switchConnector.GetSwitchConnection()
}

func (mc *InterChipletDynamicClusteringConnector) createPortTable() {
	mc.portTable = make(map[akita.Port]*routing.MeshRoutingTable, mc.numChiplets)
}

func (mc *InterChipletDynamicClusteringConnector) createSwitchConnector() {
	mc.switchConnector = switching.NewChipSwitchConnector(mc.engine)
	mc.switchConnector.SetSwitchLatency(mc.switchLatency)
	mc.switchConnector.SetNumReqPerCycle(mc.numReqPerCycle)
	mc.switchConnector.SetBufferSize(mc.numReqPerCycle)
}

func (mc *InterChipletDynamicClusteringConnector) sortSwitchPorts() {
	for r := 0; r < mc.gridSize[1]; r++ {
		for c := 0; c < mc.gridSize[0]; c++ {
			sw := mc.chipSwitches[r][c]

			// response ports come first to prevent deadlock.
			sw.(*switching.MultiFlitChipletSwitchImpl).SortPorts()
		}
	}
}

// createSwitches builds vanilla MultiFlitChipletSwitchImpl per chiplet.
// Branching of DistributeRsp is delegated to DynamicClusteringEndPoint at
// connectWithDevice; the switches themselves do only unicast wormhole
// routing.
func (mc *InterChipletDynamicClusteringConnector) createSwitches() {

	/* initializing switches */
	mc.chipSwitches = make([][]switching.MultiFlitChipletSwitch, mc.gridSize[1])

	for i := range mc.chipSwitches {
		mc.chipSwitches[i] = make([]switching.MultiFlitChipletSwitch, mc.gridSize[0])
	}

	for y := 0; y < mc.gridSize[1]; y++ {
		for x := 0; x < mc.gridSize[0]; x++ {
			swName := fmt.Sprintf("%s.SW_[%d][%d]", mc.networkName, y, x)

			vctArbiter := arbitration.NewFlitAwareXBarArbiter(mc.numReqPerCycle)
			vctArbiter.SetVCT(true) // VCT + XY = provably deadlock-free
			sw := switching.MakeMultiFlitChipletSwitchImplBuilder().
				WithEngine(mc.engine).
				WithFreq(mc.freq).
				WithArbiter(vctArbiter).
				WithRoutingTable(routing.NewMeshRoutingTable(y, x)).
				WithNumReqPerCycle(mc.numReqPerCycle).
				WithSwitchLatency(mc.switchLatency).
				WithMaxOutgoingReqsPerChiplet(12).
				WithGridSize(mc.gridSize[1], mc.gridSize[0]).
				WithSwitchPosition(y, x).
				WithFlitByteSize(mc.flitByteSize).
				Build(swName)

			mc.chipSwitches[y][x] = sw
		}
	}
}

func (mc *InterChipletDynamicClusteringConnector) createLinks() {
	for y := 0; y < mc.gridSize[1]; y++ {
		for x := 0; x < mc.gridSize[0]; x++ {
			mc.connectWithDevice(y, x)
			mc.connectWithBottomSwitch(y, x)
			mc.connectWithRightSwitch(y, x)
		}
	}
}

func (mc *InterChipletDynamicClusteringConnector) connectWithDevice(y, x int) {
	sw := mc.chipSwitches[y][x]
	chipletId := uint64(y*mc.gridSize[0] + x)
	rdmaReqPort := mc.rdmaReqPorts[chipletId]
	rdmaRspPort := mc.rdmaRspPorts[chipletId]

	// Snapshot endpoint count so we know which endpoints belong to this
	// chiplet — ConnectDeviceToSwitch appends new EndPoints to the
	// connector's internal list.
	epsBefore := len(mc.switchConnector.GetEndPoints())

	swPortA := mc.switchConnector.ConnectDeviceToSwitch(rdmaReqPort, sw, "Req")
	swPortB := mc.switchConnector.ConnectDeviceToSwitch(rdmaRspPort, sw, "Rsp")

	// Wrap the two new endpoints (Req first, Rsp second — the
	// ConnectDeviceToSwitch order above) with the fan-out deliver hook.
	// DistributeRsp now travels the (idle) Req network, so the branch sees
	// it on the Req endpoint; point its fan-out at the Rsp endpoint so the
	// resulting DataReadyRsp leave on the Rsp network. This splits the relay
	// input (Req VC) from its output (Rsp VC), breaking the single-VC
	// response->response cycle.
	eps := mc.switchConnector.GetEndPoints()
	reqDep := switching.NewDynamicClusteringEndPoint(eps[epsBefore], int(chipletId))
	switching.NewDynamicClusteringEndPoint(eps[epsBefore+1], int(chipletId))
	reqDep.SetFanoutTarget(eps[epsBefore+1])

	mc.swReqPorts[chipletId] = swPortA
	mc.swRspPorts[chipletId] = swPortB

	rt := sw.GetRoutingTable().(*routing.MeshRoutingTable)

	// Route local-destination flits to the switch's own local Req/Rsp
	// port (which is connected to the local endpoint via a flit link).
	rt.SetLocal(swPortA, swPortB)

	mc.portTable[swPortA] = rt
	mc.portTable[swPortB] = rt
	mc.portTable[rdmaReqPort] = rt
	mc.portTable[rdmaRspPort] = rt
}

func (mc *InterChipletDynamicClusteringConnector) connectWithBottomSwitch(y, x int) {
	// node at the bottom edge
	if y == mc.gridSize[1]-1 {
		return
	}

	swA := mc.chipSwitches[y][x]
	swB := mc.chipSwitches[y+1][x]

	swAReq, swBReq := mc.switchConnector.ConnectSwitches(swA, swB, mc.freq, "Req")
	swARsp, swBRsp := mc.switchConnector.ConnectSwitches(swA, swB, mc.freq, "Rsp")

	rtA := swA.GetRoutingTable().(*routing.MeshRoutingTable)
	rtB := swB.GetRoutingTable().(*routing.MeshRoutingTable)

	rtA.SetBottom(swBReq, swBRsp)
	rtB.SetTop(swAReq, swARsp)

	mc.portTable[swAReq] = rtA
	mc.portTable[swARsp] = rtA
	mc.portTable[swBReq] = rtB
	mc.portTable[swBRsp] = rtB
}

func (mc *InterChipletDynamicClusteringConnector) connectWithRightSwitch(y, x int) {
	// node at the right edge
	if x == mc.gridSize[0]-1 {
		return
	}

	swA := mc.chipSwitches[y][x]
	swB := mc.chipSwitches[y][x+1]

	swAReq, swBReq := mc.switchConnector.ConnectSwitches(swA, swB, mc.freq, "Req")
	swARsp, swBRsp := mc.switchConnector.ConnectSwitches(swA, swB, mc.freq, "Rsp")

	rtA := swA.GetRoutingTable().(*routing.MeshRoutingTable)
	rtB := swB.GetRoutingTable().(*routing.MeshRoutingTable)

	rtA.SetRight(swBReq, swBRsp)
	rtB.SetLeft(swAReq, swARsp)

	mc.portTable[swAReq] = rtA
	mc.portTable[swARsp] = rtA
	mc.portTable[swBReq] = rtB
	mc.portTable[swBRsp] = rtB
}

func (mc *InterChipletDynamicClusteringConnector) createMeshRoutingTable() {
	//make the global port map recording which switch has which port.
	for r := 0; r < mc.gridSize[1]; r++ {
		for c := 0; c < mc.gridSize[0]; c++ {
			sw := mc.chipSwitches[r][c]
			rt := sw.GetRoutingTable().(*routing.MeshRoutingTable)
			rt.SetPortTable(mc.portTable)
		}
	}
}

func (mc *InterChipletDynamicClusteringConnector) GetSwReqPorts() map[uint64]akita.Port {
	return mc.swReqPorts
}

func (mc *InterChipletDynamicClusteringConnector) GetSwRspPorts() map[uint64]akita.Port {
	return mc.swRspPorts
}
