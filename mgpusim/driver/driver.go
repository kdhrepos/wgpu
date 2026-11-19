package driver

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"log"
	"reflect"
	"strconv"
	"sync"

	"gitlab.com/akita/akita"
	"gitlab.com/akita/mem"
	"gitlab.com/akita/mem/cache/writeback"
	"gitlab.com/akita/mem/device"
	"gitlab.com/akita/mem/vm/addresstranslator"
	"gitlab.com/akita/mem/vm/tlb"
	"gitlab.com/akita/mgpusim"
	"gitlab.com/akita/mgpusim/coreprotocol"
	"gitlab.com/akita/mgpusim/kernels"
	"gitlab.com/akita/mgpusim/rdma"
	"gitlab.com/akita/mgpusim/timing/caches/l1v"
	"gitlab.com/akita/mgpusim/timing/caches/rob"
	"gitlab.com/akita/mgpusim/timing/cu"
	"gitlab.com/akita/noc/networking/switching"
	"gitlab.com/akita/util/ca"
	"gitlab.com/akita/util/global"
	"gitlab.com/akita/util/tracing"
)

// Driver is an Akita component that controls the simulated GPUs
type Driver struct {
	*akita.TickingComponent

	memAllocator device.MemoryAllocator
	distributor  distributor

	GPUs             []*mgpusim.GPU
	devices          []*device.Device
	PageTable        *device.PageTableImpl
	memAllocatorType string

	ToMMU akita.Port

	requestsToSend []akita.Msg

	contextMutex sync.Mutex
	contexts     []*Context

	ToGPUs akita.Port

	driverStopped      chan bool
	enqueueSignal      chan bool
	engineMutex        sync.Mutex
	engineRunning      bool
	engineRunningMutex sync.Mutex
	simulationID       string

	Log2PageSize uint64

	currentPageMigrationReq         *device.PageMigrationReqToDriver
	toSendToMMU                     *device.PageMigrationRspFromDriver
	migrationReqToSendToCP          []*coreprotocol.PageMigrationReqToCP
	isCurrentlyHandlingMigrationReq bool
	numRDMADrainACK                 uint64
	numRDMARestartACK               uint64
	numShootDownACK                 uint64
	numRestartACK                   uint64
	numPagesMigratingACK            uint64
	isCurrentlyMigratingOnePage     bool

	RemotePMCPorts []akita.Port

	// FastH2D, when true, makes processMemCopyH2DCommand and
	// processWritePageTableH2DCommand write bytes directly into each
	// destination chiplet's DRAM Storage instead of emitting MemCopyH2DReq
	// messages through the CP/DMA pipeline. The command completes in the
	// same tick it is issued. Set via the -fast-h2d runner flag.
	FastH2D bool
}

// Run starts a new threads that handles all commands in the command queues
func (d *Driver) Run() {
	d.logSimulationStart()
	go d.runAsync()
}

// Terminate stops the driver thread execution.
func (d *Driver) Terminate() {
	d.driverStopped <- true
	d.logSimulationTerminate()
}

func (d *Driver) logSimulationStart() {
	d.simulationID = strconv.FormatUint(akita.GetIDGenerator().Generate(), 10)
	tracing.StartTask(
		tracing.DecimalTaskID(d.simulationID),
		0,
		0,
		d,
		"Simulation", "Simulation",
		nil,
	)
}

func (d *Driver) logSimulationTerminate() {
	tracing.EndTask(tracing.DecimalTaskID(d.simulationID), d.Engine.CurrentTime(), d)
}

func (d *Driver) runAsync() {
	for {
		select {
		case <-d.driverStopped:
			return
		case <-d.enqueueSignal:
			d.Engine.Pause()

			// Schedule the driver's tick
			d.TickLater(d.Engine.CurrentTime())
			d.Engine.Continue()

			d.engineRunningMutex.Lock()
			if d.engineRunning {
				d.engineRunningMutex.Unlock()
				continue
			}

			d.engineRunning = true
			go d.runEngine()
			d.engineRunningMutex.Unlock()
		}
	}
}

func (d *Driver) runEngine() {
	d.engineMutex.Lock()
	defer d.engineMutex.Unlock()

	// components := d.getComponents()

	err := d.Engine.Run()

	if err != nil {
		panic(err)
	}

	// Quiesce-deadlock detector: the engine drained (no more events) but a
	// dispatched kernel never completed — nothing more can happen, so the
	// simulation is deadlocked. Dump where transactions are stuck (which ROB
	// group, and above-vs-below the ROB) before panicking, instead of letting
	// the driver hang waiting for a completion that never comes.
	d.checkQuiesceDeadlock()

	// if !d.GPUs[0].CommandProcessor.IsKernelCompleted() {
	// 	log.Printf("kernel not completed, force ticking until completion")

	// 	err := d.Engine.(*akita.SerialEngine).RunWithTime(components, true)
	// 	if err != nil {
	// 		panic(err)
	// 	}

	// 	log.Printf("kernel completed")
	// }

	d.engineRunningMutex.Lock()
	d.engineRunning = false
	d.engineRunningMutex.Unlock()
}

// checkQuiesceDeadlock panics with a per-ROB-group breakdown if the engine
// drained while a kernel is still incomplete. readyHeld>0 means the stall is
// ABOVE that ROB (CU not draining); awaiting>0 means it is BELOW (cache / L2 /
// network / MMU); all ROBs empty while a kernel is stuck points outside the
// vmem path entirely (CU exec / LDS / barrier / dispatcher).
func (d *Driver) checkQuiesceDeadlock() {
	for i, gpu := range d.GPUs {
		if gpu.CommandProcessor == nil || gpu.CommandProcessor.IsKernelCompleted() {
			continue
		}

		dump := func(name string, robs []*rob.ReorderBuffer) (p, r, a int) {
			for _, rb := range robs {
				pp, rr, aa := rb.PendingSummary()
				p += pp
				r += rr
				a += aa
			}
			fmt.Printf("[deadlock] GPU%d %s: pending=%d readyHeldAbove=%d awaitingBelow=%d\n",
				i+1, name, p, r, a)
			return
		}

		fmt.Printf("[deadlock] GPU%d engine drained but kernel NOT completed\n", i+1)
		vp, _, _ := dump("L1V-ROB", gpu.L1VROBs)
		sp, _, _ := dump("L1S-ROB", gpu.L1SROBs)
		ip, _, _ := dump("L1I-ROB", gpu.L1IROBs)
		if vp+sp+ip == 0 {
			fmt.Printf("[deadlock] GPU%d all ROBs empty — stall is OUTSIDE the vmem/smem/inst ROB path (CU exec / LDS / barrier / dispatcher / MMU)\n", i+1)
		}

		dumpExtendedState(i+1, gpu)

		panic(fmt.Sprintf(
			"[deadlock] GPU%d engine drained with kernel incomplete — see ROB breakdown above", i+1))
	}
}

