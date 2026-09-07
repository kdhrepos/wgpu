package global

func (g *Global) SetForceLHLookup(u bool) {
	g.forceLHLookup = u
}

func (g *Global) GetForceLHLookup() bool {
	return g.forceLHLookup
}
