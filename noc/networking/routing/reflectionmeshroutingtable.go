package routing

import (
	"strings"

	"gitlab.com/akita/akita"
)

type ReflectionMeshRoutingTable struct {
	y, x                 int                                        // owner of this routing table
	portTable            map[akita.Port]*ReflectionMeshRoutingTable // put port -> routing table from destination
	topReq, topRsp       akita.Port
	bottomReq, bottomRsp akita.Port
	rightReq, rightRsp   akita.Port
	leftReq, leftRsp     akita.Port
	localReq             akita.Port
	localRsp             akita.Port
	// local                    akita.Port
}

func NewReflectionMeshRoutingTable(y, x int) *ReflectionMeshRoutingTable {
	t := &ReflectionMeshRoutingTable{
		x: x,
		y: y,
	}

	return t
}

func (t *ReflectionMeshRoutingTable) SetTop(
	req akita.Port,
	rsp akita.Port,
) {
	t.topReq = req
	t.topRsp = rsp
}

func (t *ReflectionMeshRoutingTable) SetBottom(
	req akita.Port,
	rsp akita.Port,
) {
	t.bottomReq = req
	t.bottomRsp = rsp
}

func (t *ReflectionMeshRoutingTable) SetLeft(
	req akita.Port,
	rsp akita.Port,
) {
	t.leftReq = req
	t.leftRsp = rsp
}

func (t *ReflectionMeshRoutingTable) SetRight(
	req akita.Port,
	rsp akita.Port,
) {
	t.rightReq = req
	t.rightRsp = rsp
}

func (t *ReflectionMeshRoutingTable) SetLocal(
	req akita.Port,
	rsp akita.Port,
) {
	t.localReq = req
	t.localRsp = rsp
}

func (t *ReflectionMeshRoutingTable) SetPortTable(pt map[akita.Port]*ReflectionMeshRoutingTable) {
	t.portTable = pt
}

// Find next port with X-Y routing
func (t ReflectionMeshRoutingTable) FindPort(dst akita.Port) akita.Port {
	var nextPort akita.Port

	rt := t.portTable[dst] // put port -> routing table of destination
	curX, curY := t.x, t.y
	dstX, dstY := rt.x, rt.y

	if strings.Contains(dst.Name(), "Request") {
		switch {
		case dstX < curX:
			nextPort = t.leftReq
		case dstX > curX:
			nextPort = t.rightReq
		case dstY < curY:
			nextPort = t.topReq
		case dstY > curY:
			nextPort = t.bottomReq
		case dstX == curX && dstY == curY:
			nextPort = t.localReq
		default:
			panic("unreachable")
		}
	}

	if strings.Contains(dst.Name(), "Response") {
		switch {
		case dstY < curY:
			nextPort = t.topRsp
		case dstY > curY:
			nextPort = t.bottomRsp
		case dstX < curX:
			nextPort = t.leftRsp
		case dstX > curX:
			nextPort = t.rightRsp
		case dstX == t.x && dstY == curY:
			nextPort = t.localRsp
		default:
			panic("unreachable")
		}
	}

	return nextPort
}

// DefineRoute does noting
func (t *ReflectionMeshRoutingTable) DefineRoute(finalDst, outputPort akita.Port) {
	// Do nothing.
}

// DefineDefaultRoute sets the local port.
func (t *ReflectionMeshRoutingTable) DefineDefaultRoute(outputPort akita.Port) {
	/* do nothing for mesh network */
	// t.rdma = outputPort
}
