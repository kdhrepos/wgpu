package switching

import (
	"fmt"

	"gitlab.com/akita/akita"
)

// ChipSwitchConnector can connect switches and ports together.
type ChipSwitchConnector struct {
	engine              akita.Engine
	bufferSizeInNumFlit int
	switchLatency       int
	numReqPerCycle      int

	switchConnection []*MultiFlitSwitchConnection
	endPoints        []*EndPoint
}

// NewChipSwitchConnector creates a new switch connector.
func NewChipSwitchConnector(engine akita.Engine) *ChipSwitchConnector {

	c := &ChipSwitchConnector{
		engine:              engine,
		bufferSizeInNumFlit: 1,
		switchLatency:       1,
		switchConnection:    []*MultiFlitSwitchConnection{},
	}

	return c
}

func (c *ChipSwitchConnector) GetSwitchConnection() []*MultiFlitSwitchConnection {
	return c.switchConnection
}

// GetEndPoints returns every EndPoint this connector created when wiring
// device ports to switches. Callers (e.g. the runner's tracer hookup)
// use this to attach hooks to all msg ↔ flit boundaries.
func (c *ChipSwitchConnector) GetEndPoints() []*EndPoint {
	return c.endPoints
}

// SetBufferSize sets the default buffer size of the port at each switching for
// incoming messages.
func (c *ChipSwitchConnector) SetBufferSize(numFlit int) {
	c.bufferSizeInNumFlit = numFlit
}

// SetSwitchLatency sets the number of cycles required between when the message
// arrives at a switch and when the message is forwarded.
func (c *ChipSwitchConnector) SetSwitchLatency(numCycles int) {
	fmt.Println("setting switch connector latency:", numCycles)
	c.switchLatency = numCycles
}

func (c *ChipSwitchConnector) SetNumReqPerCycle(numReqPerCycle int) {
	c.numReqPerCycle = numReqPerCycle
}

// ConnectSwitches connect two switches together.
func (c *ChipSwitchConnector) ConnectSwitches(
	a, b MultiFlitChipletSwitch,
	freq akita.Freq,
	linkType string,
) (akita.Port, akita.Port) {
	portA := akita.NewLimitNumMsgPort(a, c.bufferSizeInNumFlit,
		fmt.Sprintf("%s.%sPort%d", a.Name(), linkType, len(a.GetPorts())))
	portB := akita.NewLimitNumMsgPort(b, c.bufferSizeInNumFlit,
		fmt.Sprintf("%s.%sPort%d", b.Name(), linkType, len(b.GetPorts())))

	// create two ports for each of switches before connecting them
	MakeMultiFlitChipletSwitchPortAdder(a).
		WithPorts(portA, portB).
		WithSwitchLatency(a.GetSwitchLatency()).
		WithNumReqPerCycle(a.GetNumReqPerCycle()).
		WithBufferSize(c.bufferSizeInNumFlit).
		AddPort()

	MakeMultiFlitChipletSwitchPortAdder(b).
		WithPorts(portB, portA).
		WithSwitchLatency(b.GetSwitchLatency()).
		WithNumReqPerCycle(b.GetNumReqPerCycle()).
		WithBufferSize(c.bufferSizeInNumFlit).
		AddPort()

	switchConn := NewMultiFlitSwitchConnection(
		portA.Name()+"-"+portB.Name()+".SwitchConnection",
		a.GetEngine(),
		a.GetFreq(),
		a.GetNumReqPerCycle(),
		a.GetSwitchLatency(),
		a.GetRow(),
		a.GetCol())

	switchConn.PlugIn(portA, c.bufferSizeInNumFlit)
	switchConn.PlugIn(portB, c.bufferSizeInNumFlit)

	c.switchConnection = append(c.switchConnection, switchConn)

	return portA, portB
}

// ConnectDeviceToSwitch connects a device port (msg level) to a switch
// (flit level) by inserting an EndPoint between them. The EndPoint
// performs msg ↔ flit conversion. The switch port itself sees only flits.
func (c *ChipSwitchConnector) ConnectDeviceToSwitch(
	rdmaPort akita.Port,
	sw MultiFlitChipletSwitch,
	linkType string,
) akita.Port {
	swPort := akita.NewLimitNumMsgPort(sw, c.bufferSizeInNumFlit,
		fmt.Sprintf("%s.%sPort%d", sw.Name(), linkType, len(sw.GetPorts())))

	ep := MakeEndPointBuilder().
		WithEngine(sw.GetEngine()).
		WithFreq(sw.GetFreq()).
		WithFlitByteSize(sw.GetFlitByteSize()).
		WithNumReqPerCycle(sw.GetNumReqPerCycle()).
		WithNetworkPortBufferSize(sw.GetNumReqPerCycle()).
		WithDevicePorts([]akita.Port{rdmaPort}).
		Build(fmt.Sprintf("%s.EP", rdmaPort.Name()))
	ep.DefaultSwitchDst = swPort
	c.endPoints = append(c.endPoints, ep)

	MakeMultiFlitChipletSwitchPortAdder(sw).
		WithPorts(swPort, ep.NetworkPort).
		WithSwitchLatency(sw.GetSwitchLatency()).
		WithNumReqPerCycle(sw.GetNumReqPerCycle()).
		WithBufferSize(c.bufferSizeInNumFlit).
		AddPort()

	switchConn := NewMultiFlitSwitchConnection(
		swPort.Name()+"-"+ep.NetworkPort.Name()+".Connection",
		sw.GetEngine(),
		sw.GetFreq(),
		sw.GetNumReqPerCycle(),
		0,
		sw.GetRow(),
		sw.GetCol())

	switchConn.PlugIn(swPort, c.bufferSizeInNumFlit)
	switchConn.PlugIn(ep.NetworkPort, c.bufferSizeInNumFlit)

	c.switchConnection = append(c.switchConnection, switchConn)

	return swPort
}