// summarizer is implemented by every component that exposes a DeadlockSummary
// method. The driver iterates each component group, calls this method, and
// prints one line per non-clean component plus a "X/N with pending" header so
// the deadlock root cause is easy to localize.
type summarizer interface {
	DeadlockSummary() (string, bool)
	Name() string
}

// dumpGroup walks a slice of summarizer-like values, prints only the
// non-clean ones, and outputs a one-line summary at the end. Use the closure
// form (toSummarizer) so type assertions stay at the call site.
func dumpGroup(gpuIdx int, label, prefix string, count int, get func(int) (summarizer, bool)) {
	stuck := 0
	for j := 0; j < count; j++ {
		s, ok := get(j)
		if !ok || s == nil {
			continue
		}
		line, any := s.DeadlockSummary()
		if any {
			if stuck == 0 {
				fmt.Printf("[deadlock] GPU%d %s with pending state:\n", gpuIdx, label)
			}
			fmt.Printf("[deadlock]   %s[%d] %s | %s\n", prefix, j, s.Name(), line)
			stuck++
		}
	}
	if stuck == 0 {
		fmt.Printf("[deadlock] GPU%d %s: all clean (%d total)\n",
			gpuIdx, label, count)
	} else {
		fmt.Printf("[deadlock] GPU%d %s: %d/%d with pending\n",
			gpuIdx, label, stuck, count)
	}
}

