package chipnetwork

import (
	"fmt"

	"gitlab.com/akita/akita"
	"gitlab.com/akita/noc/networking/arbitration"
	"gitlab.com/akita/noc/networking/routing"
	"gitlab.com/akita/noc/networking/switching"
	// "gitlab.com/akita/mgpusim"
)

type InterChipletIdealConnector struct {
	InterChipletConnectorImpl

	// routingTableFinder map[akita.Port]*routing.IdealRoutingTable
	portTable []map[akita.Port]akita.Port

	// chiplet dimension & # of chiplets
	numChiplets  int
	gridSize     [2]int
	rdmaReqPorts map[uint64]akita.Port // Only contains RDMA Request port
	rdmaRspPorts map[uint64]akita.Port // Only contains RDMA Response port

	// switch-side endpoints of each chiplet's RDMA engine (mirrors mesh API).
	swReqPorts map[uint64]akita.Port
	swRspPorts map[uint64]akita.Port
}

func NewInterChipletIdealConnector() *InterChipletIdealConnector {
	mc := &InterChipletIdealConnector{}
	mc.freq = 1 * akita.GHz
	mc.encodingOverhead = 0
	mc.flitByteSize = 16
	mc.switchLatency = 1
	mc.numReqPerCycle = 1

	mc.swReqPorts = make(map[uint64]akita.Port)
	mc.swRspPorts = make(map[uint64]akita.Port)

	mc.gridSize = [2]int{2, 2}
	mc.numChiplets = 4

	return mc
}

func (mc *InterChipletIdealConnector) WithFreq(freq akita.Freq) *InterChipletIdealConnector {
	mc.freq = freq
	return mc
}

func (mc *InterChipletIdealConnector) WithEngine(engine akita.Engine) *InterChipletIdealConnector {
	mc.engine = engine
	return mc
}

func (mc *InterChipletIdealConnector) WithSwitchLatency(numCycles int) *InterChipletIdealConnector {
	mc.switchLatency = numCycles
	return mc
}

func (mc *InterChipletIdealConnector) WithFlitByteSize(flitByteSize int) *InterChipletIdealConnector {
	mc.flitByteSize = flitByteSize
	return mc
}

func (mc *InterChipletIdealConnector) WithNumReqPerCycle(numReqPerCycle int) *InterChipletIdealConnector {
	mc.numReqPerCycle = numReqPerCycle
	return mc
}

// WithNetworkName sets the name of the network and the prefix of all the
// component in the network.
func (mc *InterChipletIdealConnector) WithNetworkName(networkName string) *InterChipletIdealConnector {
	mc.networkName = networkName
	return mc
}

func (mc *InterChipletIdealConnector) WithGridSize(xDim, yDim int) *InterChipletIdealConnector {
	mc.gridSize[0] = xDim
	mc.gridSize[1] = yDim
	return mc
}

func (mc *InterChipletIdealConnector) WithNumChiplets(n int) *InterChipletIdealConnector {
	mc.numChiplets = n
	return mc
}

func (mc *InterChipletIdealConnector) WithRDMAReqPorts(p map[uint64]akita.Port) *InterChipletIdealConnector {
	mc.rdmaReqPorts = p
	return mc
}

func (mc *InterChipletIdealConnector) WithRDMARspPorts(p map[uint64]akita.Port) *InterChipletIdealConnector {
	mc.rdmaRspPorts = p
	return mc
}

func (mc *InterChipletIdealConnector) CreateNetwork() {
	mc.createPortTable()

	mc.createSwitchConnector()

	mc.createSwitches()
	mc.createLinks()

	mc.createIdealRoutingTable()
}

func (mc *InterChipletIdealConnector) createPortTable() {
	mc.portTable = make([]map[akita.Port]akita.Port, mc.numChiplets)
}

func (mc *InterChipletIdealConnector) createSwitchConnector() {
	mc.switchConnector = switching.NewChipSwitchConnector(mc.engine)
	mc.switchConnector.SetSwitchLatency(mc.switchLatency)
	mc.switchConnector.SetNumReqPerCycle(mc.numReqPerCycle)
	mc.switchConnector.SetBufferSize(mc.numReqPerCycle)
}

