package chipnetwork

import (
	"gitlab.com/akita/akita"
	"gitlab.com/akita/noc/networking/switching"
)

func (c *InterChipletConnectorImpl) SetNetworkName(name string) {
	c.networkName = name
}

func (c *InterChipletConnectorImpl) SetEngine(engine akita.Engine) {
	c.engine = engine
}

func (c *InterChipletConnectorImpl) SetFreq(freq akita.Freq) {
	c.freq = freq
}

func (c *InterChipletConnectorImpl) SetEncodingOverhead(overhead float64) {
	c.encodingOverhead = overhead
}

func (c *InterChipletConnectorImpl) SetFlitByteSize(size int) {
	c.flitByteSize = size
}

func (c *InterChipletConnectorImpl) SetSwitchLatency(latency int) {
	c.switchLatency = latency
}

func (c *InterChipletConnectorImpl) SetNumReqPerCycle(num int) {
	c.numReqPerCycle = num
}

func (c *InterChipletConnectorImpl) SetChipSwitches(switches [][]switching.MultiFlitChipletSwitch) {
	c.chipSwitches = switches
}

func (c *InterChipletConnectorImpl) SetSwitchConnector(connector *switching.ChipSwitchConnector) {
	c.switchConnector = connector
}