// dumpExtendedState prints per-component pending state for every component
// the simulator instantiates. Each component is only listed when it actually
// has pending work, so silent components don't drown out the real stall
// location.
func dumpExtendedState(gpuIdx int, gpu *mgpusim.GPU) {
	// L2 caches
	dumpGroup(gpuIdx, "L2 caches", "L2", len(gpu.L2Caches),
		func(j int) (summarizer, bool) {
			switch c := gpu.L2Caches[j].(type) {
			case *writeback.Cache:
				return c, true
			}
			return nil, false
		})

	// L2 TLBs
	dumpGroup(gpuIdx, "L2 TLBs", "L2TLB", len(gpu.L2TLBs),
		func(j int) (summarizer, bool) {
			t, ok := gpu.L2TLBs[j].(*tlb.LatTLB)
			return t, ok
		})

	// MMUs
	dumpGroup(gpuIdx, "MMUs", "MMU", len(gpu.MMUs),
		func(j int) (summarizer, bool) { return gpu.MMUs[j], true })

	// Chip RDMA engines
	dumpGroup(gpuIdx, "Chip RDMA engines", "RDMA", len(gpu.ChipRDMAEngines),
		func(j int) (summarizer, bool) {
			switch re := gpu.ChipRDMAEngines[j].(type) {
			case *rdma.RDMAEngineImpl:
				return re, true
			}
			return nil, false
		})

	// L1V caches (per CU)
	dumpGroup(gpuIdx, "L1V caches", "L1V", len(gpu.L1VCaches),
		func(j int) (summarizer, bool) {
			c, ok := gpu.L1VCaches[j].(*l1v.Cache)
			return c, ok
		})

	// L1S caches (per SA)
	dumpGroup(gpuIdx, "L1S caches", "L1S", len(gpu.L1SCaches),
		func(j int) (summarizer, bool) {
			c, ok := gpu.L1SCaches[j].(*l1v.Cache)
			return c, ok
		})

	// L1I caches (per SA)
	dumpGroup(gpuIdx, "L1I caches", "L1I", len(gpu.L1ICaches),
		func(j int) (summarizer, bool) {
			c, ok := gpu.L1ICaches[j].(*l1v.Cache)
			return c, ok
		})

	// L1V TLBs (per CU)
	dumpGroup(gpuIdx, "L1V TLBs", "L1VTLB", len(gpu.L1VTLBs),
		func(j int) (summarizer, bool) { return gpu.L1VTLBs[j], true })

	// L1S TLBs (per SA)
	dumpGroup(gpuIdx, "L1S TLBs", "L1STLB", len(gpu.L1STLBs),
		func(j int) (summarizer, bool) { return gpu.L1STLBs[j], true })

	// L1I TLBs (per SA)
	dumpGroup(gpuIdx, "L1I TLBs", "L1ITLB", len(gpu.L1ITLBs),
		func(j int) (summarizer, bool) { return gpu.L1ITLBs[j], true })

	// L1V address translators (per CU)
	dumpGroup(gpuIdx, "L1V ATs", "L1VAT", len(gpu.L1VAddrTranslator),
		func(j int) (summarizer, bool) {
			at, ok := gpu.L1VAddrTranslator[j].(*addresstranslator.DefaultAddressTranslator)
			if ok {
				return at, true
			}
			iat, ok := gpu.L1VAddrTranslator[j].(*addresstranslator.IdealAddressTranslator)
			return iat, ok
		})

	// L1S address translators
	dumpGroup(gpuIdx, "L1S ATs", "L1SAT", len(gpu.L1SAddrTranslator),
		func(j int) (summarizer, bool) {
			at, ok := gpu.L1SAddrTranslator[j].(*addresstranslator.DefaultAddressTranslator)
			if ok {
				return at, true
			}
			iat, ok := gpu.L1SAddrTranslator[j].(*addresstranslator.IdealAddressTranslator)
			return iat, ok
		})

	// L1I address translators
	dumpGroup(gpuIdx, "L1I ATs", "L1IAT", len(gpu.L1IAddrTranslator),
		func(j int) (summarizer, bool) {
			at, ok := gpu.L1IAddrTranslator[j].(*addresstranslator.DefaultAddressTranslator)
			if ok {
				return at, true
			}
			iat, ok := gpu.L1IAddrTranslator[j].(*addresstranslator.IdealAddressTranslator)
			return iat, ok
		})

	// DRAM controllers
	dumpGroup(gpuIdx, "DRAM controllers", "DRAM", len(gpu.MemoryControllers),
		func(j int) (summarizer, bool) {
			return gpu.MemoryControllers[j], true
		})

	// Compute Units
	dumpGroup(gpuIdx, "Compute Units", "CU", len(gpu.CUs),
		func(j int) (summarizer, bool) {
			c, ok := gpu.CUs[j].(*cu.ComputeUnit)
			return c, ok
		})

	// Command Processor (single)
	if gpu.CommandProcessor != nil {
		line, any := gpu.CommandProcessor.DeadlockSummary()
		if any {
			fmt.Printf("[deadlock] GPU%d CommandProcessor %s | %s\n",
				gpuIdx, gpu.CommandProcessor.Name(), line)
		} else {
			fmt.Printf("[deadlock] GPU%d CommandProcessor: clean\n", gpuIdx)
		}
	}

	// PMC (single)
	if gpu.PMC != nil {
		line, any := gpu.PMC.DeadlockSummary()
		if any {
			fmt.Printf("[deadlock] GPU%d PMC %s | %s\n",
				gpuIdx, gpu.PMC.Name(), line)
		} else {
			fmt.Printf("[deadlock] GPU%d PMC: clean\n", gpuIdx)
		}
	}

	// Inter-chiplet network switches — walk every connector × every switch
	for netIdx, netConn := range gpu.InterChipletNetwork {
		if netConn == nil {
			continue
		}
		grid := netConn.GetChipSwitches()
		stuck := 0
		total := 0
		for r := range grid {
			for c := range grid[r] {
				total++
				sw, ok := grid[r][c].(*switching.MultiFlitChipletSwitchImpl)
				if !ok {
					continue
				}
				line, any := sw.DeadlockSummary()
				if any {
					if stuck == 0 {
						fmt.Printf("[deadlock] GPU%d net[%d] switches with pending state:\n",
							gpuIdx, netIdx)
					}
					fmt.Printf("[deadlock]   SW[%d][%d] %s | %s\n",
						r, c, sw.Name(), line)
					stuck++
				}
			}
		}
		if stuck == 0 {
			fmt.Printf("[deadlock] GPU%d net[%d] switches: all clean (%d total)\n",
				gpuIdx, netIdx, total)
		} else {
			fmt.Printf("[deadlock] GPU%d net[%d] switches: %d/%d with pending\n",
				gpuIdx, netIdx, stuck, total)
		}
	}

	// Inter-switch wires (pipelined shift register + postBuf between two
	// adjacent switches). Flits stuck mid-pipeline don't show up in either
	// endpoint switch's local buffers.
	stuck := 0
	for j, conn := range gpu.SwitchConnection {
		if conn == nil {
			continue
		}
		line, any := conn.DeadlockSummary()
		if any {
			if stuck == 0 {
				fmt.Printf("[deadlock] GPU%d switch connections with pending state:\n", gpuIdx)
			}
			lpName, rpName := "?", "?"
			if conn.LeftPort() != nil {
				lpName = conn.LeftPort().Name()
			}
			if conn.RightPort() != nil {
				rpName = conn.RightPort().Name()
			}
			fmt.Printf("[deadlock]   Wire[%d] %s <-> %s | %s\n", j, lpName, rpName, line)
			stuck++
		}
	}
	if stuck == 0 {
		fmt.Printf("[deadlock] GPU%d switch connections: all clean (%d total)\n",
			gpuIdx, len(gpu.SwitchConnection))
	} else {
		fmt.Printf("[deadlock] GPU%d switch connections: %d/%d with pending\n",
			gpuIdx, stuck, len(gpu.SwitchConnection))
	}

	// EndPoints: msg↔flit converters between each device port and its switch.
	// req/rsp use separate endpoints, so a stuck egress/ingress here is invisible
	// in both the switch and the device-side buffers.
	epStuck, epTotal := 0, 0
	for _, netConn := range gpu.InterChipletNetwork {
		if netConn == nil {
			continue
		}
		sc := netConn.GetSwitchConnector()
		if sc == nil {
			continue
		}
		for _, ep := range sc.GetEndPoints() {
			epTotal++
			line, any := ep.DeadlockSummary()
			if any {
				if epStuck == 0 {
					fmt.Printf("[deadlock] GPU%d endpoints with pending state:\n", gpuIdx)
				}
				fmt.Printf("[deadlock]   EP %s | %s\n", ep.Name(), line)
				epStuck++
			}
		}
	}
	if epStuck == 0 {
		fmt.Printf("[deadlock] GPU%d endpoints: all clean (%d total)\n", gpuIdx, epTotal)
	} else {
		fmt.Printf("[deadlock] GPU%d endpoints: %d/%d with pending\n",
			gpuIdx, epStuck, epTotal)
	}
}

// func (d *Driver) runEngine() {
// 	d.engineMutex.Lock()
// 	defer d.engineMutex.Unlock()

// 	err := d.Engine.Run()
// 	if err != nil {
// 		panic(err)
// 	}

// 	d.engineRunningMutex.Lock()
// 	d.engineRunning = false
// 	d.engineRunningMutex.Unlock()
// }

// RegisterGPU tells the driver about the existence of a GPU
func (d *Driver) RegisterGPU(gpu *mgpusim.GPU, dramSize uint64) {
	d.GPUs = append(d.GPUs, gpu)

	gpuDevice := &device.Device{
		ID:       len(d.GPUs),
		Type:     device.DeviceTypeGPU,
		MemState: device.NewDeviceMemoryState(d.Log2PageSize, d.memAllocatorType),
	}
	gpuDevice.SetTotalMemSize(dramSize)
	d.memAllocator.RegisterDevice(gpuDevice)

	d.devices = append(d.devices, gpuDevice)
}

