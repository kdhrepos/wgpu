package rdma

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"

	"gitlab.com/akita/protocol"
	"gitlab.com/akita/util/global"
)

type OraclePCCounter struct {
	IsHot bool
	// IsWrite bool

	AccessBits       []bool
	UniqueCachelines map[uint64]bool // need to be implement with HLL
	ReqCount         uint64
	DistCount        uint64
	HopCount         uint64
}

type OraclePCTable struct {
	engine RDMAEngine
	Table  map[uint64]*OraclePCCounter // PC: PC Counter
	cms    *CountMinSketch

	Capacity int
	Len      int

	NumChiplets int

	TotalReqCount uint64

	PcStates map[uint64]protocol.Hotness
}

func NewOraclePCTable(
	engine RDMAEngine,
	xDim int,
	yDim int,
) *OraclePCTable {
	numChiplets := xDim * yDim

	t := &OraclePCTable{
		engine: engine,
		Table:  make(map[uint64]*OraclePCCounter),
		// cms:         NewCountMinSketch(4, 4096),
		NumChiplets: numChiplets,
		PcStates:    make(map[uint64]protocol.Hotness),
	}

	t.loadPcStates()
	return t
}

func (t *OraclePCTable) loadPcStates() {
	filePath := os.Getenv("ORACLE_PC_JSON_PATH")
	if filePath == "" {
		_, filename, _, ok := runtime.Caller(0)
		if ok {
			filePath = filepath.Join(filepath.Dir(filename), "oracle_pc_states.json")
		} else {
			filePath = "./oracle_pc_states.json"
		}
	}

	data, err := os.ReadFile(filePath)
	if err != nil {
		fmt.Printf("Warning: Could not read oracle PC states file %s: %v\n", filePath, err)
		return
	}

	var rawData map[string]map[string]string
	err = json.Unmarshal(data, &rawData)
	if err != nil {
		fmt.Printf("Warning: Failed to parse oracle PC states json: %v\n", err)
		return
	}

	benchmarkName := global.GetInstance().GetBenchmarkName()

	pcMap, ok := rawData[benchmarkName]
	if !ok {
		fmt.Printf("Warning: No PC states found for benchmark '%s' in %s\n", benchmarkName, filePath)
		return
	}

	for pcStr, stateStr := range pcMap {
		pc, err := strconv.ParseUint(pcStr, 10, 64)
		if err != nil {
			fmt.Printf("Warning: Invalid PC format '%s'\n", pcStr)
			continue
		}

		var state protocol.Hotness
		switch stateStr {
		case "Streaming":
			state = protocol.Streaming
		case "Warm":
			state = protocol.Warm
		case "Hot":
			state = protocol.Hot
		case "Global":
			state = protocol.Global
		case "Warm_NonShared":
			state = protocol.Warm_NonShared
		case "Hot_NonShared":
			state = protocol.Hot_NonShared
		default:
			state = protocol.Cold
		}
		t.PcStates[pc] = state
	}
	fmt.Printf("Chiplet %d loaded %d PC states for benchmark '%s' from %s\n",
		t.engine.GetChipletId(), len(t.PcStates), benchmarkName, filePath)
}

func (t *OraclePCTable) ReqFromInside(
	pc uint64,
) protocol.Hotness {
	if pc == 0 {
		return protocol.Cold
	}

	state, exists := t.PcStates[pc]
	if exists {
		// switch state {
		// case protocol.Hot_NonShared:
		// 	return protocol.Hot
		// case protocol.Warm_NonShared:
		// 	return protocol.Warm
		// }

		return state
	} else {
		// counter, ok := t.Table[pc]
		// if !ok {
		// 	t.Table[pc] = &OraclePCCounter{
		// 		IsHot: false,
		// 		// IsWrite:    false,
		// 		AccessBits:       make([]bool, t.NumChiplets),
		// 		UniqueCachelines: make(map[uint64]bool),
		// 	}

		// 	return protocol.Cold
		// }

		// // get avg count per cacheline
		// avgCountPerCacheline := uint64(0)
		// if len(counter.UniqueCachelines) > 0 {
		// 	avgCountPerCacheline = counter.ReqCount / uint64(len(counter.UniqueCachelines))
		// }

		// if avgCountPerCacheline >= 30 &&
		// 	counter.ReqCount >= TotalReqCountThreshold {

		// 	return protocol.Global
		// } else if avgCountPerCacheline >= 15 &&
		// 	counter.ReqCount >= TotalReqCountThreshold {

		// 	return protocol.Hot
		// } else if avgCountPerCacheline >= 4 &&
		// 	counter.ReqCount >= TotalReqCountThreshold {

		// 	return protocol.Warm
		// }

		// // bypass the l2 if cacheline is streaming data.
		// if avgCountPerCacheline < 2 &&
		// 	counter.ReqCount >= 10000 {
		// 	return protocol.Streaming
		// }
	}

	return protocol.Cold
}

