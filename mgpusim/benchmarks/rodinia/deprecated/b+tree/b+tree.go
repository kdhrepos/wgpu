package bptree

import (
	_ "embed"
	"log"
	"math"
	"math/rand/v2"

	"gitlab.com/akita/mgpusim/driver"
	"gitlab.com/akita/mgpusim/insts"
	"gitlab.com/akita/mgpusim/kernels"
)

var (
	// DEFAULT_ORDER 256 기준 (int 4B * 514 + location 4B + isLeaf 1B + numKeys 4B + padding)
	KnodeStructSize = uint64(2068)
)

type Knode struct {
	location int32
	indices  [257]int32
	keys     [257]int32
	isLeaf   int32
	numKeys  int32
}

// FindKKernelArgs는 OpenCL 커널의 파라미터 순서와 1:1로 매핑되어야 합니다.
type FindKKernelArgs struct {
	MaxHeight  int32         // long -> int32 (GCN3 scalar 최적화 대응)
	Padding    int32         // 명시적 패딩 (binary.Write는 패딩을 무시하므로 8바이트 정렬을 위해 추가)
	KnodesD    driver.GPUPtr // __global knode *
	KnodesElem int64         // long
	RecordsD   driver.GPUPtr // __global record *

	CurrKnodeD driver.GPUPtr // __global long *
	OffsetD    driver.GPUPtr // __global long *
	KeysD      driver.GPUPtr // __global int *
	AnsD       driver.GPUPtr // __global record *

	// MGPUSim 내부 필드
	HiddenGlobalOffsetX int64
	HiddenGlobalOffsetY int64
	HiddenGlobalOffsetZ int64
}

type Benchmark struct {
	driver      *driver.Driver
	context     *driver.Context
	queue       *driver.CommandQueue
	findkKernel *insts.HsaCo
	gpus        []int

	maxHeight    int32
	defaultOrder int

	Count int // 쿼리 개수 (Thread 수 결정)
	Size  int // 트리 노드/레코드 규모

	KnodesElem  uint64
	RecordsElem uint64

	/* Host Buffers */
	KnodesH    []Knode
	RecordsH   []int32
	CurrKnodeH []int64
	OffSetH    []int64
	KeysH      []int32
	AnsH       []int32

	/* Device Pointers */
	KnodesD    driver.GPUPtr
	RecordsD   driver.GPUPtr
	CurrKnodeD driver.GPUPtr
	OffSetD    driver.GPUPtr
	KeysD      driver.GPUPtr
	AnsD       driver.GPUPtr

	useLASPMemoryAlloc bool
	useUnifiedMemory   bool
}

type syntheticNode struct {
	isLeaf   bool
	keys     []int32
	children []*syntheticNode
}

//go:embed kernels.hsaco
var hsacoBytes []byte

func NewBenchmark(driver *driver.Driver) *Benchmark {
	b := new(Benchmark)
	b.driver = driver
	b.context = b.driver.Init()
	b.queue = driver.CreateCommandQueue(b.context)
	b.findkKernel = kernels.LoadProgramFromMemory(hsacoBytes, "findK")
	b.defaultOrder = 256
	return b
}

func (b *Benchmark) SelectGPU(gpus []int) {
	b.gpus = gpus
}
func (b *Benchmark) SetLASPMemoryAlloc() {
	b.useLASPMemoryAlloc = true
}

func (b *Benchmark) Run() {
	b.driver.SelectGPU(b.context, b.gpus[0])
	b.initMem()
	b.exec()
}