// Tick ticks
func (d *Driver) Tick(now akita.VTimeInSec) bool {
	madeProgress := false

	madeProgress = d.sendToGPUs(now) || madeProgress
	madeProgress = d.sendToMMU(now) || madeProgress
	madeProgress = d.sendMigrationReqToCP(now) || madeProgress
	madeProgress = d.processReturnReq(now) || madeProgress
	madeProgress = d.processNewCommand(now) || madeProgress
	madeProgress = d.parseFromMMU(now) || madeProgress

	return madeProgress
}

func (d *Driver) sendToGPUs(now akita.VTimeInSec) bool {
	if len(d.requestsToSend) == 0 {
		return false
	}

	req := d.requestsToSend[0]
	req.Meta().SendTime = now
	err := d.ToGPUs.Send(req)
	if err == nil {
		d.requestsToSend = d.requestsToSend[1:]
		return true
	}

	return false
}

func (d *Driver) processReturnReq(now akita.VTimeInSec) bool {
	req := d.ToGPUs.Retrieve(now)
	if req == nil {
		return false
	}

	switch req := req.(type) {
	case *coreprotocol.MemCopyH2DReq:
		return d.processMemCopyH2DReturn(now, req)
	case *coreprotocol.MemCopyD2HReq:
		return d.processMemCopyD2HReturn(now, req)
	case *coreprotocol.LaunchKernelReq:
		return d.processLaunchKernelReturn(now, req)
	case *coreprotocol.FlushCommand:
		return d.processFlushReturn(now, req)
	case *coreprotocol.RDMADrainRspToDriver:
		return d.processRDMADrainRsp(now, req)
	case *coreprotocol.ShootDownCompleteRsp:
		return d.processShootdownCompleteRsp(now, req)
	case *coreprotocol.PageMigrationRspToDriver:
		return d.processPageMigrationRspFromCP(now, req)
	case *coreprotocol.RDMARestartRspToDriver:
		return d.processRDMARestartRspToDriver(now, req)
	case *coreprotocol.GPURestartRsp:
		return d.handleGPURestartRsp(now, req)
	default:
		log.Panicf("cannot handle request of type %s", reflect.TypeOf(req))
	}

	panic("never")
}

func (d *Driver) processNewCommand(now akita.VTimeInSec) bool {
	madeProgress := false

	d.contextMutex.Lock()
	for _, ctx := range d.contexts {
		madeProgress = d.processNewCommandFromContext(now, ctx) || madeProgress
	}
	d.contextMutex.Unlock()

	return madeProgress
}

func (d *Driver) processNewCommandFromContext(
	now akita.VTimeInSec,
	ctx *Context,
) bool {
	madeProgress := false
	ctx.queueMutex.Lock()
	for _, q := range ctx.queues {
		madeProgress = d.processNewCommandFromCmdQueue(now, q) || madeProgress
	}
	ctx.queueMutex.Unlock()

	return madeProgress
}

func (d *Driver) processNewCommandFromCmdQueue(
	now akita.VTimeInSec,
	q *CommandQueue,
) bool {
	if q.NumCommand() == 0 {
		return false
	}

	if q.IsRunning {
		return false
	}

	return d.processOneCommand(now, q)
}

func (d *Driver) processOneCommand(
	now akita.VTimeInSec,
	cmdQueue *CommandQueue,
) bool {
	cmd := cmdQueue.Peek()

	switch cmd := cmd.(type) {
	case *MemCopyH2DCommand:
		d.processMemCopyH2DCommand(now, cmd, cmdQueue)
	case *WritePageTableH2DCommand:
		d.processWritePageTableH2DCommand(now, cmd, cmdQueue)
	case *MemCopyD2HCommand:
		d.processMemCopyD2HCommand(now, cmd, cmdQueue)
	case *LaunchKernelCommand:
		d.processLaunchKernelCommand(now, cmd, cmdQueue)
	case *FlushCommand:
		d.processFlushCommand(now, cmd, cmdQueue)
	case *NoopCommand:
		d.processNoopCommand(now, cmd, cmdQueue)
	default:
		log.Panicf("cannot process command of type %s", reflect.TypeOf(cmd))
	}

	d.logCmdStart(cmd, now)
	return true
}

func (d *Driver) logCmdStart(cmd Command, now akita.VTimeInSec) {
	tracing.StartTask(
		tracing.DecimalTaskID(cmd.GetID()),
		tracing.DecimalTaskID(d.simulationID),
		now,
		d,
		"Driver Command",
		reflect.TypeOf(cmd).String(),
		nil,
	)
}

func (d *Driver) logCmdComplete(cmd Command, now akita.VTimeInSec) {
	tracing.EndTask(tracing.DecimalTaskID(cmd.GetID()), now, d)
}

func (d *Driver) processNoopCommand(
	now akita.VTimeInSec,
	cmd *NoopCommand,
	queue *CommandQueue,
) bool {
	queue.Dequeue()
	return true
}

