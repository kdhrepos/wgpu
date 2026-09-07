// PteAccessTracer reports every access count per memory address
package tracing

import (
	"fmt"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"

	"gitlab.com/akita/akita"
)

type PteMetrics struct {
	Owner       int // owner chip of current address
	chiplets    []uint64
	IsRemotePTW int
	pc          any
	level       int
	PteIndex    uint64
}

type PTAccessInfo struct {
	PPN      uint64
	PteIndex uint64
	PC       any
	Level    int
}

type PteAccessTracer struct {
	// time -> addr -> PC -> pte metrics
	pteAccessPerTime map[akita.VTimeInSec]map[uint64]map[uint64]*PteMetrics
	numChiplets      int

	simulationStartTime time.Time
	colPrinted          bool
	pteTraceFileName    string
}

func NewPteAccessTracer(
	name string,
	xDim int,
	yDim int,
) *PteAccessTracer {
	numChiplets := xDim * yDim

	t := &PteAccessTracer{
		pteTraceFileName:    name,
		simulationStartTime: time.Now(),
		pteAccessPerTime:    make(map[akita.VTimeInSec]map[uint64]map[uint64]*PteMetrics),
		numChiplets:         numChiplets,
	}

	return t
}

func (t *PteAccessTracer) StartTask(task Task) {
	if task.Kind != "pte-trace" {
		return
	}

	info, ok := task.Detail.(*PTAccessInfo)
	if !ok {
		return
	}

	isRemotePTW := 0
	PC, _ := info.PC.(uint64)

	ppn := info.PPN
	current := task.StartTime

	srcChip, _ := strconv.Atoi(
		strings.TrimPrefix(
			strings.Split(task.Where, ".")[1],
			"chiplet_"))

	owner, _ := strconv.Atoi(
		strings.TrimPrefix(
			strings.Split(task.What, ".")[1],
			"chiplet_"))

	if srcChip != owner {
		isRemotePTW = 1
	}

	if _, ok := t.pteAccessPerTime[current]; !ok {
		t.pteAccessPerTime[current] = make(map[uint64]map[uint64]*PteMetrics)
	}

	if _, ok := t.pteAccessPerTime[current][ppn]; !ok {
		t.pteAccessPerTime[current][ppn] = make(map[uint64]*PteMetrics)
	}
	if _, ok := t.pteAccessPerTime[current][ppn][PC]; !ok {
		t.pteAccessPerTime[current][ppn][PC] = &PteMetrics{
			chiplets: make([]uint64, t.numChiplets),
		}
	}
	t.pteAccessPerTime[current][ppn][PC].chiplets[srcChip]++
	t.pteAccessPerTime[current][ppn][PC].Owner = owner
	t.pteAccessPerTime[current][ppn][PC].IsRemotePTW = isRemotePTW
	t.pteAccessPerTime[current][ppn][PC].pc = info.PC
	t.pteAccessPerTime[current][ppn][PC].level = info.Level
	t.pteAccessPerTime[current][ppn][PC].PteIndex = info.PteIndex

	if time.Since(t.simulationStartTime) >= time.Hour {
		dataToDump := t.pteAccessPerTime
		t.pteAccessPerTime = make(map[akita.VTimeInSec]map[uint64]map[uint64]*PteMetrics)
		t.simulationStartTime = time.Now()

		t.Dump(dataToDump)
	} else {
	}
}

func (t *PteAccessTracer) StepTask(task Task) {
	// Do nothing
}

func (t *PteAccessTracer) EndTask(task Task) {
	// Do nothing
}

func (t *PteAccessTracer) Dump(
	data map[akita.VTimeInSec]map[uint64]map[uint64]*PteMetrics,
) {

	dumpFromCall := false
	if data == nil {
		data = t.pteAccessPerTime
		dumpFromCall = true
	}

	var f *os.File
	var err error

	// 파일이 없으면 생성(Create), 있으면 이어쓰기(Append)
	if _, err := os.Stat(t.pteTraceFileName + ".csv"); os.IsNotExist(err) {
		f, err = os.Create(t.pteTraceFileName + ".csv")
		if err == nil {
			fmt.Fprintf(f, "time,ppn,pc,owner,chiplet,pt_index,level,remote\n")
		}
	} else {
		f, err = os.OpenFile(t.pteTraceFileName+".csv", os.O_APPEND|os.O_WRONLY, 0644)
	}

	if err != nil {
		panic(err)
	}
	defer f.Close()

	// 1. 시간 순 정렬
	times := make([]akita.VTimeInSec, 0, len(data))
	for time := range data {
		times = append(times, time)
	}
	slices.Sort(times)

	// 3. 데이터 출력
	for _, time := range times {
		// addrMap의 타입은 map[uint64]map[uint64]*Metrics
		ppnMap := data[time]

		// 첫 번째 uint64 키 (예: addr) 순회
		for ppn, pcMap := range ppnMap {
			// 두 번째 uint64 키 (예: PC, ReqID 등) 순회 -> 여기서 *Metrics를 얻음
			for pc, metric := range pcMap {
				for c := 0; c < len(metric.chiplets); c++ {
					if metric.chiplets[c] == 0 {
						continue
					}

					fmt.Fprintf(f, "%.10f,%d,%d,%d,%d,%d,%d,%d\n",
						time, ppn, pc,
						metric.Owner, c,
						metric.PteIndex, metric.level,
						metric.IsRemotePTW)
				}
			}
		}
	}

	if dumpFromCall {
		t.pteAccessPerTime = make(map[akita.VTimeInSec]map[uint64]map[uint64]*PteMetrics)
		t.simulationStartTime = time.Now()
	}
}
