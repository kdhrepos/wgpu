package rdma

import (
	"gitlab.com/akita/protocol"
)

const (
	TotalReqCountThreshold = 10000
)

type SimplePCCounter struct {
	IsHot bool
	// IsWrite bool

	AccessBits       []bool
	UniqueCachelines map[uint64]bool // need to be implement with HLL
	ReqCount         uint64
	DistCount        uint64

	// for debug
	HopCount uint64

	// IsWrite bool
	// for debug
	// addrSet map[uint64]bool
}

type SimplePCTable struct {
	engine RDMAEngine
	Table  map[uint64]*SimplePCCounter // PC: PC Counter
	cms    *CountMinSketch

	Capacity int
	Len      int

	NumChiplets int

	TotalReqCount uint64
}

func NewSimplePCTable(
	engine RDMAEngine,
	xDim int,
	yDim int,
) *SimplePCTable {
	numChiplets := xDim * yDim

	return &SimplePCTable{
		engine:      engine,
		Table:       make(map[uint64]*SimplePCCounter),
		cms:         NewCountMinSketch(4, 4096),
		NumChiplets: numChiplets,
	}
}

func (t *SimplePCTable) ReqFromOutside(
	pc uint64,
	addr uint64,
	requestorId int,
	hopCount int, // FIXME: use hop count for weighted update of CMS
) {
	if pc == 0 {
		return
	}

	if counter, ok := t.Table[pc]; ok {
		counter.AccessBits[requestorId] = true
		counter.UniqueCachelines[addr] = true
		counter.ReqCount++
		counter.HopCount += uint64(hopCount)
	} else {
		t.Table[pc] = &SimplePCCounter{
			IsHot:            false,
			AccessBits:       make([]bool, t.NumChiplets),
			UniqueCachelines: make(map[uint64]bool),
		}
		t.Table[pc].AccessBits[requestorId] = true
		t.Table[pc].UniqueCachelines[addr] = true
		t.Table[pc].ReqCount++
		t.Table[pc].HopCount += uint64(hopCount)
	}
	t.TotalReqCount++
}

func (t *SimplePCTable) RspFromInside(
	pc uint64,
	addr uint64,
) protocol.Hotness {
	if pc == 0 {
		return protocol.Cold
	}

	counter, ok := t.Table[pc]
	if !ok {
		t.Table[pc] = &SimplePCCounter{
			IsHot: false,
			// IsWrite:    false,
			AccessBits:       make([]bool, t.NumChiplets),
			UniqueCachelines: make(map[uint64]bool),
		}

		return protocol.Cold
	}

	// get avg count per cacheline
	avgCountPerCacheline := uint64(0)
	if len(counter.UniqueCachelines) > 0 {
		avgCountPerCacheline = counter.ReqCount / uint64(len(counter.UniqueCachelines))
	}

	if avgCountPerCacheline >= 30 &&
		counter.ReqCount >= TotalReqCountThreshold {

		// check if distribution count is more than unique cacheline count
		if counter.DistCount >= uint64(len(counter.UniqueCachelines)) {
			return protocol.Cold
		}

		counter.DistCount++
		return protocol.Global
	} else if avgCountPerCacheline >= 15 &&
		counter.ReqCount >= TotalReqCountThreshold {

		// check if distribution count is more than unique cacheline count
		if counter.DistCount >= uint64(len(counter.UniqueCachelines)) {
			return protocol.Cold
		}

		counter.DistCount++
		return protocol.Hot
	} else if avgCountPerCacheline >= 4 &&
		counter.ReqCount >= TotalReqCountThreshold {

		// check if distribution count is more than unique cacheline count
		if counter.DistCount >= uint64(len(counter.UniqueCachelines)) {
			return protocol.Cold
		}

		counter.DistCount++
		return protocol.Warm
	}

	// bypass the l2 if cacheline is streaming data.
	if avgCountPerCacheline < 2 &&
		counter.ReqCount >= 10000 {
		return protocol.Streaming
	}

	return protocol.Cold
}

func (t *SimplePCTable) Lookup(pc uint64) {
	t.Table[pc].DistCount--
}

func (t *SimplePCTable) Decay() {
	// f, err := os.OpenFile(t.LogFilePath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	// if err != nil {
	// 	// 시뮬레이터 중단을 막기 위해 에러 발생 시 출력만 하고 넘어가거나 패닉 처리
	// 	fmt.Printf("Failed to open log file: %v\n", err)
	// } else {
	// 	defer f.Close()
	// }

	// t.cms.Decay()

	// decay dist count in PC counter
	for _, counter := range t.Table {
		if counter.DistCount > 10 {
			counter.DistCount -= 10
		} else if counter.DistCount > 0 {
			counter.DistCount = 0
		}
	}
}

func (t *SimplePCTable) Reset() {
	t.Table = nil
	t.Table = make(map[uint64]*SimplePCCounter)
	t.cms.Reset()
}

func (t *SimplePCTable) GetPCTable() interface{} {
	return t.Table
}

// MergeRemoteHotness is a no-op for the simple table.  Provided for
// PCTableInterface conformance; SimplePCTable's local heuristics do
// not consume home-side hotness hints.
func (t *SimplePCTable) MergeRemoteHotness(
	pc uint64,
	remote protocol.Hotness,
) {
}