func (d *Driver) processMemCopyH2DCommand(
	now akita.VTimeInSec,
	cmd *MemCopyH2DCommand,
	queue *CommandQueue,
) bool {
	buffer := bytes.NewBuffer(nil)
	err := binary.Write(buffer, binary.LittleEndian, cmd.Src)
	if err != nil {
		panic(err)
	}
	rawBytes := buffer.Bytes()

	offset := uint64(0)
	addr := uint64(cmd.Dst)
	sizeLeft := uint64(len(rawBytes))
	for sizeLeft > 0 {
		page, found := d.PageTable.Find(queue.Context.pid, addr)
		if !found {
			panic("page not found")
		}
		pAddr := page.PAddr + (addr - page.VAddr)
		sizeLeftInPage := page.PageSize - (addr - page.VAddr)
		sizeToCopy := sizeLeftInPage
		if sizeLeft < sizeLeftInPage {
			sizeToCopy = sizeLeft
		}

		gpuID := d.memAllocator.GetDeviceIDByPAddr(pAddr)
		if d.FastH2D {
			d.GPUs[gpuID-1].DirectWriteH2D(pAddr, rawBytes[offset:offset+sizeToCopy])
		} else {
			req := coreprotocol.NewMemCopyH2DReq(now,
				d.ToGPUs, d.GPUs[gpuID-1].CommandProcessor.ToDriver,
				rawBytes[offset:offset+sizeToCopy],
				pAddr)
			cmd.Reqs = append(cmd.Reqs, req)
			d.requestsToSend = append(d.requestsToSend, req)
			d.logTaskToGPUInitiate(now, cmd, req)
		}

		sizeLeft -= sizeToCopy
		addr += sizeToCopy
		offset += sizeToCopy
	}
	if len(cmd.Reqs) == 0 {
		queue.IsRunning = false
		queue.Dequeue()
		d.logCmdComplete(cmd, now)
	} else {
		queue.IsRunning = true
	}
	return true
}

func (d *Driver) processWritePageTableH2DCommand(
	now akita.VTimeInSec,
	cmd *WritePageTableH2DCommand,
	queue *CommandQueue,
) bool {
	pageTablePages := d.PageTable.PageTablePagesAsBytes(queue.Context.pid)
	for len(pageTablePages) > 0 {
		pAddr := pageTablePages[0]
		children := pageTablePages[1:513]
		pageTablePages = pageTablePages[513:]
		buffer := bytes.NewBuffer(nil)
		err := binary.Write(buffer, binary.LittleEndian, children)
		if err != nil {
			panic(err)
		}
		rawBytes := buffer.Bytes()
		gpuID := d.memAllocator.GetDeviceIDByPAddr(pAddr)
		if gpuID != 1 {
			panic("many gpus!")
		}
		if d.FastH2D {
			d.GPUs[gpuID-1].DirectWriteH2D(pAddr, rawBytes)
		} else {
			req := coreprotocol.NewMemCopyH2DReq(now,
				d.ToGPUs, d.GPUs[gpuID-1].CommandProcessor.ToDriver,
				rawBytes,
				pAddr)
			cmd.Reqs = append(cmd.Reqs, req)
			d.requestsToSend = append(d.requestsToSend, req)
			d.logTaskToGPUInitiate(now, cmd, req)
		}
	}
	if len(cmd.Reqs) == 0 {
		queue.IsRunning = false
		queue.Dequeue()
		d.logCmdComplete(cmd, now)
	} else {
		queue.IsRunning = true
	}
	return true
}

func (d *Driver) logTaskToGPUInitiate(
	now akita.VTimeInSec,
	cmd Command,
	req akita.Msg,
) {
	tracing.TraceReqInitiate(req, now, d, tracing.DecimalTaskID(cmd.GetID()))
}

func (d *Driver) logTaskToGPUClear(
	now akita.VTimeInSec,
	req akita.Msg,
) {
	tracing.TraceReqFinalize(req, now, d)
}

func (d *Driver) processMemCopyH2DReturn(
	now akita.VTimeInSec,
	req *coreprotocol.MemCopyH2DReq,
) bool {
	d.logTaskToGPUClear(now, req)

	cmd, cmdQueue := d.findCommandByReq(req)
	switch cmd.(type) {
	case *MemCopyH2DCommand:
		copyCmd := cmd.(*MemCopyH2DCommand)

		newReqs := make([]akita.Msg, 0, len(copyCmd.Reqs)-1)
		for _, r := range copyCmd.GetReqs() {
			if r != req {
				newReqs = append(newReqs, r)
			}
		}
		copyCmd.Reqs = newReqs

		if len(copyCmd.Reqs) == 0 {
			cmdQueue.IsRunning = false
			cmdQueue.Dequeue()

			d.logCmdComplete(cmd, now)
		}
	case *WritePageTableH2DCommand:
		copyCmd := cmd.(*WritePageTableH2DCommand)

		newReqs := make([]akita.Msg, 0, len(copyCmd.Reqs)-1)
		for _, r := range copyCmd.GetReqs() {
			if r != req {
				newReqs = append(newReqs, r)
			}
		}
		copyCmd.Reqs = newReqs

		if len(copyCmd.Reqs) == 0 {
			cmdQueue.IsRunning = false
			cmdQueue.Dequeue()

			d.logCmdComplete(cmd, now)
		}
	}

	return true
}

func (d *Driver) processMemCopyD2HCommand(
	now akita.VTimeInSec,
	cmd *MemCopyD2HCommand,
	queue *CommandQueue,
) bool {
	cmd.RawData = make([]byte, binary.Size(cmd.Dst))

	offset := uint64(0)
	addr := uint64(cmd.Src)
	sizeLeft := uint64(len(cmd.RawData))
	for sizeLeft > 0 {
		page, found := d.PageTable.Find(queue.Context.pid, addr)
		if !found {
			panic("page not found")
		}
		pAddr := page.PAddr + (addr - page.VAddr)
		sizeLeftInPage := page.PageSize - (addr - page.VAddr)
		sizeToCopy := sizeLeftInPage
		if sizeLeft < sizeLeftInPage {
			sizeToCopy = sizeLeft
		}

		gpuID := d.memAllocator.GetDeviceIDByPAddr(pAddr)
		req := coreprotocol.NewMemCopyD2HReq(
			now,
			d.ToGPUs,
			d.GPUs[gpuID-1].CommandProcessor.ToDriver,
			pAddr,
			cmd.RawData[offset:offset+sizeToCopy])
		cmd.Reqs = append(cmd.Reqs, req)
		d.requestsToSend = append(d.requestsToSend, req)

		sizeLeft -= sizeToCopy
		addr += sizeToCopy
		offset += sizeToCopy

		d.logTaskToGPUInitiate(now, cmd, req)
	}

	queue.IsRunning = true
	return true
}

