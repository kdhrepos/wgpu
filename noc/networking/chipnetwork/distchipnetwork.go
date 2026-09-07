package chipnetwork

import (
	"fmt"

	"gitlab.com/akita/akita"
	// "gitlab.com/akita/mgpusim"
	"gitlab.com/akita/noc/networking/arbitration"
	"gitlab.com/akita/noc/networking/routing"
	"gitlab.com/akita/noc/networking/switching"
)

type InterChipletDistConnector struct {
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

func NewInterChipletDistConnector() *InterChipletDistConnector {
	mc := &InterChipletDistConnector{}
	mc.freq = 1 * akita.GHz
	mc.encodingOverhead = 0
	mc.flitByteSize = 64
	mc.switchLatency = 1
	mc.numReqPerCycle = 1

	mc.swReqPorts = make(map[uint64]akita.Port)
	mc.swRspPorts = make(map[uint64]akita.Port)

	mc.gridSize = [2]int{0, 0}
	mc.numChiplets = 0

	return mc
}

func (mc *InterChipletDistConnector) WithFreq(freq akita.Freq) *InterChipletDistConnector {
	mc.freq = freq
	return mc
}

func (mc *InterChipletDistConnector) WithEngine(engine akita.Engine) *InterChipletDistConnector {
	mc.engine = engine
	return mc
}

func (mc *InterChipletDistConnector) WithSwitchLatency(numCycles int) *InterChipletDistConnector {
	mc.switchLatency = numCycles
	return mc
}

func (mc *InterChipletDistConnector) WithFlitByteSize(flitByteSize int) *InterChipletDistConnector {
	mc.flitByteSize = flitByteSize
	return mc
}

func (mc *InterChipletDistConnector) WithNumReqPerCycle(numReqPerCycle int) *InterChipletDistConnector {
	mc.numReqPerCycle = numReqPerCycle
	return mc
}

// WithNetworkName sets the name of the network and the prefix of all the
// component in the network.
func (mc *InterChipletDistConnector) WithNetworkName(networkName string) *InterChipletDistConnector {
	mc.networkName = networkName
	return mc
}

func (mc *InterChipletDistConnector) WithGridSize(xDim, yDim int) *InterChipletDistConnector {
	mc.gridSize[0] = xDim
	mc.gridSize[1] = yDim
	return mc
}

func (mc *InterChipletDistConnector) WithNumChiplets(n int) *InterChipletDistConnector {
	mc.numChiplets = n
	return mc
}

func (mc *InterChipletDistConnector) WithRDMAReqPorts(p map[uint64]akita.Port) *InterChipletDistConnector {
	mc.rdmaReqPorts = p
	return mc
}

func (mc *InterChipletDistConnector) WithRDMARspPorts(p map[uint64]akita.Port) *InterChipletDistConnector {
	mc.rdmaRspPorts = p
	return mc
}

// WithNoCTracer sets the tracer used to trace NoC-specific metrics, such as the
// traffics and congestions in the channels.
// func (mc *InterChipletDistConnector) WithNoCTracer(t tracing.Tracer) *Connector {
// 	mc.connector = mc.connector.WithNoCTracer(t)
// 	return c
// }

func (mc *InterChipletDistConnector) CreateNetwork() {
	mc.createPortTable()

	// switch connector to connect devices & switches
	mc.createSwitchConnector()

	// create switches for each chiplet and connect each other
	mc.createSwitches()
	mc.createLinks()

	// setup the mesh routing table for each of switch
	mc.createMeshRoutingTable()

	mc.SwitchConnection = mc.switchConnector.GetSwitchConnection()
}

func (mc *InterChipletDistConnector) createPortTable() {
	mc.portTable = make(map[akita.Port]*routing.MeshRoutingTable, mc.numChiplets)
}

func (mc *InterChipletDistConnector) createSwitchConnector() {
	mc.switchConnector = switching.NewChipSwitchConnector(mc.engine)
	mc.switchConnector.SetSwitchLatency(mc.switchLatency)
	mc.switchConnector.SetNumReqPerCycle(mc.numReqPerCycle)
	mc.switchConnector.SetBufferSize(mc.numReqPerCycle)
}

func (mc *InterChipletDistConnector) createSwitches() {

	/* initializing switches */
	mc.chipSwitches = make([][]switching.MultiFlitChipletSwitch, mc.gridSize[1])

	for i := range mc.chipSwitches {
		mc.chipSwitches[i] = make([]switching.MultiFlitChipletSwitch, mc.gridSize[0])
	}

	for y := 0; y < mc.gridSize[1]; y++ {
		for x := 0; x < mc.gridSize[0]; x++ {
			swName := fmt.Sprintf("%s.SW_[%d][%d]", mc.networkName, y, x)

			sw := switching.MakeDistMultiFlitChipletSwitchBuilder().
				WithEngine(mc.engine).
				WithFreq(mc.freq).
				WithArbiter(arbitration.NewFlitAwareXBarArbiter(mc.numReqPerCycle)).
				WithRoutingTable(routing.NewMeshRoutingTable(y, x)).
				WithNumReqPerCycle(mc.numReqPerCycle). // this refers to bandwidth
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

func (mc *InterChipletDistConnector) createLinks() {
	for y := 0; y < mc.gridSize[1]; y++ {
		for x := 0; x < mc.gridSize[0]; x++ {
			mc.connectWithDevice(y, x)
			mc.connectWithBottomSwitch(y, x)
			mc.connectWithRightSwitch(y, x)
		}
	}
}

func (mc *InterChipletDistConnector) connectWithDevice(y, x int) {
	sw := mc.chipSwitches[y][x]
	chipletId := uint64(y*mc.gridSize[0] + x)
	rdmaReqPort := mc.rdmaReqPorts[chipletId]
	rdmaRspPort := mc.rdmaRspPorts[chipletId]

	swPortA := mc.switchConnector.ConnectDeviceToSwitch(rdmaReqPort, sw, "Req")
	swPortB := mc.switchConnector.ConnectDeviceToSwitch(rdmaRspPort, sw, "Rsp")

	mc.swReqPorts[chipletId] = swPortA
	mc.swRspPorts[chipletId] = swPortB

	rt := sw.GetRoutingTable().(*routing.MeshRoutingTable)

	// After the wormhole refactor the switch port (not the RDMA port) is
	// what's registered in portToComplexMapping. Route local-destination
	// flits to the switch's own local Req/Rsp port; the endpoint behind
	// it reassembles and delivers to the RDMA port.
	rt.SetLocal(swPortA, swPortB)

	mc.portTable[swPortA] = rt
	mc.portTable[swPortB] = rt
	mc.portTable[rdmaReqPort] = rt
	mc.portTable[rdmaRspPort] = rt
}

func (mc *InterChipletDistConnector) connectWithBottomSwitch(y, x int) {
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

func (mc *InterChipletDistConnector) connectWithRightSwitch(y, x int) {
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

func (mc *InterChipletDistConnector) createMeshRoutingTable() {
	//make the global port map recording which switch has which port.
	for r := 0; r < mc.gridSize[1]; r++ {
		for c := 0; c < mc.gridSize[0]; c++ {
			sw := mc.chipSwitches[r][c]
			rt := sw.GetRoutingTable().(*routing.MeshRoutingTable)
			rt.SetPortTable(mc.portTable)
		}
	}
}

func (mc *InterChipletDistConnector) GetSwReqPorts() map[uint64]akita.Port {
	return mc.swReqPorts
}

func (mc *InterChipletDistConnector) GetSwRspPorts() map[uint64]akita.Port {
	return mc.swRspPorts
}
