package platform

import (
	"gitlab.com/akita/akita"
	"gitlab.com/akita/mem"
	"gitlab.com/akita/mgpusim/builders"
	"gitlab.com/akita/mgpusim/driver"
)

// DataIdealGPUPlatformBuilder produces a wafer-scale GPU that uses an ideal
// 1-hop crossbar for data inter-chiplet traffic but a parallel realistic mesh
// NoC for PTW (page-table-walk) traffic. It is the symmetric inverse of the
// pt-ideal platform, useful to isolate the hop-count penalty data pays in pure
// mesh topologies from PTW behavior. See builders/dataideal.go for the
// dual-network wiring.
type DataIdealGPUPlatformBuilder struct {
	CommonPlatformBuilder
}

func MakeDataIdealGPUPlatformBuilder() *DataIdealGPUPlatformBuilder {
	b := &DataIdealGPUPlatformBuilder{
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

func (b DataIdealGPUPlatformBuilder) Build() (akita.Engine, *driver.Driver) {
	engine := b.createEngine()

	gpuDriver := driver.NewDriver(engine,
		b.log2PageSize,
		b.memAllocatorType,
		b.numChiplets,
		b.dataPlacementPartition,
		b.dataPlacementStride)

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

func (b *DataIdealGPUPlatformBuilder) createGPUBuilder(
	engine akita.Engine,
	gpuDriver *driver.Driver,
) builders.Builder {
	gpuBuilder := builders.MakeDataIdealGPUBuilder()
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

	b.setVisTracer(gpuDriver, gpuBuilder)
	b.setTLBTracer(gpuBuilder)
	b.setMemTracer(gpuBuilder)
	b.setISADebugger(gpuBuilder)

	if b.disableProgressBar {
		gpuBuilder.WithoutProgressBar()
	}

	return gpuBuilder
}
