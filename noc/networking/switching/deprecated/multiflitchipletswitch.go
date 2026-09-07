package switching

/*
 *
 * MultiFlitChipletSwitchImpl
 * -
 */

// MultiFlitChipletSwitch is an Akita component that can forward request to destination.
// type MultiFlitChipletSwitchImpl struct {
// 	*akita.TickingComponent

// 	ports                []akita.Port
// 	portToComplexMapping map[akita.Port]*multiFlitPortComplex
// 	routingTable         routing.Table
// 	arbiter              *arbitration.FlitAwareXBarArbiter

// 	flitByteSize     int
// 	encodingOverhead float64
// 	switchLatency    int
// 	numReqPerCycle   int

// 	row int // wafer size
// 	col int // wafer size
// 	y   int // switch position
// 	x   int // switch position

// 	ToRDMAReqPort akita.Port

// 	nextPortId int
// }

// /*
// - localPort: port from switch
// - remotePort: port from another component
// */
// func (s *MultiFlitChipletSwitchImpl) addPort(
// 	pc *multiFlitPortComplex,
// ) {
// 	s.ports = append(s.ports, pc.localPort)
// 	s.portToComplexMapping[pc.localPort] = pc
// 	s.portToComplexMapping[pc.remotePort] = pc
// 	s.arbiter.AddBuffer(pc.forwardBuffer)

// 	// in the mesh network, this function will do nothing.
// 	s.routingTable.DefineRoute(pc.remotePort, pc.localPort)
// }

// // // Tick update the MultiFlitChipletSwitchImpl's state.
// // func (s *MultiFlitChipletSwitchImpl) Tick(now akita.VTimeInSec) bool {
// // 	// for i := 0; i < s.numChiplets; i++ {
// // 	// 	s.outgoingReqsPerChiplet[i] = 0
// // 	// }
// // 	s.arbiter.Reset()

// // 	madeProgress := false
// // 	madeProgress = s.tryDeliver(now) || madeProgress
// // 	madeProgress = s.assemble(now) || madeProgress
// // 	madeProgress = s.forward(now) || madeProgress
// // 	madeProgress = s.route(now) || madeProgress
// // 	madeProgress = s.startProcessing(now) || madeProgress
// // 	madeProgress = s.prepareFlits(now) || madeProgress
// // 	madeProgress = s.recv(now) || madeProgress

// // 	return madeProgress
// // }

// // Tick update the MultiFlitChipletSwitchImpl's state.
// func (s *MultiFlitChipletSwitchImpl) Tick(now akita.VTimeInSec) bool {
// 	// for i := 0; i < s.numChiplets; i++ {
// 	// 	s.outgoingReqsPerChiplet[i] = 0
// 	// }
// 	// s.arbiter.Reset()
// 	s.nextPortId = 0

// 	madeProgress := false
// 	madeProgress = s.tryDeliver(now) || madeProgress
// 	madeProgress = s.forwardToSwitch(now) || madeProgress
// 	madeProgress = s.forwardToChiplet(now) || madeProgress
// 	madeProgress = s.route(now) || madeProgress
// 	madeProgress = s.startProcessing(now) || madeProgress
// 	madeProgress = s.prepareFlits(now) || madeProgress
// 	madeProgress = s.recv(now) || madeProgress
// 	madeProgress = s.assemble(now) || madeProgress

// 	return madeProgress
// }

// func (s *MultiFlitChipletSwitchImpl) assemble(now akita.VTimeInSec) bool {
// 	madeProgress := false

// 	for i := 0; i < s.numReqPerCycle; i++ {
// 		for _, port := range s.ports {
// 			// split two logics for RDMA <-> Switch and Switch <-> Switch
// 			item := port.Peek()
// 			if item == nil {
// 				continue
// 			}

