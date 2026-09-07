package chipnetwork

import (
	"gitlab.com/akita/akita"
	"gitlab.com/akita/noc/networking/switching"
)

func (c *InterChipletConnectorImpl) GetInterChipletConnectorImplName() string {
	return c.networkName
}

func (c *InterChipletConnectorImpl) GetEngine() akita.Engine {
	return c.engine
}

func (c *InterChipletConnectorImpl) GetFreq() akita.Freq {
	return c.freq
}

func (c *InterChipletConnectorImpl) GetEncodingOverhead() float64 {
	return c.encodingOverhead
}

func (c *InterChipletConnectorImpl) GetFlitByteSize() int {
	return c.flitByteSize
}

func (c *InterChipletConnectorImpl) GetSwitchLatency() int {
	return c.switchLatency
}

func (c *InterChipletConnectorImpl) GetNumReqPerCycle() int {
	return c.numReqPerCycle
}

func (c *InterChipletConnectorImpl) GetChipSwitches() [][]switching.MultiFlitChipletSwitch {
	return c.chipSwitches
}

func (c *InterChipletConnectorImpl) GetSwitchConnector() *switching.ChipSwitchConnector {
	return c.switchConnector
}
