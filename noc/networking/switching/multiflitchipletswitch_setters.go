package switching

import (
	"gitlab.com/akita/akita"
	"gitlab.com/akita/noc/networking/arbitration"
	"gitlab.com/akita/noc/networking/routing"
)

// Setters
func (s *MultiFlitChipletSwitchImpl) SetEngine(engine akita.Engine) {
	s.Engine = engine
}

func (s *MultiFlitChipletSwitchImpl) SetFreq(freq akita.Freq) {
	s.Freq = freq
}

func (s *MultiFlitChipletSwitchImpl) SetPorts(ports []akita.Port) {
	s.ports = ports
}

func (s *MultiFlitChipletSwitchImpl) SetPortToComplexMapping(portToComplexMapping map[akita.Port]*multiFlitPortComplex) {
	s.portToComplexMapping = portToComplexMapping
}

func (s *MultiFlitChipletSwitchImpl) SetRoutingTable(routingTable routing.Table) {
	s.routingTable = routingTable
}

func (s *MultiFlitChipletSwitchImpl) SetArbiter(arbiter *arbitration.FlitAwareXBarArbiter) {
	s.arbiter = arbiter
}

func (s *MultiFlitChipletSwitchImpl) SetNumReqPerCycle(numReqPerCycle int) {
	s.numReqPerCycle = numReqPerCycle
}

func (s *MultiFlitChipletSwitchImpl) SetFlitByteSize(flitByteSize int) {
	s.flitByteSize = flitByteSize
}

func (s *MultiFlitChipletSwitchImpl) SetEncodingOverhead(encodingOverhead float64) {
	s.encodingOverhead = encodingOverhead
}

func (s *MultiFlitChipletSwitchImpl) SetSwitchLatency(switchLatency int) {
	s.switchLatency = switchLatency
}

func (s *MultiFlitChipletSwitchImpl) SetRow(row int) {
	s.row = row
}

func (s *MultiFlitChipletSwitchImpl) SetCol(col int) {
	s.col = col
}

func (s *MultiFlitChipletSwitchImpl) SetY(y int) {
	s.y = y
}

func (s *MultiFlitChipletSwitchImpl) SetX(x int) {
	s.x = x
}

func (s *MultiFlitChipletSwitchImpl) SetRDMAReqPort(port akita.Port) {
	s.ToRDMAReqPort = port
}