func (b *Benchmark) initMem() {
	count := b.Count
	size := b.Size

	// 1. Host-side packed tree construction that mirrors transform_to_cuda.
	b.KnodesH, b.RecordsH, b.maxHeight = b.buildSyntheticTree()
	b.KnodesElem = uint64(len(b.KnodesH))
	b.RecordsElem = uint64(len(b.RecordsH))

	// 2. 메모리 할당 크기 계산
	knodesByteSize := b.KnodesElem * KnodeStructSize
	recordsByteSize := b.RecordsElem * 4

	// 3. 호스트 query buffers 준비
	b.CurrKnodeH = make([]int64, count)
	b.OffSetH = make([]int64, count)
	b.KeysH = make([]int32, count)
	b.AnsH = make([]int32, count)

	for i := 0; i < count; i++ {
		b.CurrKnodeH[i] = 0 // Root 시작
		b.OffSetH[i] = 0
		b.KeysH[i] = int32(rand.Float64() * float64(size))
		b.AnsH[i] = -1
	}

	if b.useUnifiedMemory {
		b.KnodesD = b.driver.AllocateUnifiedMemory(b.context, knodesByteSize)
		b.RecordsD = b.driver.AllocateUnifiedMemory(b.context, recordsByteSize)
		b.CurrKnodeD = b.driver.AllocateUnifiedMemory(b.context, uint64(count*8))
		b.OffSetD = b.driver.AllocateUnifiedMemory(b.context, uint64(count*8))
		b.KeysD = b.driver.AllocateUnifiedMemory(b.context, uint64(count*4))
		b.AnsD = b.driver.AllocateUnifiedMemory(b.context, uint64(count*4))
	} else if b.useLASPMemoryAlloc {
		b.KnodesD = b.driver.AllocateMemoryLASP(b.context, knodesByteSize, "div4")
		b.RecordsD = b.driver.AllocateMemoryLASP(b.context, recordsByteSize, "div4")
		b.CurrKnodeD = b.driver.AllocateMemoryLASP(b.context, uint64(count*8), "div4")
		b.OffSetD = b.driver.AllocateMemoryLASP(b.context, uint64(count*8), "div4")
		b.KeysD = b.driver.AllocateMemoryLASP(b.context, uint64(count*4), "div4")
		b.AnsD = b.driver.AllocateMemoryLASP(b.context, uint64(count*4), "div4")
	} else {
		b.KnodesD = b.driver.AllocateMemory(b.context, knodesByteSize)
		b.RecordsD = b.driver.AllocateMemory(b.context, recordsByteSize)
		b.CurrKnodeD = b.driver.AllocateMemory(b.context, uint64(count*8))
		b.OffSetD = b.driver.AllocateMemory(b.context, uint64(count*8))
		b.KeysD = b.driver.AllocateMemory(b.context, uint64(count*4))
		b.AnsD = b.driver.AllocateMemory(b.context, uint64(count*4))
	}
}

func (b *Benchmark) exec() {
	b.driver.MemCopyH2D(b.context, b.KnodesD, b.KnodesH)
	b.driver.MemCopyH2D(b.context, b.RecordsD, b.RecordsH)
	b.driver.MemCopyH2D(b.context, b.CurrKnodeD, b.CurrKnodeH)
	b.driver.MemCopyH2D(b.context, b.OffSetD, b.OffSetH)
	b.driver.MemCopyH2D(b.context, b.KeysD, b.KeysH)
	b.driver.MemCopyH2D(b.context, b.AnsD, b.AnsH)

	log.Printf("KnodesD Address: 0x%x", b.KnodesD)
	log.Printf("RecordsD Address: 0x%x", b.RecordsD)
	log.Printf("CurrKnodeD Address: 0x%x", b.CurrKnodeD)
	log.Printf("OffSetD Address: 0x%x", b.OffSetD)
	log.Printf("KeysD Address: 0x%x", b.KeysD)
	log.Printf("AnsD Address: 0x%x", b.AnsD)

	// GPGPU 워크로드 설정: 각 쿼리당 스레드 하나가 아니라, 노드 차수(Order)만큼의 스레드 사용
	localWorkSize := [3]uint16{uint16(b.defaultOrder), 1, 1}
	globalWorkSize := [3]uint32{uint32(b.Count * b.defaultOrder), 1, 1}

	kernArg := FindKKernelArgs{
		MaxHeight:           b.maxHeight,
		KnodesD:             b.KnodesD,
		KnodesElem:          int64(b.KnodesElem),
		RecordsD:            b.RecordsD,
		CurrKnodeD:          b.CurrKnodeD,
		OffsetD:             b.OffSetD,
		KeysD:               b.KeysD,
		AnsD:                b.AnsD,
		HiddenGlobalOffsetX: 0,
		HiddenGlobalOffsetY: 0,
		HiddenGlobalOffsetZ: 0,
	}

	b.driver.LaunchKernel(
		b.context,
		b.findkKernel,
		globalWorkSize,
		localWorkSize,
		&kernArg,
	)

	b.driver.DrainCommandQueue(b.queue)
}