// 			complex := s.portToComplexMapping[port]
// 			if strings.Contains(complex.remotePort.Name(), "RDMA") {
// 				complex.assemblingMsg = item
// 				port.Retrieve(now)
// 				// Handle RDMA messages
// 			} else {
// 				newFlits := make([]*noc.Flit, 0)
// 				for {
// 					item := port.Retrieve(now)
// 					if item == nil {
// 						break
// 					}
// 					f := item.(*noc.Flit)
// 					if complex.assemblingMsg == nil {
// 						complex.assemblingMsg = f.Msg
// 						complex.numFlitArrived = 1
// 						complex.numFlitRequired = f.NumFlitInMsg
// 						madeProgress = true
// 					} else if complex.assemblingMsg != nil && f.Msg == complex.assemblingMsg {
// 						complex.numFlitArrived++
// 						madeProgress = true
// 					} else {
// 						newFlits = append(newFlits, f)
// 					}
// 				}
// 				for i := 0; i < len(newFlits); i++ {
// 					// push the assembled messages
// 					complex.sendOutBuffer.Push(newFlits[i])
// 				}
// 			}
// 		}
// 	}

// 	return madeProgress
// }

// func (s *MultiFlitChipletSwitchImpl) sendMsg(now akita.VTimeInSec) bool {
// 	madeProgress := false

// 	for i := 0; i < s.numReqPerCycle; i++ {
// 		for _, port := range s.ports {
// 			complex := s.portToComplexMapping[port]
// 			if complex.assemblingMsg == nil {
// 				continue
// 			}
// 			if complex.numFlitArrived < complex.numFlitRequired {
// 				continue
// 			}
// 			if !complex.msgInBuf.CanPush() {
// 				continue
// 			}

// 			// complex.assemblingMsg.Meta().SendTime = now

// 			complex.msgInBuf.Push(complex.assemblingMsg)

// 			complex.assemblingMsg = nil
// 			complex.numFlitRequired = 0
// 			complex.numFlitArrived = 0

// 			madeProgress = true
// 		}
// 	}

// 	return madeProgress
// }

// func (s *MultiFlitChipletSwitchImpl) msgToFlits(msg akita.Msg) []*noc.Flit {
// 	numFlit := 1
// 	if msg.Meta().TrafficBytes > 0 {
// 		trafficByte := msg.Meta().TrafficBytes
// 		trafficByte += int(math.Ceil(
// 			float64(trafficByte) * s.encodingOverhead))
// 		numFlit = (trafficByte-1)/s.flitByteSize + 1
// 	}

// 	flits := make([]*noc.Flit, numFlit)
// 	for i := 0; i < numFlit; i++ {
// 		flits[i] = noc.FlitBuilder{}.
// 			WithSrc(msg.Meta().Src).
// 			WithDst(msg.Meta().Dst).
// 			WithSeqID(i).
// 			WithNumFlitInMsg(numFlit).
// 			WithMsg(msg).
// 			Build()
// 	}
// 	return flits
// }

// func (s *MultiFlitChipletSwitchImpl) assignFlitOutputBuf(f *noc.Flit) {
// 	outPort := s.routingTable.FindPort(f.Msg.Meta().Dst)
// 	complex := s.portToComplexMapping[outPort]
// 	f.OutputBuf = complex.sendOutBuffer
// }

// func (s *MultiFlitChipletSwitchImpl) recv(now akita.VTimeInSec) bool {
// 	madeProgress := false

// 	for i := 0; i < s.numReqPerCycle; i++ {
// 		for _, port := range s.ports {
// 			complex := s.portToComplexMapping[port]

// 			if complex.msgInBuf.Size() == 0 {
// 				continue
// 			}

// 			// if flits are already set up in flit buffer, skip
// 			if complex.flitBuf.Size() > 0 {
// 				continue
// 			}

// 			msg := complex.msgInBuf.Pop().(akita.Msg)

