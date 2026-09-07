package routing

import (
	"gitlab.com/akita/akita"
)

type IdealRoutingTable struct {
	y, x      int // owner of this routing table
	portTable map[akita.Port]akita.Port
}

func NewIdealRoutingTable(y, x int) *IdealRoutingTable {
	t := &IdealRoutingTable{
		x: x,
		y: y,
	}

	return t
}

// portTable find switch port connected to destination chiplet
func (t *IdealRoutingTable) SetPortTable(pt map[akita.Port]akita.Port) {
	t.portTable = pt
}

// No need to find port when point-to-point network is used
func (t IdealRoutingTable) FindPort(dst akita.Port) akita.Port {
	nextPort := t.portTable[dst]
	return nextPort
}

// DefineRoute does noting
func (t *IdealRoutingTable) DefineRoute(finalDst, outputPort akita.Port) {
	// Do nothing.
}

// DefineDefaultRoute sets the local port.
func (t *IdealRoutingTable) DefineDefaultRoute(outputPort akita.Port) {
	/* do nothing for mesh network */
	// t.rdma = outputPort
}