func (b *Benchmark) Verify() {
}

// SetUnifiedMemory uses Unified Memory
func (b *Benchmark) SetUnifiedMemory() {
	b.useUnifiedMemory = true
}

func (b *Benchmark) buildSyntheticTree() ([]Knode, []int32, int32) {
	order := b.defaultOrder
	if b.Size <= 0 {
		return nil, nil, 0
	}

	records := make([]int32, b.Size)
	for i := range records {
		records[i] = int32(i * 10)
	}

	leafCapacity := order - 1
	if leafCapacity <= 0 {
		panic("default order must be at least 2")
	}

	leaves := make([]*syntheticNode, 0, (len(records)+leafCapacity-1)/leafCapacity)
	for start := 0; start < len(records); start += leafCapacity {
		end := start + leafCapacity
		if end > len(records) {
			end = len(records)
		}

		keys := make([]int32, end-start)
		copy(keys, records[start:end])
		leaves = append(leaves, &syntheticNode{
			isLeaf: true,
			keys:   keys,
		})
	}

	levels := 0
	currentLevel := leaves
	for len(currentLevel) > 1 {
		nextLevel := make([]*syntheticNode, 0, (len(currentLevel)+order-1)/order)
		for start := 0; start < len(currentLevel); start += order {
			end := start + order
			if end > len(currentLevel) {
				end = len(currentLevel)
			}

			children := currentLevel[start:end]
			separatorKeys := make([]int32, 0, len(children)-1)
			for i := 1; i < len(children); i++ {
				separatorKeys = append(separatorKeys, children[i].keys[0])
			}

			nextLevel = append(nextLevel, &syntheticNode{
				keys:     separatorKeys,
				children: children,
			})
		}
		currentLevel = nextLevel
		levels++
	}

	root := currentLevel[0]
	queue := []*syntheticNode{root}
	flat := make([]*syntheticNode, 0)
	for len(queue) > 0 {
		node := queue[0]
		queue = queue[1:]
		flat = append(flat, node)
		if !node.isLeaf {
			queue = append(queue, node.children...)
		}
	}

	location := make(map[*syntheticNode]int32, len(flat))
	for i, node := range flat {
		location[node] = int32(i)
	}

	knodes := make([]Knode, len(flat))
	recordIndex := 0
	for i, node := range flat {
		knode := &knodes[i]
		knode.location = int32(i)

		if node.isLeaf {
			actualKeys := len(node.keys)
			knode.isLeaf = 1
			knode.numKeys = int32(actualKeys + 2)
			knode.keys[0] = math.MinInt32
			for j, key := range node.keys {
				knode.keys[j+1] = key
				knode.indices[j+1] = int32(recordIndex)
				recordIndex++
			}
			knode.keys[actualKeys+1] = math.MaxInt32
			for j := actualKeys + 2; j < len(knode.keys); j++ {
				knode.keys[j] = math.MaxInt32
			}
			knode.indices[actualKeys+1] = int32(i + 1)
		} else {
			childCount := len(node.children)
			actualKeys := childCount - 1
			knode.isLeaf = 0
			knode.numKeys = int32(actualKeys + 2)
			knode.keys[0] = math.MinInt32
			for j, key := range node.keys {
				knode.keys[j+1] = key
			}
			knode.keys[actualKeys+1] = math.MaxInt32
			for j := actualKeys + 2; j < len(knode.keys); j++ {
				knode.keys[j] = math.MaxInt32
			}
			for j, child := range node.children {
				knode.indices[j] = location[child]
			}
			knode.indices[childCount] = int32(i + 1)
		}
	}

	return knodes, records, int32(levels)
}
