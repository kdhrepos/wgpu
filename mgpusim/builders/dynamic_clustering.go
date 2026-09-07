package builders

import (
	"fmt"

	"gitlab.com/akita/akita"
	"gitlab.com/akita/mgpusim"
	"gitlab.com/akita/noc/networking/chipnetwork"
)

type DynamicClusteringGPUBuilder struct {
	*CommonBuilder
	gridSize    [2]int
	clusterSize int
	cbfSize     int
}

func (b *DynamicClusteringGPUBuilder) WithGridSize(xDim, yDim int) {
	b.gridSize[0] = xDim
	b.gridSize[1] = yDim
}

func (b *DynamicClusteringGPUBuilder) WithClusterSize(clusterSize int) {
	b.clusterSize = clusterSize
}

func (b *DynamicClusteringGPUBuilder) WithCbfSize(s int) {
	b.cbfSize = s
}

func MakeDynamicClusteringGPUBuilder() DynamicClusteringGPUBuilder {
	cbp := CommonBuilder{}
	b := DynamicClusteringGPUBuilder{CommonBuilder: &cbp}
	b.SetDefaultCommonBuilderParams()
	return b
}

func (b DynamicClusteringGPUBuilder) Build(
	name string,
	id uint64,
) *mgpusim.GPU {
	b.createGPU(name, id)
	b.buildCP()

	for r := 0; r < b.gridSize[1]; r++ { // row
		for c := 0; c < b.gridSize[0]; c++ { // column
			chipletName := fmt.Sprintf("%s.chiplet_%02d", b.gpuName, r*b.gridSize[0]+c)
			chipletId := uint64(r*b.gridSize[0] + c)
			chiplet := NewChiplet(chipletName, chipletId)

			b.buildSAs(chiplet)      // L1 cache and TLB are built in here.
			b.buildMemBanks(chiplet) // L2 is built in here.
			b.buildMMU(chiplet)
			b.buildL2TLB(chiplet)

			b.buildChipRDMAEngine(chiplet)

			b.connectL1ToL2(chiplet)
			b.connectL2ToDram(chiplet) // Single-port L2-DRAM wiring
			b.connectL2ToRdma(chiplet) // Single-port L2-RDMA wiring
			b.connectL1TLBToL2TLB(chiplet)
			b.connectL2TLBToMMU(chiplet)
			b.connectMMUToBottom(chiplet)
			b.connectMMUToL2(chiplet) // for pte caching

			b.chiplets = append(b.chiplets, chiplet)

			chiplet.chipRdmaEngine.SetY(r)
			chiplet.chipRdmaEngine.SetX(c)

			b.setConnectionToDriver(chiplet)
		}
	}

	b.connectRDMAEngines()

	b.buildPageMigrationController()
	b.setupDMA()

	b.connectCP()
	b.setupDistInterchipNetwork()
	b.setupSwPortsForRdmaEngine()

	return b.gpu
}

func (b *DynamicClusteringGPUBuilder) setupDistInterchipNetwork() {
	fmt.Println("Dist Builder")

	// interconnect bandwidth: 1GHz * 64B * 12 = 768 GBps (example)
	chipDistConnector := chipnetwork.NewInterChipletDynamicClusteringConnector().
		WithEngine(b.engine).
		WithSwitchLatency(b.switchLatency). // 32ns
		WithFreq(1*akita.GHz).
		// WithFlitByteSize(64).
		WithFlitByteSize(16).
		WithNumReqPerCycle(b.numReqPerCycle).
		WithGridSize(b.gridSize[0], b.gridSize[1]).
		WithNumChiplets(b.gridSize[0] * b.gridSize[1]).
		WithRDMAReqPorts(b.GetRdmaReqPorts()).
		WithRDMARspPorts(b.GetRdmaRspPorts()).
		WithNetworkName("ICN")

	chipDistConnector.CreateNetwork()

	b.gpu.InterChipletNetwork = append(b.gpu.InterChipletNetwork, chipDistConnector)
	b.gpu.SwitchConnection = chipDistConnector.SwitchConnection
}

func (b *DynamicClusteringGPUBuilder) GetRdmaReqPorts() map[uint64]akita.Port {
	chipletPorts := make(map[uint64]akita.Port)

	for _, chiplet := range b.chiplets {
		chipletPorts[chiplet.ChipletID] = chiplet.chipRdmaEngine.GetRequestPort()
	}

	return chipletPorts
}

func (b *DynamicClusteringGPUBuilder) GetRdmaRspPorts() map[uint64]akita.Port {
	chipletPorts := make(map[uint64]akita.Port)

	for _, chiplet := range b.chiplets {
		chipletPorts[chiplet.ChipletID] = chiplet.chipRdmaEngine.GetResponsePort()
	}

	return chipletPorts
}
