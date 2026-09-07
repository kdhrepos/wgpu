// Package chipnetwork for interconnect
package chipnetwork

import (
	"gitlab.com/akita/akita"
	"gitlab.com/akita/noc/networking/switching"
)

// InterChipletConnector can connect devices into a PCIe network.
type InterChipletConnector interface {
	SetNetworkName(name string)
	SetEngine(engine akita.Engine)
	SetFreq(freq akita.Freq)
	SetEncodingOverhead(overhead float64)
	SetFlitByteSize(size int)
	SetSwitchLatency(latency int)
	SetNumReqPerCycle(num int)
	SetChipSwitches(switches [][]switching.MultiFlitChipletSwitch)
	SetSwitchConnector(connector *switching.ChipSwitchConnector)

	GetInterChipletConnectorImplName() string
	GetEngine() akita.Engine
	GetFreq() akita.Freq
	GetEncodingOverhead() float64
	GetFlitByteSize() int
	GetSwitchLatency() int
	GetNumReqPerCycle() int
	GetChipSwitches() [][]switching.MultiFlitChipletSwitch
	GetSwitchConnector() *switching.ChipSwitchConnector
}
