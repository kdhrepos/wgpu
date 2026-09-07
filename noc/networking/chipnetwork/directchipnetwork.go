package chipnetwork

import (
	"gitlab.com/akita/akita"
	"gitlab.com/akita/noc/networking/switching"
)

type InterChipletDirectConnector struct {
	InterChipletConnectorImpl

	// routingTableFinder map[akita.Port]*routing.IdealRoutingTable
	portTable []map[akita.Port]akita.Port

	// chiplet dimension & # of chiplets
	numChiplets  int
	gridSize     [2]int
	rdmaReqPorts map[uint64]akita.Port // Only contains RDMA Request port
	rdmaRspPorts map[uint64]akita.Port // Only contains RDMA Response port

	idealConnection *switching.DirectConnectionWithLatency
}

func NewInterChipletDirectConnector() *InterChipletDirectConnector {
	mc := &InterChipletDirectConnector{}
	mc.freq = 1 * akita.GHz
	mc.encodingOverhead = 0
	mc.flitByteSize = 64
	mc.switchLatency = 1
	mc.numReqPerCycle = 1

	mc.gridSize = [2]int{2, 2}
	mc.numChiplets = 4

	return mc
}

func (mc *InterChipletDirectConnector) WithFreq(freq akita.Freq) *InterChipletDirectConnector {
	mc.freq = freq
	return mc
}

func (mc *InterChipletDirectConnector) WithEngine(engine akita.Engine) *InterChipletDirectConnector {
	mc.engine = engine
	return mc
}

func (mc *InterChipletDirectConnector) WithSwitchLatency(numCycles int) *InterChipletDirectConnector {
	mc.switchLatency = numCycles
	return mc
}

func (mc *InterChipletDirectConnector) WithFlitByteSize(flitByteSize int) *InterChipletDirectConnector {
	mc.flitByteSize = flitByteSize
	return mc
}

func (mc *InterChipletDirectConnector) WithNumReqPerCycle(numReqPerCycle int) *InterChipletDirectConnector {
	mc.numReqPerCycle = numReqPerCycle
	return mc
}

// WithNetworkName sets the name of the network and the prefix of all the
// component in the network.
func (mc *InterChipletDirectConnector) WithNetworkName(networkName string) *InterChipletDirectConnector {
	mc.networkName = networkName
	return mc
}

func (mc *InterChipletDirectConnector) WithGridSize(xDim, yDim int) *InterChipletDirectConnector {
	mc.gridSize[0] = xDim
	mc.gridSize[1] = yDim
	return mc
}

func (mc *InterChipletDirectConnector) WithNumChiplets(n int) *InterChipletDirectConnector {
	mc.numChiplets = n
	return mc
}

func (mc *InterChipletDirectConnector) WithRDMAReqPorts(p map[uint64]akita.Port) *InterChipletDirectConnector {
	mc.rdmaReqPorts = p
	return mc
}

func (mc *InterChipletDirectConnector) WithRDMARspPorts(p map[uint64]akita.Port) *InterChipletDirectConnector {
	mc.rdmaRspPorts = p
	return mc
}

func (mc *InterChipletDirectConnector) CreateNetwork() {
	mc.createDirectNetwork()
	mc.createLinks()
}

func (mc *InterChipletDirectConnector) createDirectNetwork() {
	mc.idealConnection = switching.NewDirectConnectionWithLatency(
		mc.networkName+"IdealConnection",
		mc.engine,
		mc.freq,
		mc.numReqPerCycle,
		mc.switchLatency)
}

func (mc *InterChipletDirectConnector) createLinks() {
	for y := 0; y < mc.gridSize[1]; y++ {
		for x := 0; x < mc.gridSize[0]; x++ {
			mc.connectDevices(y, x)
		}
	}
}

func (mc *InterChipletDirectConnector) connectDevices(y, x int) {
	chipletId := uint64(y*mc.gridSize[0] + x)
	rdmaReqPort := mc.rdmaReqPorts[chipletId]
	rdmaRspPort := mc.rdmaRspPorts[chipletId]

	// mc.idealConnection.PlugIn(rdmaReqPort, 2*mc.numReqPerCycle*mc.numChiplets)
	// mc.idealConnection.PlugIn(rdmaRspPort, 2*mc.numReqPerCycle*mc.numChiplets)

	mc.idealConnection.PlugIn(rdmaReqPort, 4*mc.numReqPerCycle)
	mc.idealConnection.PlugIn(rdmaRspPort, 4*mc.numReqPerCycle)
}
