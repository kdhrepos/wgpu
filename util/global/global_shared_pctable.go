package global

func (g *Global) SetUseSharedPCTable(u bool) {
	g.useSharedPCTable = u
}

func (g *Global) GetUseSharedPCTable() bool {
	return g.useSharedPCTable
}
