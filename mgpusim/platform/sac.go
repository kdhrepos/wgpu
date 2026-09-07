package platform

import (
	"gitlab.com/akita/akita"
	"gitlab.com/akita/mem"
	"gitlab.com/akita/mgpusim/builders"
	"gitlab.com/akita/mgpusim/driver"
)

// SACGPUPlatformBuilder builds the SAC (Sharing-Aware Caching, ISCA'23)
// wafer-scale platform: identical to the mesh platform, with a
// reconfigurable memory-side/SM-side LLC driven by a per-kernel profiling
// controller.
type SACGPUPlatformBuilder struct {
	CommonPlatformBuilder
}

func MakeSACGPUPlatformBuilder() *SACGPUPlatformBuilder {
	b := &SACGPUPlatformBuilder{
		CommonPlatformBuilder: CommonPlatformBuilder{
			numGPU:                   1,
			log2PageSize:             uint64(12), // 4KB
			numCUPerShaderArray:      uint64(4),
			numShaderArrayPerChiplet: uint64(8),
			numMemoryBankPerChiplet:  uint64(32),
			numChiplets:              uint64(4),
			totalMem:                 16 * mem.GB,
			bankSize:                 256 * mem.MB,
			lowAddr:                  4 * mem.GB,
		}}
	return b
}

func (b SACGPUPlatformBuilder) Build() (akita.Engine, *driver.Driver) {
	engine := b.createEngine()

	gpuDriver := driver.NewDriver(engine,
		b.log2PageSize,
		b.memAllocatorType,
		b.numChiplets,
		b.dataPlacementPartition,
		b.dataPlacementStride)

	gpuBuilder := b.createSACGPUBuilder(engine, gpuDriver)
	pcieConnector, rootComplexID :=
		b.createConnection(engine, gpuDriver)

	rdmaAddressTable := b.createRDMAAddrTable()

	pmcAddressTable := b.createPMCPageTable()

	b.createGPUs(
		rootComplexID, pcieConnector,
		gpuBuilder, gpuDriver,
		rdmaAddressTable, pmcAddressTable)

	if ctrl := gpuBuilder.SACController(); ctrl != nil {
		gpuDriver.SetKernelCompletionObserver(ctrl)
	}

	return engine, gpuDriver
}

func (b *SACGPUPlatformBuilder) createSACGPUBuilder(
	engine akita.Engine,
	gpuDriver *driver.Driver,
) *builders.SACGPUBuilder {
	gpuBuilder := builders.MakeSACGPUBuilder()
	gpuBuilder.WithEngine(engine)
	gpuBuilder.WithNumCUPerShaderArray(b.numCUPerShaderArray)
	gpuBuilder.WithNumShaderArrayPerChiplet(b.numShaderArrayPerChiplet)
	gpuBuilder.WithNumMemoryBankPerChiplet(b.numMemoryBankPerChiplet)
	gpuBuilder.WithSwitchLatency(b.switchLatency)
	gpuBuilder.WithNumReqPerCycle(b.numReqPerCycle)
	gpuBuilder.WithGridSize(b.gridSize[0], b.gridSize[1])
	gpuBuilder.WithNumChiplets(uint64(b.gridSize[0] * b.gridSize[1]))
	gpuBuilder.WithTotalMem(b.totalMem)
	gpuBuilder.CalculateMemoryParameters()
	gpuBuilder.WithLog2PageSize(b.log2PageSize)
	gpuBuilder.WithPageTable(gpuDriver.PageTable)
	gpuBuilder.WithAlg(b.alg)
	gpuBuilder.WithSchedulingPartition(b.schedPartition)
	gpuBuilder.WithDataPlacementPartition(b.dataPlacementPartition)
	gpuBuilder.WithDataPlacementStride(b.dataPlacementStride)
	gpuBuilder.WithPC(b.reqWithPC)
	gpuBuilder.WithPteCaching(b.pteCaching)

	b.setVisTracer(gpuDriver, &gpuBuilder)
	b.setTLBTracer(&gpuBuilder)
	b.setMemTracer(&gpuBuilder)
	b.setISADebugger(&gpuBuilder)

	if b.disableProgressBar {
		gpuBuilder.WithoutProgressBar()
	}

	return &gpuBuilder
}
