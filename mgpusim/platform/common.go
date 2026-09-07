package platform

import (
	"fmt"
	"log"
	"os"

	"gitlab.com/akita/lmf"
	memtraces "gitlab.com/akita/mem/trace"
	"gitlab.com/akita/mgpusim"

	"gitlab.com/akita/akita"
	"gitlab.com/akita/mem"
	"gitlab.com/akita/mgpusim/builders"
	"gitlab.com/akita/mgpusim/driver"
	"gitlab.com/akita/noc/networking/pcie"
	"gitlab.com/akita/util/tracing"
)

type Builder interface {
	Build() (akita.Engine, *driver.Driver)
	WithISADebugging()
	WithoutProgressBar()
	WithGridSize(int, int)
	WithNumReqPerCycle(int)
	WithSwitchLatency(int)
	WithTotalMemSize(uint64)
	WithMemAllocatorType(string)
	WithAlg(string)
	WithSchedulingPartition(string)
	WithDataPlacementPartition(string)
	WithDataPlacementStride(int)
	WithLog2PageSize(uint64)
	WithPteCaching(bool)
	WithPC(bool)

	// WithEngine(akita.Engine)
	// WithNumCUPerShaderArray(uint64)
	// WithNumShaderArrayPerChiplet(uint64)
	// WithNumMemoryBankPerChiplet(uint64)
	// WithNumChiplets(uint64)
	// WithTotalMem(uint64)
	// CalculateMemoryParameters()
	// WithLog2PageSize(uint64)
	// WithPageTable(*device.PageTableImpl)
	// WithAlg(string)
	// WithSchedulingPartition(string)
}

// CommonPlatformBuilder can build a platform that equips DisTLBGPU GPU.
type CommonPlatformBuilder struct {
	useParallelEngine        bool
	debugISA                 bool
	traceVis                 bool
	traceTLB                 bool
	tlbTraceFilename         string
	traceMem                 bool
	memTraceFilename         string
	numGPU                   int
	log2PageSize             uint64
	disableProgressBar       bool
	numChiplets              uint64
	numCUPerShaderArray      uint64
	numShaderArrayPerChiplet uint64
	numMemoryBankPerChiplet  uint64
	numReqPerCycle           int
	totalMem                 uint64
	bankSize                 uint64
	lowAddr                  uint64
	alg                      string
	schedPartition           string
	dataPlacementPartition   string
	dataPlacementStride      int
	useCoalescingTLBPort     bool
	useCoalescingRTU         bool
	pteCaching               bool
	memAllocatorType         string
	customHSLpmdUnits        uint64

	switchLatency int

	reqWithPC bool

	gridSize [2]int
}

// Makebuilder creates a EmuBuilder with default parameters.
func MakeCommonPlatformBuilder() CommonPlatformBuilder {
	b := CommonPlatformBuilder{
		numGPU:                   1,
		log2PageSize:             uint64(12),
		numCUPerShaderArray:      uint64(4),
		numShaderArrayPerChiplet: uint64(8),
		numMemoryBankPerChiplet:  uint64(32),
		numChiplets:              uint64(4), // just a default value, will be changed at WithNumChiplets
		totalMem:                 16 * mem.GB,
		bankSize:                 256 * mem.MB,
		lowAddr:                  4 * mem.GB,
	}
	return b
}

// WithParallelEngine lets the EmuBuilder to use parallel engine.
func (b *CommonPlatformBuilder) WithParallelEngine() {
	b.useParallelEngine = true
}

// WithISADebugging enables ISA debugging in the simulation.
func (b *CommonPlatformBuilder) WithISADebugging() {
	b.debugISA = true
}

// WithVisTracing lets the platform to record traces for visualization purposes.
func (b *CommonPlatformBuilder) WithVisTracing() {
	b.traceVis = true
}

// WithTLBTracing lets the platform to trace memory operations.
func (b *CommonPlatformBuilder) WithTLBTracing() {
	b.traceTLB = true
}

// WithTLBTraceFilename lets the platform to set memory trace file name.
func (b *CommonPlatformBuilder) WithTLBTraceFilename(tlbTraceFilename string) {
	b.tlbTraceFilename = tlbTraceFilename
}

// WithMemTracing lets the platform to trace memory operations.
func (b *CommonPlatformBuilder) WithMemTracing() {
	b.traceMem = true
}

// WithMemTraceFilename lets the platform to set memory trace file name.
func (b *CommonPlatformBuilder) WithMemTraceFilename(memTraceFilename string) {
	b.memTraceFilename = memTraceFilename
}

