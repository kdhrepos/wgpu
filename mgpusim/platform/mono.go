package platform

import (
	"gitlab.com/akita/akita"
	"gitlab.com/akita/mem"
	"gitlab.com/akita/mgpusim/builders"
	"gitlab.com/akita/mgpusim/driver"
)

// MonoGPUPlatformBuilder builds a monolithic single-chiplet GPU whose CU /
// L1 / L2 / DRAM resources scale with (xDim * yDim). It serves as the
// "all-resources-fused-into-one-chip" baseline for comparing against the
// chiplet-based platforms (mesh / direct / dynamic-dist / ...).
//
// xDim and yDim are reinterpreted as a resource multiplier — there is no
// inter-chiplet network, RDMA traffic, or page migration across chiplets.
type MonoGPUPlatformBuilder struct {
	CommonPlatformBuilder
}

// WithGridSize overrides CommonPlatformBuilder.WithGridSize to keep
// numChiplets pinned at 1 for monolithic semantics. CommonPlatformBuilder's
// version would set numChiplets = xDim*yDim, which causes driver.NewDriver
// (and downstream the LASP page allocator) to split memory across that many
// "virtual chiplets" — wrong for mono, where every page lives on the single
// physical chiplet. The xDim*yDim product is still consumed inside
// MonoGPUBuilder.Build to scale SA/L2-bank counts.
func (b *MonoGPUPlatformBuilder) WithGridSize(xDim, yDim int) {
	b.gridSize[0] = xDim
	b.gridSize[1] = yDim
	b.numChiplets = 1
}

func MakeMonoGPUPlatformBuilder() *MonoGPUPlatformBuilder {
	b := &MonoGPUPlatformBuilder{
		CommonPlatformBuilder: CommonPlatformBuilder{
			numGPU:                   1,
			log2PageSize:             uint64(12),
			numCUPerShaderArray:      uint64(4),
			numShaderArrayPerChiplet: uint64(8),
			numMemoryBankPerChiplet:  uint64(32),
			numChiplets:              uint64(1),
			totalMem:                 16 * mem.GB,
			bankSize:                 256 * mem.MB,
			lowAddr:                  4 * mem.GB,
		}}
	return b
}

func (b MonoGPUPlatformBuilder) Build() (akita.Engine, *driver.Driver) {
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

func (b *MonoGPUPlatformBuilder) createGPUBuilder(
	engine akita.Engine,
	gpuDriver *driver.Driver,
) builders.Builder {
	gpuBuilder := builders.MakeMonoGPUBuilder()
	gpuBuilder.WithEngine(engine)
	gpuBuilder.WithNumCUPerShaderArray(b.numCUPerShaderArray)
	gpuBuilder.WithNumShaderArrayPerChiplet(b.numShaderArrayPerChiplet)
	gpuBuilder.WithNumMemoryBankPerChiplet(b.numMemoryBankPerChiplet)
	gpuBuilder.WithNumReqPerCycle(b.numReqPerCycle)
	gpuBuilder.WithSwitchLatency(b.switchLatency)
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
	gpuBuilder.WithPteCaching(b.pteCaching)
	gpuBuilder.WithPC(b.reqWithPC)

	b.setVisTracer(gpuDriver, gpuBuilder)
	b.setTLBTracer(gpuBuilder)
	b.setMemTracer(gpuBuilder)
	b.setISADebugger(gpuBuilder)

	if b.disableProgressBar {
		gpuBuilder.WithoutProgressBar()
	}

	return gpuBuilder
}
