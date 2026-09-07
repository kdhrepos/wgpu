package switching

import (
	"gitlab.com/akita/akita"
	"gitlab.com/akita/noc/networking/arbitration"
	"gitlab.com/akita/noc/networking/routing"
)

// Getters
func (s *MultiFlitChipletSwitchImpl) GetEngine() akita.Engine {
	return s.Engine
}

func (s *MultiFlitChipletSwitchImpl) GetFreq() akita.Freq {
	return s.Freq
}

func (s *MultiFlitChipletSwitchImpl) GetPorts() []akita.Port {
	return s.ports
}

func (s *MultiFlitChipletSwitchImpl) GetPortToComplexMapping() map[akita.Port]*multiFlitPortComplex {
	return s.portToComplexMapping
}

func (s *MultiFlitChipletSwitchImpl) GetRoutingTable() routing.Table {
	return s.routingTable
}

func (s *MultiFlitChipletSwitchImpl) GetArbiter() *arbitration.FlitAwareXBarArbiter {
	return s.arbiter
}

func (s *MultiFlitChipletSwitchImpl) GetNumReqPerCycle() int {
	return s.numReqPerCycle
}

func (s *MultiFlitChipletSwitchImpl) GetFlitByteSize() int {
	return s.flitByteSize
}

func (s *MultiFlitChipletSwitchImpl) GetEncodingOverhead() float64 {
	return s.encodingOverhead
}

func (s *MultiFlitChipletSwitchImpl) GetSwitchLatency() int {
	return s.switchLatency
}

func (s *MultiFlitChipletSwitchImpl) GetRow() int {
	return s.row
}

func (s *MultiFlitChipletSwitchImpl) GetCol() int {
	return s.col
}

func (s *MultiFlitChipletSwitchImpl) GetY() int {
	return s.y
}

func (s *MultiFlitChipletSwitchImpl) GetX() int {
	return s.x
}

func (s *MultiFlitChipletSwitchImpl) GetRDMAReqPort() akita.Port {
	return s.ToRDMAReqPort
}