// WithNumGPU sets the number of GPUs to build.
func (b *CommonPlatformBuilder) WithNumGPU(n int) {
	b.numGPU = n
}

// WithoutProgressBar disables the progress bar for kernel execution
func (b *CommonPlatformBuilder) WithoutProgressBar() {
	b.disableProgressBar = true
}

// WithLog2PageSize sets the page size as a power of 2.
func (b *CommonPlatformBuilder) WithLog2PageSize(n uint64) {
	b.log2PageSize = n
}

func (b *CommonPlatformBuilder) WithAlg(alg string) {
	b.alg = alg
}

func (b *CommonPlatformBuilder) WithDataPlacementStride(stride int) {
	b.dataPlacementStride = stride
}

func (b *CommonPlatformBuilder) WithDataPlacementPartition(partition string) {
	b.dataPlacementPartition = partition
}

func (b *CommonPlatformBuilder) WithSchedulingPartition(partition string) {
	b.schedPartition = partition
}

func (b *CommonPlatformBuilder) UseCoalescingTLBPort(u bool) {
	b.useCoalescingTLBPort = u
}

func (b *CommonPlatformBuilder) UseCoalescingRTU(u bool) {
	b.useCoalescingRTU = u
}

func (b *CommonPlatformBuilder) WithMemAllocatorType(allocatorType string) {
	b.memAllocatorType = allocatorType
}

func (b *CommonPlatformBuilder) WithCustomHSL(pmdUnits uint64) {
	b.customHSLpmdUnits = pmdUnits
}

// WithNumChiplets sets the number of chiplets in the GPU.
func (b *CommonPlatformBuilder) WithNumChiplets(n uint64) {
	b.numChiplets = n
}

func (b *CommonPlatformBuilder) WithNumReqPerCycle(n int) {
	b.numReqPerCycle = n
}

func (b *CommonPlatformBuilder) WithSwitchLatency(n int) {
	b.switchLatency = n
}

func (b *CommonPlatformBuilder) WithPteCaching(pteCaching bool) {
	b.pteCaching = pteCaching
}

func (b *CommonPlatformBuilder) WithPC(p bool) {
	b.reqWithPC = p
}

func (b *CommonPlatformBuilder) WithGridSize(xDim, yDim int) {
	b.gridSize[0] = xDim
	b.gridSize[1] = yDim
	b.numChiplets = uint64(xDim * yDim)
}

func (b *CommonPlatformBuilder) WithTotalMemSize(t uint64) {
	b.totalMem = t
}

func (b CommonPlatformBuilder) createGPUs(
	rootComplexID int,
	pcieConnector *pcie.Connector,
	gpuBuilder builders.Builder,
	gpuDriver *driver.Driver,
	rdmaAddressTable *lmf.BankedLowModuleFinder,
	pmcAddressTable *lmf.BankedLowModuleFinder,
) {
	lastSwitchID := rootComplexID
	for i := 1; i < b.numGPU+1; i++ {
		if i%2 == 1 {
			lastSwitchID = pcieConnector.AddSwitch(lastSwitchID)
		}

		b.createGPU(i, gpuBuilder, gpuDriver,
			rdmaAddressTable, pmcAddressTable,
			pcieConnector, lastSwitchID)
	}
}

func (b CommonPlatformBuilder) createPMCPageTable() *lmf.BankedLowModuleFinder {
	pmcAddressTable := new(lmf.BankedLowModuleFinder)
	pmcAddressTable.BankSize = 4 * mem.GB
	pmcAddressTable.LowModules = append(pmcAddressTable.LowModules, nil)
	return pmcAddressTable
}

func (b CommonPlatformBuilder) createRDMAAddrTable() *lmf.BankedLowModuleFinder {
	rdmaAddressTable := new(lmf.BankedLowModuleFinder)
	rdmaAddressTable.BankSize = 4 * mem.GB
	rdmaAddressTable.LowModules = append(rdmaAddressTable.LowModules, nil)
	return rdmaAddressTable
}

func (b CommonPlatformBuilder) createConnection(
	engine akita.Engine,
	gpuDriver *driver.Driver,
) (*pcie.Connector, int) {
	//connection := akita.NewDirectConnection(engine)
	// connection := noc.NewFixedBandwidthConnection(32, engine, 1*akita.GHz)
	// connection.SrcBufferCapacity = 40960000
	pcieConnector := pcie.NewConnector().
		WithEngine(engine).
		WithVersion3().
		WithX16().
		WithSwitchLatency(140).
		WithNetworkName("PCIe")
	pcieConnector.CreateNetwork()
	rootComplexID := pcieConnector.CreateRootComplex(
		[]akita.Port{
			gpuDriver.ToGPUs,
			gpuDriver.ToMMU,
			// mmuComponent.MigrationPort,
			// mmuComponent.ToTop,
		})
	return pcieConnector, rootComplexID
}

