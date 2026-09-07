// PCStateTracer reports the state of PC
package tracing

import (
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"gitlab.com/akita/akita"
	"gitlab.com/akita/protocol"
)

type PCStateTrace struct {
	Counts     [4]uint16
	FromRemote bool
	PCState    [4]protocol.Hotness

	LookupCount     uint16
	LookupMissCount uint16
	IsWrite         bool
	ChipId          int
}

type PCStateTracer struct {
	PCStateTraceList map[akita.VTimeInSec]map[uint64]*PCStateTrace
	taskFilter       TaskFilter

	simulationStartTime  time.Time
	colPrinted           bool
	pcStateTraceFileName string
}

func NewPCStateTracer(
	taskFilter TaskFilter,
	pcStateTraceFileName string,
) *PCStateTracer {
	t := &PCStateTracer{
		taskFilter:           taskFilter,
		pcStateTraceFileName: pcStateTraceFileName,
		PCStateTraceList:     make(map[akita.VTimeInSec]map[uint64]*PCStateTrace),
	}

	return t
}

func (t *PCStateTracer) StartTask(task Task) {
	if !t.taskFilter(task) {
		return
	}

	pcStateTrace, ok := task.Detail.(*PCStateTrace)
	if !ok {
		return
	}

	chipId, _ := strconv.Atoi(
		strings.TrimPrefix(
			strings.Split(task.Where, ".")[1],
			"chiplet_"))

	if t.PCStateTraceList[task.StartTime] == nil {
		t.PCStateTraceList[task.StartTime] = make(map[uint64]*PCStateTrace)
	}
	t.PCStateTraceList[task.StartTime][task.ID] = pcStateTrace
	t.PCStateTraceList[task.StartTime][task.ID].ChipId = chipId

	if time.Since(t.simulationStartTime) >= time.Hour {
		t.Dump()
		t.colPrinted = true
		t.simulationStartTime = time.Now()
		t.PCStateTraceList = make(map[akita.VTimeInSec]map[uint64]*PCStateTrace)
	}
}

func (t *PCStateTracer) StepTask(task Task) {
	// Do nothing
}

func (t *PCStateTracer) EndTask(task Task) {
	// Do nothing
}

func (t *PCStateTracer) Dump() {
	var f *os.File
	var err error

	if !t.colPrinted {
		f, err = os.Create(t.pcStateTraceFileName + ".csv")
	} else {
		f, err = os.OpenFile(
			t.pcStateTraceFileName+".csv",
			os.O_APPEND|os.O_CREATE|os.O_WRONLY,
			0644,
		)
	}

	if err != nil {
		panic(fmt.Sprintf("%v at MemoryAccessTracer, Dump", err))
	}
	defer f.Close()

	times := make([]akita.VTimeInSec, 0, len(t.PCStateTraceList))
	for tm := range t.PCStateTraceList {
		times = append(times, tm)
	}
	sort.Slice(times, func(i, j int) bool {
		return times[i] < times[j]
	})

	if !t.colPrinted {
		fmt.Fprintf(f, "time,chiplet,pc,isWrite,fromRemote,lookupCount,lookupMissCount,pcs0,pcs1,pcs2,pcs3,cls0,cls1,cls2,cls3,total\n")
	}

	for _, tm := range times {
		traceMap := t.PCStateTraceList[tm]

		for pc, trace := range traceMap {
			var total uint64
			// 구조체 필드명이 대문자인 경우에 맞춰 수정 (Counts)
			for _, count := range trace.Counts {
				total += uint64(count)
			}

			fmt.Fprintf(
				f,
				"%f,%d,%d,%t,%t,%d,%d,%d,%d,%d,%d,%d,%d,%d,%d,%d\n",
				tm,
				trace.ChipId, // 구조체 필드명 대문자 확인 (ChipId)
				pc,
				trace.IsWrite,
				trace.FromRemote,
				trace.LookupCount,
				trace.LookupMissCount,
				trace.PCState[0],
				trace.PCState[1],
				trace.PCState[2],
				trace.PCState[3],
				trace.Counts[0],
				trace.Counts[1],
				trace.Counts[2],
				trace.Counts[3],
				total,
			)
		}
	}
}
