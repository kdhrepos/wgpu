package l1v

import (
	"gitlab.com/akita/akita"
	"gitlab.com/akita/lmf"
)

func (c *Cache) GetTopPort() akita.Port {
	return c.TopPort
}

func (c *Cache) GetBottomToL2Port() akita.Port {
	panic("L1V cache does not have BottomToL2Port")
}

func (c *Cache) GetBottomToRdmaPort() akita.Port {
	panic("L1V cache does not have BottomToRdmaPort")
}

func (c *Cache) GetBottomPort() akita.Port {
	return c.BottomPort
}

func (c *Cache) GetControlPort() akita.Port {
	return c.ControlPort
}

func (c *Cache) GetLowModuleFinder() lmf.LowModuleFinder {
	return c.lowModuleFinder
}
