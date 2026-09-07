// MemoryAccessTracer reports every access count per memory address
package tracing

import (
	"fmt"
	"os"
	"slices"
	"strconv"
	"time"

	"gitlab.com/akita/akita"
	"gitlab.com/akita/protocol"
	"gitlab.com/akita/util/global"
)

const (
	READ  int = 0
	WRITE int = 1
)

type accessInfo struct {
	addr       uint64
	pc         uint64
	rw         int
	owner      int
	isVector   uint64
	accessType uint64
	isPTW      int
}

type MemoryAccessTracer struct {
	// time -> addr -> PC -> metrics
	memoryAccessPerTime map[akita.VTimeInSec]map[accessInfo][]uint64
	numChiplets         int

	simulationStartTime time.Time
	colPrinted          bool
	memoryTraceFileName string

	accessCountPerAddrs map[uint64]uint64

	// chipletIDCache memoizes task.Where (cache component name) -> chiplet ID
	// so we avoid string-parsing the full component name on every access.
	chipletIDCache map[string]int
}

func NewMemoryAccessTracer(
	name string,
	xDim int,
	yDim int,
) *MemoryAccessTracer {
	t := &MemoryAccessTracer{
		memoryTraceFileName: name,
		simulationStartTime: time.Now(),
		memoryAccessPerTime: make(map[akita.VTimeInSec]map[accessInfo][]uint64),
		numChiplets:         xDim * yDim,
		accessCountPerAddrs: make(map[uint64]uint64),
		chipletIDCache:      make(map[string]int),
	}

	return t
}

func (t *MemoryAccessTracer) StartTask(task Task) {
	if task.Kind != "memory-trace" {
		return
	}

	var addr uint64
	var PC uint64
	rw := READ
	isPTW := 0

	switch d := task.Detail.(type) {
	case *VictimInfo:
		// type 8 (victim_evict) / type 9 (victim_overwrite) row: the line
		// that just got evicted. rw/vector/isPTW are not meaningful here.
		addr = d.VictimAddr
		PC = d.VictimPC
	case protocol.AccessReq:
		req := d
		switch req := req.(type) {
		case *protocol.ReadReq:
			pc, ok := req.Info.(uint64)
			if !ok {
				return
			}
			PC = pc
			if req.IsPtw {
				isPTW = 1
			}
		case *protocol.WriteReq:
			pc, ok := req.Info.(uint64)
			if !ok {
				return
			}
			PC = pc
			rw = WRITE
		default:
			return
		}
		addr = req.GetAddress()
	default:
		return
	}

	current := task.StartTime

	srcChip, ok := t.chipletIDCache[task.Where]
	if !ok {
		srcChip = parseChipletIDFromComponentName(task.Where)
		t.chipletIDCache[task.Where] = srcChip
	}

	owner, _ := strconv.Atoi(task.What)

	if _, ok := t.memoryAccessPerTime[current]; !ok {
		t.memoryAccessPerTime[current] = make(map[accessInfo][]uint64)
	}

	key := accessInfo{
		addr:       addr,
		pc:         PC,
		rw:         rw,
		owner:      owner,
		isVector:   task.ParentID,
		accessType: task.ID,
		isPTW:      isPTW,
	}

	if _, ok := t.memoryAccessPerTime[current][key]; !ok {
		t.memoryAccessPerTime[current][key] = make([]uint64, t.numChiplets)
	}
	t.memoryAccessPerTime[current][key][srcChip]++

	// t.accessCountPerAddrs[addr]++

	if time.Since(t.simulationStartTime) >= time.Hour {
		dataToDump := t.memoryAccessPerTime
		t.memoryAccessPerTime = make(map[akita.VTimeInSec]map[accessInfo][]uint64)
		t.simulationStartTime = time.Now()

		t.Dump(dataToDump)
	} else {
	}
}

func (t *MemoryAccessTracer) StepTask(task Task) {
	// Do nothing
}

func (t *MemoryAccessTracer) EndTask(task Task) {
	// Do nothing
}

func (t *MemoryAccessTracer) Dump(
	data map[akita.VTimeInSec]map[accessInfo][]uint64,
) {
	dumpFromCall := false
	if data == nil {
		data = t.memoryAccessPerTime
		dumpFromCall = true
	}

	if len(data) == 0 {
		return
	}

	var f *os.File
	var err error

	// 파일이 없으면 생성(Create), 있으면 이어쓰기(Append)
	if _, statErr := os.Stat(t.memoryTraceFileName + ".csv"); os.IsNotExist(statErr) {
		f, err = os.Create(t.memoryTraceFileName + ".csv")
		if err == nil {
			fmt.Fprintf(f, "time,addr,pc,owner,rw,rdma,vector,type,chiplet,data_id,is_ptw\n")
		}
	} else {
		f, err = os.OpenFile(t.memoryTraceFileName+".csv", os.O_APPEND|os.O_WRONLY, 0644)
	}

	if err != nil {
		panic(err)
	}
	defer f.Close()

	// 2. 시간 순 정렬
	times := make([]akita.VTimeInSec, 0, len(data))
	for ts := range data {
		times = append(times, ts)
	}
	slices.Sort(times)

	// 3. 데이터 출력
	for _, ts := range times {
		keyMap := data[ts]
		for key, chipletCounts := range keyMap {
			for cIdx, count := range chipletCounts {
				if count == 0 {
					continue
				}

				// RDMA 계산 로직
				isRdma := 0
				if key.owner != cIdx {
					isRdma = 1
				}

				dataID := global.GetInstance().LookupDataID(key.addr)

				// Fprintf를 writer에 수행
				fmt.Fprintf(f, "%.12f,%d,%d,%d,%d,%d,%d,%d,%d,%d,%d\n",
					ts,
					key.addr,
					key.pc,
					key.owner,
					key.rw,
					isRdma,
					key.isVector,
					key.accessType,
					cIdx,
					dataID,
					key.isPTW)
			}
		}
	}

	if dumpFromCall {
		t.memoryAccessPerTime = make(map[akita.VTimeInSec]map[accessInfo][]uint64)
		t.simulationStartTime = time.Now()
	}
}