func (d *Driver) processMemCopyD2HReturn(
	now akita.VTimeInSec,
	req *coreprotocol.MemCopyD2HReq,
) bool {
	d.logTaskToGPUClear(now, req)

	cmd, cmdQueue := d.findCommandByReq(req)

	copyCmd := cmd.(*MemCopyD2HCommand)
	newReqs := make([]akita.Msg, 0, len(copyCmd.Reqs)-1)
	for _, r := range copyCmd.GetReqs() {
		if r != req {
			newReqs = append(newReqs, r)
		}
	}
	copyCmd.Reqs = newReqs

	if len(copyCmd.Reqs) == 0 {
		cmdQueue.IsRunning = false
		buf := bytes.NewReader(copyCmd.RawData)
		err := binary.Read(buf, binary.LittleEndian, copyCmd.Dst)
		if err != nil {
			panic(err)
		}

		cmdQueue.Dequeue()

		d.logCmdComplete(copyCmd, now)
	}

	return true
}

func (d *Driver) processLaunchKernelCommand(
	now akita.VTimeInSec,
	cmd *LaunchKernelCommand,
	queue *CommandQueue,
) bool {
	tracing.StartTask(
		tracing.DecimalTaskID(cmd.GetID()),
		0,
		now,
		d,
		"kernel-trace",
		"launch",
		cmd.Packet.KernelObject)

	l := global.GetInstance()
	l.PrintDataLocation()

	dev := d.devices[queue.GPUID]
	if dev.Type == device.DeviceTypeUnifiedGPU {
		return d.processUnifiedMultiGPULaunchKernelCommand(now, cmd, queue)
	}

	req := coreprotocol.NewLaunchKernelReq(now,
		d.ToGPUs, d.GPUs[queue.GPUID-1].CommandProcessor.ToDriver)
	req.PID = queue.Context.pid
	req.HsaCo = cmd.CodeObject

	req.Packet = cmd.Packet
	req.PacketAddress = uint64(cmd.DPacket)

	queue.IsRunning = true
	cmd.Reqs = append(cmd.Reqs, req)

	d.requestsToSend = append(d.requestsToSend, req)

	queue.Context.l2Dirty = true

	d.logCmdStart(cmd, now)
	d.logTaskToGPUInitiate(now, cmd, req)

	return true
}

func (d *Driver) processUnifiedMultiGPULaunchKernelCommand(
	now akita.VTimeInSec,
	cmd *LaunchKernelCommand,
	queue *CommandQueue,
) bool {
	dev := d.devices[queue.GPUID]
	d.logCmdStart(cmd, now)

	for i, gpuID := range dev.UnifiedGPUIDs {
		req := coreprotocol.NewLaunchKernelReq(now,
			d.ToGPUs, d.GPUs[gpuID-1].CommandProcessor.ToDriver)
		req.PID = queue.Context.pid
		req.HsaCo = cmd.CodeObject
		req.Packet = cmd.Packet
		req.PacketAddress = uint64(cmd.DPacket)

		numGPUs := len(dev.UnifiedGPUIDs)
		currentGPUIndex := i
		req.WGFilter = func(
			pkt *kernels.HsaKernelDispatchPacket,
			wg *kernels.WorkGroup,
		) bool {
			numWGX := (pkt.GridSizeX-1)/uint32(pkt.WorkgroupSizeX) + 1
			numWGY := (pkt.GridSizeY-1)/uint32(pkt.WorkgroupSizeY) + 1
			flattenedID :=
				wg.IDZ*int(numWGX)*int(numWGY) +
					wg.IDY*int(numWGX) +
					wg.IDX
			return flattenedID%numGPUs == currentGPUIndex
		}

		queue.IsRunning = true
		cmd.Reqs = append(cmd.Reqs, req)

		d.requestsToSend = append(d.requestsToSend, req)

		queue.Context.l2Dirty = true

		d.logTaskToGPUInitiate(now, cmd, req)
	}

	return true
}

func (d *Driver) processLaunchKernelReturn(
	now akita.VTimeInSec,
	req *coreprotocol.LaunchKernelReq,
) bool {
	cmd, cmdQueue := d.findCommandByReq(req)
	cmd.RemoveReq(req)

	d.logTaskToGPUClear(now, req)

	if len(cmd.GetReqs()) == 0 {
		cmdQueue.IsRunning = false
		cmdQueue.Dequeue()

		d.logCmdComplete(cmd, now)
	}

	tracing.StartTask(
		tracing.DecimalTaskID(cmd.GetID()),
		0,
		now,
		d,
		"kernel-trace",
		"terminate",
		nil)

	return true
}

func (d *Driver) processFlushCommand(
	now akita.VTimeInSec,
	cmd *FlushCommand,
	queue *CommandQueue,
) bool {
	for _, gpu := range d.GPUs {
		req := coreprotocol.NewFlushCommand(now,
			d.ToGPUs, gpu.CommandProcessor.ToDriver)
		d.requestsToSend = append(d.requestsToSend, req)
		cmd.Reqs = append(cmd.Reqs, req)
		d.logTaskToGPUInitiate(now, cmd, req)
	}

	queue.IsRunning = true

	d.logCmdStart(cmd, now)

	tracing.StartTask(
		tracing.DecimalTaskID(cmd.ID),
		0,
		now,
		d,
		"flushing",
		"",
		nil,
	)

	return true
}

func (d *Driver) processFlushReturn(
	now akita.VTimeInSec,
	req *coreprotocol.FlushCommand,
) bool {
	d.logTaskToGPUClear(now, req)

	cmd, cmdQueue := d.findCommandByReq(req)
	flushCmd := cmd.(*FlushCommand)

	flushCmd.RemoveReq(req)

	if len(flushCmd.Reqs) > 0 {
		return true
	}

	cmdQueue.IsRunning = false
	cmdQueue.Dequeue()

	cmdQueue.Context.l2Dirty = false

	d.logCmdComplete(cmd, now)

	tracing.EndTask(
		tracing.DecimalTaskID(cmd.GetID()),
		now,
		d,
	)

	return true
}

