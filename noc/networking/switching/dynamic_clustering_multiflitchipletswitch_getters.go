package switching

import (
	"gitlab.com/akita/akita"
	"gitlab.com/akita/noc/networking/arbitration"
	"gitlab.com/akita/noc/networking/routing"
)

// Getters
func (s *DynamicClusteringMultiFlitChipletSwitch) GetEngine() akita.Engine {
	return s.Engine
}

func (s *DynamicClusteringMultiFlitChipletSwitch) GetFreq() akita.Freq {
	return s.Freq
}

func (s *DynamicClusteringMultiFlitChipletSwitch) GetPorts() []akita.Port {
	return s.ports
}

func (s *DynamicClusteringMultiFlitChipletSwitch) GetPortToComplexMapping() map[akita.Port]*multiFlitPortComplex {
	return s.portToComplexMapping
}

func (s *DynamicClusteringMultiFlitChipletSwitch) GetRoutingTable() routing.Table {
	return s.routingTable
}

func (s *DynamicClusteringMultiFlitChipletSwitch) GetArbiter() *arbitration.FlitAwareXBarArbiter {
	return s.arbiter
}

func (s *DynamicClusteringMultiFlitChipletSwitch) GetNumReqPerCycle() int {
	return s.numReqPerCycle
}

func (s *DynamicClusteringMultiFlitChipletSwitch) GetFlitByteSize() int {
	return s.flitByteSize
}

func (s *DynamicClusteringMultiFlitChipletSwitch) GetEncodingOverhead() float64 {
	return s.encodingOverhead
}

func (s *DynamicClusteringMultiFlitChipletSwitch) GetSwitchLatency() int {
	return s.switchLatency
}

func (s *DynamicClusteringMultiFlitChipletSwitch) GetRow() int {
	return s.row
}

func (s *DynamicClusteringMultiFlitChipletSwitch) GetCol() int {
	return s.col
}

func (s *DynamicClusteringMultiFlitChipletSwitch) GetY() int {
	return s.y
}

func (s *DynamicClusteringMultiFlitChipletSwitch) GetX() int {
	return s.x
}

func (s *DynamicClusteringMultiFlitChipletSwitch) GetRDMAReqPort() akita.Port {
	return s.ToRDMAReqPort
}
