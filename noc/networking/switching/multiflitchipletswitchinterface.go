package switching

import (
	"gitlab.com/akita/akita"
	"gitlab.com/akita/noc/networking/arbitration"
	"gitlab.com/akita/noc/networking/routing"
	"gitlab.com/akita/util"
	"gitlab.com/akita/util/tracing"
)

/*
 *
 * multiFlitPortComplex
 * -
 */

// Flit path inside the switch (no internal latency — switch latency lives
// in MultiFlitSwitchConnection):
//
//   port.Recv  →  routeBuffer  →  forwardBuffer  →  sendOutBuffer  →  port.Send
//                (recv stage)    (route stage)     (forward stage)   (sendOut stage)
//
// All four stages execute within one Tick in forward order so a flit can
// traverse the switch in a single cycle when no backpressure exists.
type multiFlitPortComplex struct {
	// localPort is the port that is equipped on the switch.
	localPort akita.Port

	// remotePort is the port that is connected to the localPort.
	remotePort akita.Port

	// Incoming flits land here. The route stage pops from here.
	routeBuffer util.Buffer

	// Routed flits wait here for arbitration.
	forwardBuffer util.Buffer

	// forwardVOQ holds per-output-port virtual output queues (mesh switch
	// only). Splitting the single forwardBuffer by destination output port
	// removes head-of-line blocking: a flit bound for a free output is no
	// longer stuck behind a head flit whose output is full. Populated lazily
	// in route(); each queue is registered with the arbiter on creation.
	forwardVOQ map[akita.Port]util.Buffer

	// Arbitrated flits land here, to be drained by sendOut.
	sendOutBuffer util.Buffer

	// deliverBuf is the 7th-stage buffer used by the 7-stage pipeline
	// mimic (assemble→tryDeliver split). Pass-through only.
	deliverBuf util.Buffer

	// The 7-stage mimic reuses msgInBuf and flitBuf as pass-through stage
	// buffers.
	assemblingMsg   akita.Msg
	numFlitRequired int
	numFlitArrived  int
	msgInBuf        util.Buffer
	flitBuf         util.Buffer
	stucked         int
}

/*
 *
 * MultiFlitChipletSwitch
 * -
 */

// MultiFlitChipletSwitch is an Akita component that can forward request to destination.
type MultiFlitChipletSwitch interface {
	akita.Component
	tracing.NamedHookable

	SetEngine(engine akita.Engine)
	SetFreq(freq akita.Freq)
	GetEngine() akita.Engine
	GetFreq() akita.Freq

	SetPorts(ports []akita.Port)
	SetPortToComplexMapping(portToComplexMapping map[akita.Port]*multiFlitPortComplex)
	SetRoutingTable(routingTable routing.Table)
	SetArbiter(arbiter *arbitration.FlitAwareXBarArbiter)

	SetFlitByteSize(flitByteSize int)
	SetEncodingOverhead(encodingOverhead float64)
	SetSwitchLatency(switchLatency int)
	SetNumReqPerCycle(numReqPerCycle int)

	SetRow(row int)
	SetCol(col int)
	SetY(y int)
	SetX(x int)

	GetPorts() []akita.Port
	GetPortToComplexMapping() map[akita.Port]*multiFlitPortComplex
	GetRoutingTable() routing.Table
	GetArbiter() *arbitration.FlitAwareXBarArbiter

	GetFlitByteSize() int
	GetEncodingOverhead() float64
	GetSwitchLatency() int
	GetNumReqPerCycle() int

	GetRow() int
	GetCol() int
	GetY() int
	GetX() int

	addPort(pc *multiFlitPortComplex)
}

/*
 *
 * Port Adder for MultiFlitChipletSwitch
 *
 */

type MultiFlitChipletSwitchPortAdder struct {
	sw                  MultiFlitChipletSwitch
	localPort           akita.Port
	remotePort          akita.Port
	switchLatency       int
	numReqPerCycle      int
	bufferSizeInNumFlit int
}

func MakeMultiFlitChipletSwitchPortAdder(s MultiFlitChipletSwitch) MultiFlitChipletSwitchPortAdder {
	return MultiFlitChipletSwitchPortAdder{
		sw:             s,
		switchLatency:  1,
		numReqPerCycle: 1,
	}
}

func (a MultiFlitChipletSwitchPortAdder) WithPorts(local, remote akita.Port) MultiFlitChipletSwitchPortAdder {
	a.localPort = local
	a.remotePort = remote
	return a
}

func (a MultiFlitChipletSwitchPortAdder) WithSwitchLatency(switchLatency int) MultiFlitChipletSwitchPortAdder {
	a.switchLatency = switchLatency
	return a
}

func (a MultiFlitChipletSwitchPortAdder) WithNumReqPerCycle(numReqPerCycle int) MultiFlitChipletSwitchPortAdder {
	a.numReqPerCycle = numReqPerCycle
	return a
}

func (a MultiFlitChipletSwitchPortAdder) WithBufferSize(numFlit int) MultiFlitChipletSwitchPortAdder {
	a.bufferSizeInNumFlit = numFlit
	return a
}

func (a MultiFlitChipletSwitchPortAdder) AddPort() {
	msgInBuf := util.NewBuffer(a.bufferSizeInNumFlit)
	flitBuf := util.NewBuffer(a.bufferSizeInNumFlit)
	sendOutBuf := util.NewBuffer(a.bufferSizeInNumFlit)
	forwardBuf := util.NewBuffer(a.bufferSizeInNumFlit)
	routeBuf := util.NewBuffer(a.bufferSizeInNumFlit)
	deliverBuf := util.NewBuffer(a.bufferSizeInNumFlit)

	pc := &multiFlitPortComplex{
		localPort:     a.localPort,
		remotePort:    a.remotePort,
		routeBuffer:   routeBuf,
		forwardBuffer: forwardBuf,
		sendOutBuffer: sendOutBuf,
		deliverBuf:    deliverBuf,
		msgInBuf:      msgInBuf,
		flitBuf:       flitBuf,
	}

	a.sw.addPort(pc)
}