// 			if strings.Contains(complex.remotePort.Name(), "RDMA") {
// 				complex.flitBuf.push(msg)
// 			} else {
// 				// make flits based on msg
// 				for _, f := range s.msgToFlits(msg) {
// 					s.assignFlitOutputBuf(f)
// 					complex.flitBuf.Push(f)
// 				}

// 			}
// 			madeProgress = true

// 			if global.GetInstance().GetVerbose() {
// 				fmt.Printf("recv, %s, %s, %s, %s, %.10f\n",
// 					s.Name(),
// 					msg.Meta().Src.Name(),
// 					msg.Meta().Dst.Name(),
// 					msg.Meta().ID,
// 					now,
// 				)
// 			}

// 			tracing.AddTaskStep(
// 				msg.Meta().ID,
// 				now,
// 				s,
// 				"")
// 		}
// 	}
// 	return madeProgress
// }

// // The order in which MultiFlitChipletSwitchImpls tick might also be important
// func (s *MultiFlitChipletSwitchImpl) forward(now akita.VTimeInSec) (madeProgress bool) {

// 	for i := 0; i < s.numReqPerCycle; i++ {
// 		// arbitration across flitInBuf
// 		inputBuffers := s.arbitrate()

// 		for _, buf := range inputBuffers {
// 			item := buf.Peek()
// 			if item == nil {
// 				continue
// 			}

// 			item.Meta().SendTime = now

// 			// find the next destination, and get the complex
// 			// based on the complex, send the request to next node
// 			s.assignFlitOutputBuf(flit)

// 			complex := s.portToComplexMapping[flit.OutputBuf]

// 			err := complex.localPort.Send(flit)
// 			if err == nil {
// 				buf.Pop()
// 				madeProgress = true
// 			}
// 		}
// 	}

// 	return madeProgress
// }

// func (s *MultiFlitChipletSwitchImpl) arbitrate() []util.Buffer {
// 	selectedBuffers := make([]util.Buffer, 0)
// 	startingPortId := s.nextPortId

// 	for i := 0; i < len(s.ports); i++ {
// 		currPortId := (startingPortId + i) % len(s.ports)
// 		currPort := s.ports[currPortId]
// 		complex := s.portToComplexMapping[currPort]
// 		selectedBuffers = append(selectedBuffers, complex.forwardBuffer)
// 	}

// 	s.nextPortId = (s.nextPortId + 1) & len(s.ports)
// 	return selectedBuffers
// }

// /*
//  *
//  * Builder for MultiFlitChipletSwitchImpl
//  *
//  */

// // MultiFlitChipletSwitchImplBuilder can build MultiFlitChipletSwitchImpls
// type MultiFlitChipletSwitchImplBuilder struct {
// 	engine         akita.Engine
// 	freq           akita.Freq
// 	routingTable   routing.Table
// 	arbiter        arbitration.Arbiter
// 	numReqPerCycle int
// 	switchLatency  int
// 	// numChiplets               int
// 	// maxOutgoingReqsPerChiplet int

// 	row int
// 	col int
// 	y   int
// 	x   int
// }

// func MakeMultiFlitChipletSwitchImplBuilder() *MultiFlitChipletSwitchImplBuilder {
// 	b := MultiFlitChipletSwitchImplBuilder{}
// 	return &b
// }

// // WithEngine sets the engine that the MultiFlitChipletSwitchImpl to build uses.
// func (b *MultiFlitChipletSwitchImplBuilder) WithEngine(engine akita.Engine) *MultiFlitChipletSwitchImplBuilder {
// 	b.engine = engine
// 	return b
// }

// // WithFreq sets the frequency that the MultiFlitChipletSwitchImpl to build works at.
// func (b *MultiFlitChipletSwitchImplBuilder) WithFreq(freq akita.Freq) *MultiFlitChipletSwitchImplBuilder {
// 	b.freq = freq
// 	return b
// }

