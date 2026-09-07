package switching

import (
	"gitlab.com/akita/akita"
	"gitlab.com/akita/noc/networking/arbitration"
	"gitlab.com/akita/noc/networking/routing"
)

// Getters
func (s *DistMultiFlitChipletSwitch) GetEngine() akita.Engine {
	return s.Engine
}

func (s *DistMultiFlitChipletSwitch) GetFreq() akita.Freq {
	return s.Freq
}

func (s *DistMultiFlitChipletSwitch) GetPorts() []akita.Port {
	return s.ports
}

func (s *DistMultiFlitChipletSwitch) GetPortToComplexMapping() map[akita.Port]*multiFlitPortComplex {
	return s.portToComplexMapping
}

func (s *DistMultiFlitChipletSwitch) GetRoutingTable() routing.Table {
	return s.routingTable
}

func (s *DistMultiFlitChipletSwitch) GetArbiter() *arbitration.FlitAwareXBarArbiter {
	return s.arbiter
}

func (s *DistMultiFlitChipletSwitch) GetNumReqPerCycle() int {
	return s.numReqPerCycle
}

func (s *DistMultiFlitChipletSwitch) GetFlitByteSize() int {
	return s.flitByteSize
}

func (s *DistMultiFlitChipletSwitch) GetEncodingOverhead() float64 {
	return s.encodingOverhead
}

func (s *DistMultiFlitChipletSwitch) GetSwitchLatency() int {
	return s.switchLatency
}

func (s *DistMultiFlitChipletSwitch) GetRow() int {
	return s.row
}

func (s *DistMultiFlitChipletSwitch) GetCol() int {
	return s.col
}

func (s *DistMultiFlitChipletSwitch) GetY() int {
	return s.y
}

func (s *DistMultiFlitChipletSwitch) GetX() int {
	return s.x
}

func (s *DistMultiFlitChipletSwitch) GetRDMAReqPort() akita.Port {
	return s.ToRDMAReqPort
}
