package switching

import (
	"gitlab.com/akita/akita"
	"gitlab.com/akita/noc/networking/arbitration"
	"gitlab.com/akita/noc/networking/routing"
)

// Setters
func (s *DistMultiFlitChipletSwitch) SetEngine(engine akita.Engine) {
	s.Engine = engine
}

func (s *DistMultiFlitChipletSwitch) SetFreq(freq akita.Freq) {
	s.Freq = freq
}

func (s *DistMultiFlitChipletSwitch) SetPorts(ports []akita.Port) {
	s.ports = ports
}

func (s *DistMultiFlitChipletSwitch) SetPortToComplexMapping(portToComplexMapping map[akita.Port]*multiFlitPortComplex) {
	s.portToComplexMapping = portToComplexMapping
}

func (s *DistMultiFlitChipletSwitch) SetRoutingTable(routingTable routing.Table) {
	s.routingTable = routingTable
}

func (s *DistMultiFlitChipletSwitch) SetArbiter(arbiter *arbitration.FlitAwareXBarArbiter) {
	s.arbiter = arbiter
}

func (s *DistMultiFlitChipletSwitch) SetNumReqPerCycle(numReqPerCycle int) {
	s.numReqPerCycle = numReqPerCycle
}

func (s *DistMultiFlitChipletSwitch) SetFlitByteSize(flitByteSize int) {
	s.flitByteSize = flitByteSize
}

func (s *DistMultiFlitChipletSwitch) SetEncodingOverhead(encodingOverhead float64) {
	s.encodingOverhead = encodingOverhead
}

func (s *DistMultiFlitChipletSwitch) SetSwitchLatency(switchLatency int) {
	s.switchLatency = switchLatency
}

func (s *DistMultiFlitChipletSwitch) SetRow(row int) {
	s.row = row
}

func (s *DistMultiFlitChipletSwitch) SetCol(col int) {
	s.col = col
}

func (s *DistMultiFlitChipletSwitch) SetY(y int) {
	s.y = y
}

func (s *DistMultiFlitChipletSwitch) SetX(x int) {
	s.x = x
}

func (s *DistMultiFlitChipletSwitch) SetRDMAReqPort(port akita.Port) {
	s.ToRDMAReqPort = port
}