// // WithArbiter sets the arbiter to be used by the swtich to build.
// func (b *MultiFlitChipletSwitchImplBuilder) WithArbiter(arbiter arbitration.Arbiter) *MultiFlitChipletSwitchImplBuilder {
// 	b.arbiter = arbiter
// 	return b
// }

// // WithRoutingTable sets the routing table to be used by the MultiFlitChipletSwitchImpl to build.
// func (b *MultiFlitChipletSwitchImplBuilder) WithRoutingTable(rt routing.Table) *MultiFlitChipletSwitchImplBuilder {
// 	b.routingTable = rt
// 	return b
// }

// func (b *MultiFlitChipletSwitchImplBuilder) WithNumReqPerCycle(numReqPerCycle int) *MultiFlitChipletSwitchImplBuilder {
// 	b.numReqPerCycle = numReqPerCycle
// 	return b
// }

// func (b *MultiFlitChipletSwitchImplBuilder) WithMaxOutgoingReqsPerChiplet(maxOutgoingReqsPerChiplet int) *MultiFlitChipletSwitchImplBuilder {
// 	// b.maxOutgoingReqsPerChiplet = maxOutgoingReqsPerChiplet
// 	return b
// }

// func (b *MultiFlitChipletSwitchImplBuilder) WithGridSize(r, c int) *MultiFlitChipletSwitchImplBuilder {
// 	b.row = r
// 	b.col = c
// 	return b
// }

// func (b *MultiFlitChipletSwitchImplBuilder) WithSwitchPosition(y, x int) *MultiFlitChipletSwitchImplBuilder {
// 	b.y = y
// 	b.x = x
// 	return b
// }

// func (b *MultiFlitChipletSwitchImplBuilder) WithSwitchLatency(sw int) *MultiFlitChipletSwitchImplBuilder {
// 	b.switchLatency = sw
// 	return b
// }

// // Build creates a new MultiFlitChipletSwitchImpl
// func (b MultiFlitChipletSwitchImplBuilder) Build(name string) *MultiFlitChipletSwitchImpl {

// 	b.engineMustBeGiven()
// 	b.freqMustNotBeZero()
// 	b.routingTableMustBeGiven()
// 	b.arbiterMustBeGiven()

// 	s := &MultiFlitChipletSwitchImpl{}
// 	s.TickingComponent = akita.NewTickingComponent(name, b.engine, b.freq, s)
// 	s.routingTable = b.routingTable
// 	s.arbiter = b.arbiter.(*arbitration.FlitAwareXBarArbiter)
// 	s.portToComplexMapping = make(map[akita.Port]*multiFlitPortComplex)
// 	s.numReqPerCycle = b.numReqPerCycle
// 	s.flitByteSize = 64
// 	s.encodingOverhead = 0
// 	s.switchLatency = b.switchLatency //720 //180 //360 // changed this here
// 	s.row = b.row
// 	s.col = b.col
// 	// fmt.Println("switch latency:", s.switchLatency)

// 	// position for mesh network
// 	// s.pos[0] = b.pos[0]
// 	// s.pos[1] = b.pos[1]

// 	return s
// }

// func (b MultiFlitChipletSwitchImplBuilder) engineMustBeGiven() {
// 	if b.engine == nil {
// 		panic("engine of MultiFlitChipletSwitchImpl is not given")
// 	}
// }

// func (b MultiFlitChipletSwitchImplBuilder) freqMustNotBeZero() {
// 	if b.freq == 0 {
// 		panic("MultiFlitChipletSwitchImpl frequency cannot be 0")
// 	}
// }

// func (b MultiFlitChipletSwitchImplBuilder) routingTableMustBeGiven() {
// 	if b.routingTable == nil {
// 		panic("MultiFlitChipletSwitchImpl requires a routing table to operate")
// 	}
// }

// func (b MultiFlitChipletSwitchImplBuilder) arbiterMustBeGiven() {
// 	if b.arbiter == nil {
// 		panic("MultiFlitChipletSwitchImpl requires an arbiter to operate")
// 	}
// }