func (mc *InterChipletIdealConnector) createSwitches() {
	/* initializing switches */
	mc.chipSwitches = make([][]switching.MultiFlitChipletSwitch, mc.gridSize[1])

	for i := range mc.chipSwitches {
		mc.chipSwitches[i] = make([]switching.MultiFlitChipletSwitch, mc.gridSize[0])
	}

	for y := 0; y < mc.gridSize[1]; y++ {
		for x := 0; x < mc.gridSize[0]; x++ {
			chipletId := y*mc.gridSize[0] + x

			swName := fmt.Sprintf("%s.SW_[%d][%d]", mc.networkName, y, x)

			sw := switching.MakeMultiFlitChipletSwitchImplBuilder().
				WithEngine(mc.engine).
				WithFreq(mc.freq).
				WithArbiter(arbitration.NewFlitAwareXBarArbiter(mc.numReqPerCycle)).
				WithRoutingTable(routing.NewIdealRoutingTable(y, x)).
				WithNumReqPerCycle(mc.numReqPerCycle). // this refers to bandwidth
				WithSwitchLatency(mc.switchLatency).
				WithMaxOutgoingReqsPerChiplet(12).
				WithGridSize(mc.gridSize[1], mc.gridSize[0]).
				WithFlitByteSize(mc.flitByteSize).
				WithSwitchPosition(y, x).
				Build(swName)

			mc.chipSwitches[y][x] = sw

			mc.portTable[chipletId] = make(map[akita.Port]akita.Port)
		}
	}
}

func (mc *InterChipletIdealConnector) createLinks() {
	for y := 0; y < mc.gridSize[1]; y++ {
		for x := 0; x < mc.gridSize[0]; x++ {
			mc.connectWithDevice(y, x)
			mc.connectSwitches(y, x)
		}
	}
}

func (mc *InterChipletIdealConnector) connectWithDevice(y, x int) {
	sw := mc.chipSwitches[y][x]
	chipletId := uint64(y*mc.gridSize[0] + x)
	rdmaReqPort := mc.rdmaReqPorts[chipletId]
	rdmaRspPort := mc.rdmaRspPorts[chipletId]

	reqSwPort := mc.switchConnector.ConnectDeviceToSwitch(rdmaReqPort, sw, "Req")
	rspSwPort := mc.switchConnector.ConnectDeviceToSwitch(rdmaRspPort, sw, "Rsp")

	mc.portTable[chipletId][rdmaReqPort] = reqSwPort
	mc.portTable[chipletId][rdmaRspPort] = rspSwPort

	mc.swReqPorts[chipletId] = reqSwPort
	mc.swRspPorts[chipletId] = rspSwPort
}

func (mc *InterChipletIdealConnector) GetSwReqPorts() map[uint64]akita.Port {
	return mc.swReqPorts
}

func (mc *InterChipletIdealConnector) GetSwRspPorts() map[uint64]akita.Port {
	return mc.swRspPorts
}

// connectSwitches wires switch (y,x) point-to-point to every switch with a
// higher chiplet id, giving a full crossbar (1-hop remote access).
func (mc *InterChipletIdealConnector) connectSwitches(y, x int) {
	chipletIdA := uint64(y*mc.gridSize[0] + x)
	portTableA := mc.portTable[chipletIdA]
	rdmaReqPortA := mc.rdmaReqPorts[chipletIdA]
	rdmaRspPortA := mc.rdmaRspPorts[chipletIdA]

	swA := mc.chipSwitches[y][x]

	for yy := 0; yy < mc.gridSize[1]; yy++ {
		for xx := 0; xx < mc.gridSize[0]; xx++ {
			chipletIdB := uint64(yy*mc.gridSize[0] + xx)
			if chipletIdB <= chipletIdA {
				continue
			}

			swB := mc.chipSwitches[yy][xx]
			portTableB := mc.portTable[chipletIdB]
			rdmaReqPortB := mc.rdmaReqPorts[chipletIdB]
			rdmaRspPortB := mc.rdmaRspPorts[chipletIdB]

			reqSwPortA, reqSwPortB := mc.switchConnector.ConnectSwitches(swA, swB, mc.freq, "Req")
			rspSwPortA, rspSwPortB := mc.switchConnector.ConnectSwitches(swA, swB, mc.freq, "Rsp")

			portTableA[rdmaReqPortB] = reqSwPortA
			portTableA[rdmaRspPortB] = rspSwPortA
			portTableB[rdmaReqPortA] = reqSwPortB
			portTableB[rdmaRspPortA] = rspSwPortB
		}
	}
}

func (mc *InterChipletIdealConnector) createIdealRoutingTable() {
	for r := 0; r < mc.gridSize[1]; r++ {
		for c := 0; c < mc.gridSize[0]; c++ {
			sw := mc.chipSwitches[r][c]
			chipletId := r*mc.gridSize[0] + c
			rt := sw.GetRoutingTable().(*routing.IdealRoutingTable)
			rt.SetPortTable(mc.portTable[chipletId])
		}
	}
}
