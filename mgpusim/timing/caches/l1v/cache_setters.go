package l1v

import "gitlab.com/akita/lmf"

func (c *Cache) SetLowModuleFinder(lowModuleFinder lmf.LowModuleFinder) {
	c.lowModuleFinder = lowModuleFinder
}
