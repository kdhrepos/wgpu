package routing

import (
	"strings"

	"gitlab.com/akita/akita"
)

type MeshRoutingTable struct {
	y, x                 int                              // owner of this routing table
	portTable            map[akita.Port]*MeshRoutingTable // put port -> routing table from destination
	topReq, topRsp       akita.Port
	bottomReq, bottomRsp akita.Port
	rightReq, rightRsp   akita.Port
	leftReq, leftRsp     akita.Port
	localReq             akita.Port
	localRsp             akita.Port
	// local                    akita.Port
}

func NewMeshRoutingTable(y, x int) *MeshRoutingTable {
	t := &MeshRoutingTable{
		x: x,
		y: y,
	}

	return t
}

func (t *MeshRoutingTable) SetTop(
	req akita.Port,
	rsp akita.Port,
) {
	t.topReq = req
	t.topRsp = rsp
}

func (t *MeshRoutingTable) SetBottom(
	req akita.Port,
	rsp akita.Port,
) {
	t.bottomReq = req
	t.bottomRsp = rsp
}

func (t *MeshRoutingTable) SetLeft(
	req akita.Port,
	rsp akita.Port,
) {
	t.leftReq = req
	t.leftRsp = rsp
}

func (t *MeshRoutingTable) SetRight(
	req akita.Port,
	rsp akita.Port,
) {
	t.rightReq = req
	t.rightRsp = rsp
}

func (t *MeshRoutingTable) SetLocal(
	req akita.Port,
	rsp akita.Port,
) {
	t.localReq = req
	t.localRsp = rsp
}

func (t *MeshRoutingTable) SetPortTable(pt map[akita.Port]*MeshRoutingTable) {
	t.portTable = pt
}

// Find next port with X-Y routing
func (t MeshRoutingTable) FindPort(dst akita.Port) akita.Port {
	var nextPort akita.Port

	rt := t.portTable[dst] // put port -> routing table of destination
	dstX, dstY := rt.x, rt.y

	switch {
	case dstX < t.x:
		if strings.Contains(dst.Name(), "Request") {
			nextPort = t.leftReq
		} else if strings.Contains(dst.Name(), "Response") {
			nextPort = t.leftRsp
		}
	case dstX > t.x:
		if strings.Contains(dst.Name(), "Request") {
			nextPort = t.rightReq
		} else if strings.Contains(dst.Name(), "Response") {
			nextPort = t.rightRsp
		}
	case dstY < t.y:
		if strings.Contains(dst.Name(), "Request") {
			nextPort = t.topReq
		} else if strings.Contains(dst.Name(), "Response") {
			nextPort = t.topRsp
		}
	case dstY > t.y:
		if strings.Contains(dst.Name(), "Request") {
			nextPort = t.bottomReq
		} else if strings.Contains(dst.Name(), "Response") {
			nextPort = t.bottomRsp
		}
	case dstX == t.x && dstY == t.y:
		if strings.Contains(dst.Name(), "Request") {
			nextPort = t.localReq
		} else if strings.Contains(dst.Name(), "Response") {
			nextPort = t.localRsp
		}
	default:
		panic("unreachable")
	}

	return nextPort
}

// DefineRoute does noting
func (t *MeshRoutingTable) DefineRoute(finalDst, outputPort akita.Port) {
	// Do nothing.
}

// DefineDefaultRoute sets the local port.
func (t *MeshRoutingTable) DefineDefaultRoute(outputPort akita.Port) {
	/* do nothing for mesh network */
	// t.rdma = outputPort
}
