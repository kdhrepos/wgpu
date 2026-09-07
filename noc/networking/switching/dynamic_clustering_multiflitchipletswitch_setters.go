package switching

import (
	"gitlab.com/akita/akita"
	"gitlab.com/akita/noc/networking/arbitration"
	"gitlab.com/akita/noc/networking/routing"
)

// Setters
func (s *DynamicClusteringMultiFlitChipletSwitch) SetEngine(engine akita.Engine) {
	s.Engine = engine
}

func (s *DynamicClusteringMultiFlitChipletSwitch) SetFreq(freq akita.Freq) {
	s.Freq = freq
}

func (s *DynamicClusteringMultiFlitChipletSwitch) SetPorts(ports []akita.Port) {
	s.ports = ports
}

func (s *DynamicClusteringMultiFlitChipletSwitch) SetPortToComplexMapping(portToComplexMapping map[akita.Port]*multiFlitPortComplex) {
	s.portToComplexMapping = portToComplexMapping
}

func (s *DynamicClusteringMultiFlitChipletSwitch) SetRoutingTable(routingTable routing.Table) {
	s.routingTable = routingTable
}

func (s *DynamicClusteringMultiFlitChipletSwitch) SetArbiter(arbiter *arbitration.FlitAwareXBarArbiter) {
	s.arbiter = arbiter
}

func (s *DynamicClusteringMultiFlitChipletSwitch) SetNumReqPerCycle(numReqPerCycle int) {
	s.numReqPerCycle = numReqPerCycle
}

func (s *DynamicClusteringMultiFlitChipletSwitch) SetFlitByteSize(flitByteSize int) {
	s.flitByteSize = flitByteSize
}

func (s *DynamicClusteringMultiFlitChipletSwitch) SetEncodingOverhead(encodingOverhead float64) {
	s.encodingOverhead = encodingOverhead
}

func (s *DynamicClusteringMultiFlitChipletSwitch) SetSwitchLatency(switchLatency int) {
	s.switchLatency = switchLatency
}

func (s *DynamicClusteringMultiFlitChipletSwitch) SetRow(row int) {
	s.row = row
}

func (s *DynamicClusteringMultiFlitChipletSwitch) SetCol(col int) {
	s.col = col
}

func (s *DynamicClusteringMultiFlitChipletSwitch) SetY(y int) {
	s.y = y
}

func (s *DynamicClusteringMultiFlitChipletSwitch) SetX(x int) {
	s.x = x
}

func (s *DynamicClusteringMultiFlitChipletSwitch) SetRDMAReqPort(port akita.Port) {
	s.ToRDMAReqPort = port
}