func (t *OraclePCTable) ReqFromOutside(
	pc uint64,
	addr uint64,
	requestorId int,
	hopCount int, // FIXME: use hop count for weighted update of CMS
) {
	if pc == 0 {
		return
	}

	if _, ok := t.Table[pc]; !ok {
		t.Table[pc] = &OraclePCCounter{}
		t.Table[pc].UniqueCachelines = make(map[uint64]bool)
		t.Table[pc].AccessBits = make([]bool, t.NumChiplets)
	}

	t.Table[pc].AccessBits[requestorId] = true
	t.Table[pc].UniqueCachelines[addr] = true
	t.Table[pc].ReqCount++
	t.Table[pc].HopCount += uint64(hopCount)
	t.TotalReqCount++
}

func (t *OraclePCTable) RspFromInside(
	pc uint64,
	addr uint64,
) protocol.Hotness {
	if pc == 0 {
		return protocol.Cold
	}

	state, exists := t.PcStates[pc]
	if exists {
		counter := t.Table[pc]

		if counter == nil {
			t.Table[pc] = &OraclePCCounter{
				IsHot: false,
				// IsWrite:    false,
				AccessBits:       make([]bool, t.NumChiplets),
				UniqueCachelines: make(map[uint64]bool),
			}

			counter = t.Table[pc]
		}

		if state <= protocol.Cold {
			return state
		}

		// check if distribution count is more than unique cacheline count
		// if counter.DistCount >= uint64(len(counter.UniqueCachelines)) {
		// 	return protocol.Cold
		// }

		return state
	} else {
		// counter, ok := t.Table[pc]
		// if !ok {
		// 	t.Table[pc] = &OraclePCCounter{
		// 		IsHot: false,
		// 		// IsWrite:    false,
		// 		AccessBits:       make([]bool, t.NumChiplets),
		// 		UniqueCachelines: make(map[uint64]bool),
		// 	}

		// 	return protocol.Cold
		// }

		// // get avg count per cacheline
		// avgCountPerCacheline := uint64(0)
		// if len(counter.UniqueCachelines) > 0 {
		// 	avgCountPerCacheline = counter.ReqCount / uint64(len(counter.UniqueCachelines))
		// }

		// if avgCountPerCacheline >= 30 &&
		// 	counter.ReqCount >= TotalReqCountThreshold {

		// 	// check if distribution count is more than unique cacheline count
		// 	if counter.DistCount >= uint64(len(counter.UniqueCachelines)) {
		// 		return protocol.Cold
		// 	}

		// 	counter.DistCount++
		// 	return protocol.Global
		// } else if avgCountPerCacheline >= 15 &&
		// 	counter.ReqCount >= TotalReqCountThreshold {

		// 	// check if distribution count is more than unique cacheline count
		// 	if counter.DistCount >= uint64(len(counter.UniqueCachelines)) {
		// 		return protocol.Cold
		// 	}

		// 	counter.DistCount++
		// 	return protocol.Hot
		// } else if avgCountPerCacheline >= 4 &&
		// 	counter.ReqCount >= TotalReqCountThreshold {

		// 	// check if distribution count is more than unique cacheline count
		// 	if counter.DistCount >= uint64(len(counter.UniqueCachelines)) {
		// 		return protocol.Cold
		// 	}

		// 	counter.DistCount++
		// 	return protocol.Warm
		// }

		// // bypass the l2 if cacheline is streaming data.
		// if avgCountPerCacheline < 2 &&
		// 	counter.ReqCount >= 10000 {
		// 	return protocol.Streaming
		// }
	}

	return protocol.Cold
}

func (t *OraclePCTable) Decay() {
	// f, err := os.OpenFile(t.LogFilePath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	// if err != nil {
	// 	// 시뮬레이터 중단을 막기 위해 에러 발생 시 출력만 하고 넘어가거나 패닉 처리
	// 	fmt.Printf("Failed to open log file: %v\n", err)
	// } else {
	// 	defer f.Close()
	// }

	// decay dist count in PC counter
	for _, counter := range t.Table {
		if counter.DistCount > 10 {
			counter.DistCount -= 10
		} else if counter.DistCount > 0 {
			counter.DistCount = 0
		}
	}
}

func (t *OraclePCTable) Distribute(pc uint64) {
	if counter, ok := t.Table[pc]; ok {
		counter.DistCount++
	}
}

// MergeRemoteHotness is a no-op for the Oracle table: the oracle JSON
// already provides a global view, so home-side hints from responses
// would not add information.
func (t *OraclePCTable) MergeRemoteHotness(
	pc uint64,
	remote protocol.Hotness,
) {
}

func (t *OraclePCTable) Reset() {
	// t.Table = nil
	// t.Table = make(map[uint64]*OraclePCCounter)
	// t.cms.Reset()
}

func (t *OraclePCTable) GetPCTable() interface{} {
	return t.Table
}
