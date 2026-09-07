package chipnetwork

import (
	"fmt"

	"gitlab.com/akita/akita"
	// "gitlab.com/akita/mgpusim"
	"gitlab.com/akita/noc/networking/arbitration"
	"gitlab.com/akita/noc/networking/routing"
	"gitlab.com/akita/noc/networking/switching"
)

type InterChipletMeshConnector struct {
	InterChipletConnectorImpl

	portTable map[akita.Port]*routing.MeshRoutingTable // location of port

	// chiplet dimension & # of chiplets
	numChiplets  int
	gridSize     [2]int
	rdmaReqPorts map[uint64]akita.Port // Only contains RDMA Request port
	rdmaRspPorts map[uint64]akita.Port // Only contains RDMA Response port

	swReqPorts map[uint64]akita.Port
	swRspPorts map[uint64]akita.Port

	SwitchConnection []*switching.MultiFlitSwitchConnection
}

func NewInterChipletMeshConnector() *InterChipletMeshConnector {
	mc := &InterChipletMeshConnector{}
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

func (mc *InterChipletMeshConnector) WithFreq(freq akita.Freq) *InterChipletMeshConnector {
	mc.freq = freq
	return mc
}

func (mc *InterChipletMeshConnector) WithEngine(engine akita.Engine) *InterChipletMeshConnector {
	mc.engine = engine
	return mc
}

func (mc *InterChipletMeshConnector) WithSwitchLatency(numCycles int) *InterChipletMeshConnector {
	mc.switchLatency = numCycles
	return mc
}

func (mc *InterChipletMeshConnector) WithFlitByteSize(flitByteSize int) *InterChipletMeshConnector {
	mc.flitByteSize = flitByteSize
	return mc
}

func (mc *InterChipletMeshConnector) WithNumReqPerCycle(numReqPerCycle int) *InterChipletMeshConnector {
	mc.numReqPerCycle = numReqPerCycle
	return mc
}

// WithNetworkName sets the name of the network and the prefix of all the
// component in the network.
func (mc *InterChipletMeshConnector) WithNetworkName(networkName string) *InterChipletMeshConnector {
	mc.networkName = networkName
	return mc
}

func (mc *InterChipletMeshConnector) WithGridSize(xDim, yDim int) *InterChipletMeshConnector {
	mc.gridSize[0] = xDim
	mc.gridSize[1] = yDim
	return mc
}

func (mc *InterChipletMeshConnector) WithNumChiplets(n int) *InterChipletMeshConnector {
	mc.numChiplets = n
	return mc
}

func (mc *InterChipletMeshConnector) WithRDMAReqPorts(p map[uint64]akita.Port) *InterChipletMeshConnector {
	mc.rdmaReqPorts = p
	return mc
}

func (mc *InterChipletMeshConnector) WithRDMARspPorts(p map[uint64]akita.Port) *InterChipletMeshConnector {
	mc.rdmaRspPorts = p
	return mc
}

func (mc *InterChipletMeshConnector) CreateNetwork() {
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

func (mc *InterChipletMeshConnector) sortSwitchPorts() {
	for r := 0; r < mc.gridSize[1]; r++ {
		for c := 0; c < mc.gridSize[0]; c++ {
			sw := mc.chipSwitches[r][c]

			// response ports come first to prevent deadlock.
			sw.(*switching.MultiFlitChipletSwitchImpl).SortPorts()
		}
	}
}

func (mc *InterChipletMeshConnector) createPortTable() {
	mc.portTable = make(map[akita.Port]*routing.MeshRoutingTable, mc.numChiplets)
}

func (mc *InterChipletMeshConnector) createSwitchConnector() {
	mc.switchConnector = switching.NewChipSwitchConnector(mc.engine)
	mc.switchConnector.SetSwitchLatency(mc.switchLatency)
	mc.switchConnector.SetNumReqPerCycle(mc.numReqPerCycle)
	mc.switchConnector.SetBufferSize(mc.numReqPerCycle)
}

func (mc *InterChipletMeshConnector) createSwitches() {

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
				WithNumReqPerCycle(mc.numReqPerCycle). // this refers to bandwidth
				WithSwitchLatency(mc.switchLatency).
				WithMaxOutgoingReqsPerChiplet(12).
				WithGridSize(mc.gridSize[1], mc.gridSize[0]).
				WithFlitByteSize(mc.flitByteSize).
				WithSwitchPosition(y, x).
				Build(swName)

			mc.chipSwitches[y][x] = sw
		}
	}
}

func (mc *InterChipletMeshConnector) createLinks() {
	for y := 0; y < mc.gridSize[1]; y++ {
		for x := 0; x < mc.gridSize[0]; x++ {
			mc.connectWithDevice(y, x)
			mc.connectWithBottomSwitch(y, x)
			mc.connectWithRightSwitch(y, x)
		}
	}
}

func (mc *InterChipletMeshConnector) connectWithDevice(y, x int) {
	sw := mc.chipSwitches[y][x]
	chipletId := uint64(y*mc.gridSize[0] + x)
	rdmaReqPort := mc.rdmaReqPorts[chipletId]
	rdmaRspPort := mc.rdmaRspPorts[chipletId]

	swPortA := mc.switchConnector.ConnectDeviceToSwitch(rdmaReqPort, sw, "Req")
	swPortB := mc.switchConnector.ConnectDeviceToSwitch(rdmaRspPort, sw, "Rsp")

	mc.swReqPorts[chipletId] = swPortA
	mc.swRspPorts[chipletId] = swPortB

	rt := sw.GetRoutingTable().(*routing.MeshRoutingTable)

	// Local output points to the switch's own local port (which is connected
	// to the local EndPoint via a flit-only connection). The flit's final
	// destination remains the RDMA port — that is preserved on the flit's
	// inner Msg.Dst and the EndPoint will deliver the reassembled msg there.
	rt.SetLocal(swPortA, swPortB)

	mc.portTable[swPortA] = rt
	mc.portTable[swPortB] = rt
	mc.portTable[rdmaReqPort] = rt
	mc.portTable[rdmaRspPort] = rt
}

func (mc *InterChipletMeshConnector) connectWithBottomSwitch(y, x int) {
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

func (mc *InterChipletMeshConnector) connectWithRightSwitch(y, x int) {
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

func (mc *InterChipletMeshConnector) createMeshRoutingTable() {
	//make the global port map recording which switch has which port.
	for r := 0; r < mc.gridSize[1]; r++ {
		for c := 0; c < mc.gridSize[0]; c++ {
			sw := mc.chipSwitches[r][c]
			rt := sw.GetRoutingTable().(*routing.MeshRoutingTable)
			rt.SetPortTable(mc.portTable)
		}
	}
}

func (mc *InterChipletMeshConnector) GetSwReqPorts() map[uint64]akita.Port {
	return mc.swReqPorts
}

func (mc *InterChipletMeshConnector) GetSwRspPorts() map[uint64]akita.Port {
	return mc.swRspPorts
}
