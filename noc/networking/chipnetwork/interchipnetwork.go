// Package chipnetwork for interconnect
package chipnetwork

import (
	"gitlab.com/akita/akita"
	"gitlab.com/akita/noc/networking/switching"
)

// type finalDests struct {
// 	endswitch *switching.Switch
// 	ports     []akita.Port
// }

// InterChipletConnector can connect devices into a PCIe network.
type InterChipletConnectorImpl struct {
	networkName      string
	engine           akita.Engine
	freq             akita.Freq
	encodingOverhead float64
	flitByteSize     int
	switchLatency    int
	numReqPerCycle   int

	chipSwitches    [][]switching.MultiFlitChipletSwitch
	switchConnector *switching.ChipSwitchConnector
}

// NewConnector creates a new InterChipletConnectorImpl that can help configure PCIe networks.
func NewInterChipletConnectorImpl() *InterChipletConnectorImpl {
	c := &InterChipletConnectorImpl{}
	c.encodingOverhead = 0
	c.flitByteSize = 16
	c.freq = 1 * akita.GHz
	c.numReqPerCycle = 1
	c.switchLatency = 1
	// c.finalDestinations = make(map[*switching.Switch]*finalDests)

	return c
}

func (c *InterChipletConnectorImpl) WithFreq(freq akita.Freq) *InterChipletConnectorImpl {
	c.freq = freq
	return c
}

func (c *InterChipletConnectorImpl) WithEngine(engine akita.Engine) *InterChipletConnectorImpl {
	c.engine = engine
	return c
}

func (c *InterChipletConnectorImpl) WithSwitchLatency(numCycles int) *InterChipletConnectorImpl {
	c.switchLatency = numCycles
	return c
}

func (c *InterChipletConnectorImpl) WithFlitByteSize(flitByteSize int) *InterChipletConnectorImpl {
	c.flitByteSize = flitByteSize
	return c
}

func (c *InterChipletConnectorImpl) WithNumReqPerCycle(numReqPerCycle int) *InterChipletConnectorImpl {
	c.numReqPerCycle = numReqPerCycle
	return c
}

// WithNetworkName sets the name of the network and the prefix of all the
// component in the network.
func (c *InterChipletConnectorImpl) WithNetworkName(name string) *InterChipletConnectorImpl {
	c.networkName = name
	return c
}