func (d *Driver) findCommandByReq(req akita.Msg) (Command, *CommandQueue) {
	d.contextMutex.Lock()
	defer d.contextMutex.Unlock()

	for _, ctx := range d.contexts {
		ctx.queueMutex.Lock()
		for _, q := range ctx.queues {
			cmd := q.Peek()
			if cmd == nil {
				continue
			}

			reqs := cmd.GetReqs()
			for _, r := range reqs {
				if r == req {
					ctx.queueMutex.Unlock()
					return cmd, q
				}
			}
		}
		ctx.queueMutex.Unlock()
	}
	panic("cannot find command")
}

func (d *Driver) createCPU() {
	cpu := &device.Device{
		ID:       0,
		Type:     device.DeviceTypeCPU,
		MemState: device.NewDeviceMemoryState(d.Log2PageSize, d.memAllocatorType),
	}
	cpu.SetTotalMemSize(2 * mem.GB)

	d.memAllocator.RegisterDevice(cpu)
	d.devices = append(d.devices, cpu)
}

func (d *Driver) parseFromMMU(now akita.VTimeInSec) bool {
	if d.isCurrentlyHandlingMigrationReq {
		return false
	}

	req := d.ToMMU.Retrieve(now)
	if req == nil {
		return false
	}

	switch req := req.(type) {
	case *device.PageMigrationReqToDriver:
		d.currentPageMigrationReq = req
		d.isCurrentlyHandlingMigrationReq = true
		d.initiateRDMADrain(now)
	default:
		log.Panicf("Driver canot handle request of type %s",
			reflect.TypeOf(req))
	}

	return true
}

func (d *Driver) initiateRDMADrain(now akita.VTimeInSec) bool {
	for i := 0; i < len(d.GPUs); i++ {
		req := coreprotocol.NewRDMADrainCmdFromDriver(now, d.ToGPUs,
			d.GPUs[i].CommandProcessor.ToDriver)
		d.requestsToSend = append(d.requestsToSend, req)
		d.numRDMADrainACK++
	}

	return true
}

func (d *Driver) processRDMADrainRsp(
	now akita.VTimeInSec,
	req *coreprotocol.RDMADrainRspToDriver,
) bool {
	d.numRDMADrainACK--

	if d.numRDMADrainACK == 0 {
		d.sendShootDownReqs(now)
	}

	return true
}

func (d *Driver) sendShootDownReqs(now akita.VTimeInSec) bool {
	vAddr := make([]uint64, 0)
	migrationInfo := d.currentPageMigrationReq.MigrationInfo

	numReqsGPUInMap := 0
	for i := 1; i < d.GetNumGPUs()+1; i++ {
		pages, found := migrationInfo.GPUReqToVAddrMap[uint64(i)]

		if found {
			numReqsGPUInMap++
			for j := 0; j < len(pages); j++ {
				vAddr = append(vAddr, pages[j])
			}
		}
	}

	accesingGPUs := d.currentPageMigrationReq.CurrAccessingGPUs
	pid := d.currentPageMigrationReq.PID
	d.numShootDownACK = uint64(len(accesingGPUs))

	for i := 0; i < len(accesingGPUs); i++ {
		toShootdownGPU := accesingGPUs[i] - 1
		shootDwnReq := coreprotocol.NewShootdownCommand(
			now,
			d.ToGPUs, d.GPUs[toShootdownGPU].CommandProcessor.ToDriver,
			vAddr, pid)
		d.requestsToSend = append(d.requestsToSend, shootDwnReq)
	}

	return true
}

func (d *Driver) processShootdownCompleteRsp(
	now akita.VTimeInSec,
	req *coreprotocol.ShootDownCompleteRsp,
) bool {
	d.numShootDownACK--

	if d.numShootDownACK == 0 {
		toRequestFromGPU := d.currentPageMigrationReq.CurrPageHostGPU
		toRequestFromPMCPort := d.RemotePMCPorts[toRequestFromGPU-1]

		migrationInfo := d.currentPageMigrationReq.MigrationInfo

		requestingGPUs := d.findRequestingGPUs(migrationInfo)
		context := d.findContext(d.currentPageMigrationReq.PID)

		pageVaddrs := make(map[uint64][]uint64)

		for i := 0; i < len(requestingGPUs); i++ {
			pageVaddrs[requestingGPUs[i]] =
				migrationInfo.GPUReqToVAddrMap[requestingGPUs[i]+1]
		}

		for gpuID, vAddrs := range pageVaddrs {
			for i := 0; i < len(vAddrs); i++ {
				vAddr := vAddrs[i]
				page, oldPaddr :=
					d.preparePageForMigration(vAddr, context, gpuID)

				req := coreprotocol.NewPageMigrationReqToCP(now, d.ToGPUs,
					d.GPUs[gpuID].CommandProcessor.ToDriver)
				req.DestinationPMCPort = toRequestFromPMCPort
				req.ToReadFromPhysicalAddress = oldPaddr
				req.ToWriteToPhysicalAddress = page.PAddr
				req.PageSize = d.currentPageMigrationReq.PageSize

				d.migrationReqToSendToCP = append(d.migrationReqToSendToCP, req)
				d.numPagesMigratingACK++
			}
		}
		return true
	}

	return false
}

func (d *Driver) findRequestingGPUs(
	migrationInfo *device.PageMigrationInfo,
) []uint64 {
	requestingGPUs := make([]uint64, 0)

	for i := 1; i < d.GetNumGPUs()+1; i++ {
		_, found := migrationInfo.GPUReqToVAddrMap[uint64(i)]
		if found {
			requestingGPUs = append(requestingGPUs, uint64(i-1))
		}
	}
	return requestingGPUs
}

func (d *Driver) findContext(pid ca.PID) *Context {
	context := &Context{}
	for i := 0; i < len(d.contexts); i++ {
		if d.contexts[i].pid == d.currentPageMigrationReq.PID {
			context = d.contexts[i]
		}
	}
	if context == nil {
		log.Panicf("Process does not exist")
	}
	return context
}

