// local home finder for l1 cache.

package lmf

// type DynamicLocalHomeFinder struct {
// 	lock sync.Mutex

// 	ChipletId               uint64
// 	MemAddrOffset           uint64
// 	NumBanks                uint64
// 	NumMemoryBankPerChiplet uint64
// 	Striping                uint64
// 	LocalBankStart          uint64
// 	LocalBankEnd            uint64
// 	LowModules              []akita.Port
// 	RemoteModule            akita.Port // port of rdma engine

// 	WaferSize   int
// 	ClusterSize int
// }

// func NewDynamicLocalHomeFinder(
// 	chipletId uint64,
// 	waferSize int,
// 	memAddrOffset uint64,
// 	numBanks uint64,
// 	numMemoryBankPerChiplet uint64,
// 	striping uint64,
// 	start uint64,
// 	end uint64,
// ) *DynamicLocalHomeFinder {
// 	f := new(DynamicLocalHomeFinder)
// 	f.ChipletId = chipletId
// 	f.WaferSize = waferSize

// 	f.MemAddrOffset = memAddrOffset
// 	f.NumBanks = numBanks
// 	f.NumMemoryBankPerChiplet = numMemoryBankPerChiplet
// 	f.Striping = striping
// 	f.LocalBankStart = start
// 	f.LocalBankEnd = end
// 	f.LowModules = make([]akita.Port, 0)

// 	return f
// }

// func (f *DynamicLocalHomeFinder) Find(address uint64) akita.Port {
// 	address = address - f.MemAddrOffset
// 	i := (address % (f.NumBanks * f.Striping)) / f.Striping

// 	// global home is current chiplet
// 	if f.LocalBankStart <= i && i <= f.LocalBankEnd {
// 		return f.LowModules[i-f.LocalBankStart]
// 	}
// 	// global home chiplet id
// 	globalHomeId := i / f.NumMemoryBankPerChiplet
// 	globalHomeClusterId := f.GetClusterId(int(globalHomeId))
// 	currentClusterId := f.GetClusterId(int(f.ChipletId))

// 	// if the global home is in the current cluster,
// 	// cacheline can't be cached at current chiplet's l2 cache.
// 	if globalHomeClusterId == currentClusterId {
// 		return f.RemoteModule
// 	}

// 	// if local home is current chiplet, send to l2 cache.
// 	localHomeId := f.GetLocalHomeId(int(f.ChipletId), int(globalHomeId))
// 	if localHomeId == int(f.ChipletId) {
// 		i = (address % (f.NumMemoryBankPerChiplet * f.Striping)) / f.Striping
// 		return f.LowModules[i]
// 	}

// 	// if local home is not current chiplet, send to rdma engine.
// 	return f.RemoteModule
// }

// func (f *DynamicLocalHomeFinder) GetLocalHomeId(
// 	requestorId int,
// 	globalHomeId int,
// 	clusterSize int,
// ) int {
// 	requestorClusterId := f.GetClusterId(requestorId)

// 	clusterWidth := int(math.Sqrt(float64(f.ClusterSize)))

// 	// 1. global home 칩렛의 절대 좌표 계산
// 	globalHomeX := globalHomeId % f.WaferSize
// 	globalHomeY := globalHomeId / f.WaferSize

// 	// 2. global home의 클러스터 내 상대 좌표 (Local Offset) 계산
// 	globalHomeInClusterX := globalHomeX % clusterWidth
// 	globalHomeInClusterY := globalHomeY % clusterWidth

// 	// 3. requestor 클러스터의 시작 좌표 (Origin) 계산
// 	requestorClusterStartX := (requestorClusterId % (f.WaferSize / clusterWidth)) * clusterWidth
// 	requestorClusterStartY := (requestorClusterId / (f.WaferSize / clusterWidth)) * clusterWidth

// 	// 4. global home에 매칭되는 requestor 클러스터 내 절대 좌표 계산
// 	mappedX := requestorClusterStartX + globalHomeInClusterX
// 	mappedY := requestorClusterStartY + globalHomeInClusterY

// 	return mappedY*f.WaferSize + mappedX
// }

// func (f *DynamicLocalHomeFinder) GetClusterId(
// 	chipletId int,
// ) int {
// 	var div int
// 	switch f.ClusterSize {
// 	case 9:
// 		div = 2
// 	case 4:
// 		div = 3
// 	case 1:
// 		return chipletId
// 	}

// 	clusterDim := f.WaferSize / div

// 	row := chipletId / f.WaferSize
// 	col := chipletId % f.WaferSize

// 	partRow := row / clusterDim
// 	partCol := col / clusterDim

// 	return partRow*div + partCol
// }
