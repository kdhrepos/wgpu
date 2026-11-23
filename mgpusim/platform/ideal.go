package platform

import (
	"gitlab.com/akita/akita"
	"gitlab.com/akita/mem"
	"gitlab.com/akita/mgpusim/builders"
	"gitlab.com/akita/mgpusim/driver"
)

// Ideal GPU for wafer-scale GPU
type IdealGPUPlatformBuilder struct {
	CommonPlatformBuilder
}

func MakeIdealGPUPlatformBuilder() *IdealGPUPlatformBuilder {
	b := &IdealGPUPlatformBuilder{
		CommonPlatformBuilder: CommonPlatformBuilder{
			numGPU:                   1,
			log2PageSize:             uint64(12), // 4KB
			numCUPerShaderArray:      uint64(4),
			numShaderArrayPerChiplet: uint64(8), // changed from 8 to 16
			numMemoryBankPerChiplet:  uint64(32),
			numChiplets:              uint64(4),
			totalMem:                 16 * mem.GB,  // just a default value, will be set at createGPUBuilder function
			bankSize:                 256 * mem.MB, // bank sizes are fixed as 256MB
			lowAddr:                  4 * mem.GB,   // memory size of each chiplet is fixed as 4GB
		}}
	return b
}

func (b IdealGPUPlatformBuilder) Build() (akita.Engine, *driver.Driver) {
	engine := b.createEngine()

	gpuDriver := driver.NewDriver(engine,
		b.log2PageSize,
		b.memAllocatorType,
		b.numChiplets,
		b.dataPlacementPartition,
		b.dataPlacementStride)

	// need to set the number of chiplets
	gpuBuilder := b.createGPUBuilder(engine, gpuDriver)
	pcieConnector, rootComplexID :=
		b.createConnection(engine, gpuDriver)

	rdmaAddressTable := b.createRDMAAddrTable()

	pmcAddressTable := b.createPMCPageTable()

	b.createGPUs(
		rootComplexID, pcieConnector,
		gpuBuilder, gpuDriver,
		rdmaAddressTable, pmcAddressTable)

	return engine, gpuDriver
}

func (b *IdealGPUPlatformBuilder) createGPUBuilder(
	engine akita.Engine,
	gpuDriver *driver.Driver,
) builders.Builder {
	gpuBuilder := builders.MakeIdealGPUBuilder()
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
	gpuBuilder.WithPteCaching(true)

	b.setISADebugger(gpuBuilder)

	if b.disableProgressBar {
		gpuBuilder.WithoutProgressBar()
	}

	return gpuBuilder
}
