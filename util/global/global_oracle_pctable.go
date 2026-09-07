package global

func (g *Global) SetUseOraclePCTable(u bool) {
	g.useOraclePCTable = u
}

func (g *Global) GetUseOraclePCTable() bool {
	return g.useOraclePCTable
}