func (b CommonPlatformBuilder) createEngine() akita.Engine {
	var engine akita.Engine

	if b.useParallelEngine {
		engine = akita.NewParallelEngine()
	} else {
		engine = akita.NewSerialEngine()
	}
	// engine.AcceptHook(akita.NewEventLogger(log.New(os.Stdout, "", 0)))

	return engine
}

func (b *CommonPlatformBuilder) setISADebugger(
	gpuBuilder builders.Builder,
) builders.Builder {
	if !b.debugISA {
		return gpuBuilder
	}

	gpuBuilder.WithISADebugging()
	return gpuBuilder
}

func (b *CommonPlatformBuilder) setTLBTracer(
	gpuBuilder builders.Builder,
) builders.Builder {
	if !b.traceTLB {
		return gpuBuilder
	}

	file, err := os.Create(b.tlbTraceFilename)
	if err != nil {
		panic(err)
	}
	logger := log.New(file, "", 0)
	tlbTracer := memtraces.NewTracer(logger)
	gpuBuilder.WithTLBTracer(tlbTracer)
	return gpuBuilder
}

func (b *CommonPlatformBuilder) setMemTracer(
	gpuBuilder builders.Builder,
) builders.Builder {
	if !b.traceMem {
		return gpuBuilder
	}

	file, err := os.Create(b.memTraceFilename)
	if err != nil {
		panic(err)
	}
	logger := log.New(file, "", 0)
	memTracer := memtraces.NewTracer(logger)
	gpuBuilder.WithMemTracer(memTracer)
	return gpuBuilder
}

func (b *CommonPlatformBuilder) setVisTracer(
	gpuDriver *driver.Driver,
	gpuBuilder builders.Builder,
) builders.Builder {
	if !b.traceVis {
		return gpuBuilder
	}

	tracer := tracing.NewMySQLTracer()
	tracer.Init()
	tracing.CollectTrace(gpuDriver, tracer)

	gpuBuilder.WithVisTracer(tracer)
	return gpuBuilder
}

func (b *CommonPlatformBuilder) createGPU(
	index int,
	gpuBuilder builders.Builder,
	gpuDriver *driver.Driver,
	rdmaAddressTable *lmf.BankedLowModuleFinder,
	pmcAddressTable *lmf.BankedLowModuleFinder,
	pcieConnector *pcie.Connector,
	pcieSwitchID int,
) *mgpusim.GPU {
	name := fmt.Sprintf("GPU%d", index)
	fmt.Println("platform, common.go, ", index, b.log2PageSize)
	memAddrOffset := uint64(index)*2*mem.GB + uint64(1<<b.log2PageSize)
	gpuBuilder.WithMemAddrOffset(memAddrOffset)
	gpu := gpuBuilder.Build(name, uint64(index))
	gpuDriver.RegisterGPU(gpu, b.totalMem)
	gpu.CommandProcessor.Driver = gpuDriver.ToGPUs

	// b.configRDMAEngine(gpu, rdmaAddressTable)
	b.configPMC(gpu, gpuDriver, pmcAddressTable)

	pcieConnector.PlugInDevice(pcieSwitchID, gpu.ExternalPorts())

	return gpu
}

// func (b *CommonPlatformBuilder) configRDMAEngine(
// 	gpu *mgpusim.GPU,
// 	addrTable *cache.BankedLowModuleFinder,
// ) {
// 	gpu.RDMAEngine.RemoteRDMAAddressTable = addrTable
// 	// addrTable.LowModules = append(
// 	// 	addrTable.LowModules,
// 	// 	gpu.RDMAEngine.RequestPort)
// }

func (b *CommonPlatformBuilder) configPMC(
	gpu *mgpusim.GPU,
	gpuDriver *driver.Driver,
	addrTable *lmf.BankedLowModuleFinder,
) {
	gpu.PMC.RemotePMCAddressTable = addrTable
	addrTable.LowModules = append(addrTable.LowModules, gpu.PMC.RemotePort)
	gpuDriver.RemotePMCPorts = append(gpuDriver.RemotePMCPorts, gpu.PMC.RemotePort)
}