func (d *Driver) preparePageForMigration(
	vAddr uint64,
	context *Context,
	gpuID uint64,
) (*device.Page, uint64) {
	page, found := d.PageTable.Find(context.pid, vAddr)
	if !found {
		panic("page not founds")
	}
	oldPaddr := page.PAddr

	newPage := d.memAllocator.AllocatePageWithGivenVAddr(
		context.pid, int(gpuID+1), vAddr, true)
	newPage.DeviceID = gpuID + 1

	// newPage.IsMigrating = true
	d.PageTable.Update(newPage)

	return &newPage, oldPaddr
}

func (d *Driver) sendMigrationReqToCP(now akita.VTimeInSec) bool {
	if len(d.migrationReqToSendToCP) == 0 {
		return false
	}

	if d.isCurrentlyMigratingOnePage {
		return false
	}

	req := d.migrationReqToSendToCP[0]
	req.SendTime = now

	err := d.ToGPUs.Send(req)
	if err == nil {
		d.migrationReqToSendToCP = d.migrationReqToSendToCP[1:]
		d.isCurrentlyMigratingOnePage = true
		return true
	}

	return false
}

func (d *Driver) processPageMigrationRspFromCP(
	now akita.VTimeInSec,
	rsp *coreprotocol.PageMigrationRspToDriver,
) bool {
	d.numPagesMigratingACK--
	d.isCurrentlyMigratingOnePage = false

	if d.numPagesMigratingACK == 0 {
		d.prepareGPURestartReqs(now)
		d.preparePageMigrationRspToMMU(now)
	}

	return true
}

func (d *Driver) prepareGPURestartReqs(now akita.VTimeInSec) {
	accessingGPUs := d.currentPageMigrationReq.CurrAccessingGPUs

	for i := 0; i < len(accessingGPUs); i++ {
		restartGPUID := accessingGPUs[i] - 1
		restartReq := coreprotocol.NewGPURestartReq(
			now, d.ToGPUs, d.GPUs[restartGPUID].CommandProcessor.ToDriver)
		d.requestsToSend = append(d.requestsToSend, restartReq)
		d.numRestartACK++
	}
}

func (d *Driver) preparePageMigrationRspToMMU(now akita.VTimeInSec) {
	requestingGPUs := make([]uint64, 0)

	migrationInfo := d.currentPageMigrationReq.MigrationInfo

	for i := 1; i < d.GetNumGPUs()+1; i++ {
		_, found := migrationInfo.GPUReqToVAddrMap[uint64(i)]
		if found {
			requestingGPUs = append(requestingGPUs, uint64(i-1))
		}
	}

	pageVaddrs := make(map[uint64][]uint64)

	for i := 0; i < len(requestingGPUs); i++ {
		pageVaddrs[requestingGPUs[i]] = migrationInfo.GPUReqToVAddrMap[requestingGPUs[i]+1]
	}

	req := device.NewPageMigrationRspFromDriver(now, d.ToMMU, d.currentPageMigrationReq.Src)

	for _, vAddrs := range pageVaddrs {
		for j := 0; j < len(vAddrs); j++ {
			req.VAddr = append(req.VAddr, vAddrs[j])
		}
	}
	req.RspToTop = d.currentPageMigrationReq.RespondToTop
	d.toSendToMMU = req
}

func (d *Driver) handleGPURestartRsp(
	now akita.VTimeInSec,
	req *coreprotocol.GPURestartRsp,
) bool {
	d.numRestartACK--
	if d.numRestartACK == 0 {
		d.prepareRDMARestartReqs(now)
	}
	return true
}

func (d *Driver) prepareRDMARestartReqs(now akita.VTimeInSec) {
	for i := 0; i < len(d.GPUs); i++ {
		req := coreprotocol.NewRDMARestartCmdFromDriver(now, d.ToGPUs,
			d.GPUs[i].CommandProcessor.ToDriver)
		d.requestsToSend = append(d.requestsToSend, req)
		d.numRDMARestartACK++
	}
}

func (d *Driver) processRDMARestartRspToDriver(
	now akita.VTimeInSec,
	rsp *coreprotocol.RDMARestartRspToDriver) bool {
	d.numRDMARestartACK--

	if d.numRDMARestartACK == 0 {
		d.currentPageMigrationReq = nil
		d.isCurrentlyHandlingMigrationReq = false
		return true
	}
	return true
}

func (d *Driver) sendToMMU(now akita.VTimeInSec) bool {
	if d.toSendToMMU == nil {
		return false
	}
	req := d.toSendToMMU
	req.SendTime = now
	err := d.ToMMU.Send(req)
	if err == nil {
		d.toSendToMMU = nil
		return true
	}

	return false
}

// NewDriver creates a new driver
func NewDriver(
	engine akita.Engine,
	log2PageSize uint64,
	memAllocatorType string,
	numChiplets uint64,
	dataPlacementPartition string,
	dataPlacementStride int,
) *Driver {
	driver := new(Driver)
	driver.TickingComponent = akita.NewTickingComponent(
		"driver", engine, 1*akita.GHz, driver)

	driver.Log2PageSize = log2PageSize

	pageTableImpl := device.NewPageTable(log2PageSize)

	memAllocatorImpl := device.NewMemoryAllocator(
		pageTableImpl,
		log2PageSize,
		numChiplets,
		dataPlacementPartition)

	pageTableImpl.SetMemoryAllocator(memAllocatorImpl)
	driver.memAllocator = memAllocatorImpl
	driver.memAllocatorType = memAllocatorType
	driver.PageTable = pageTableImpl

	distributorImpl := newDistributorImpl(memAllocatorImpl)
	distributorImpl.pageSizeAsPowerOf2 = log2PageSize
	driver.distributor = distributorImpl

	driver.ToGPUs = akita.NewLimitNumMsgPort(driver, 40960000, "driver.ToGPUs")
	driver.ToMMU = akita.NewLimitNumMsgPort(driver, 1, "driver.ToMMU")

	driver.enqueueSignal = make(chan bool)
	driver.driverStopped = make(chan bool)

	driver.createCPU()

	return driver
}

func (d *Driver) GetMemAllocator() device.MemoryAllocator {
	return d.memAllocator
}
