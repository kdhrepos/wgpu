// MSHRFailTracer counts MSHR reservation failures in the cache directory
// stage and tracks how many requests end up merged into a single MSHR
// entry when that entry is finally pushed to the mshr stage buffer. A
// "fail" is recorded whenever the directory stage observes a read miss
// or a partial-line write miss while the MSHR is already full. A
// "merge-fail" is recorded whenever a miss hits an existing entry that is
// already at its per-entry max-merge cap, so the request cannot coalesce.
// A "merge" event records len(mshrEntry.Requests) at the moment the
// write-buffer stage wakes the entry up — i.e., the final number of
// requests that coalesced under the same outstanding miss.
// A "mshr-wait-time" event records how long (in sim seconds) a request
// waited from its first MSHR-full stall until an MSHR slot was finally
// allocated for it.
package tracing

type MSHRFailTracer struct {
	readFailCount  uint64
	writeFailCount uint64

	readMergeFailCount  uint64
	writeMergeFailCount uint64

	chipletIDCache        map[string]int
	readFailPerChip       map[int]uint64
	writeFailPerChip      map[int]uint64
	readMergeFailPerChip  map[int]uint64
	writeMergeFailPerChip map[int]uint64

	// merge stats
	mergeEventCount    uint64
	mergeRequestsTotal uint64
	mergeHistogram     map[int]uint64
	mergeBySource      map[string]uint64 // "buffer" | "dram" | "rdma"
	mergeReqsBySource  map[string]uint64
	mergeTotalPerChip  map[int]uint64
	mergeReqsPerChip   map[int]uint64

	// wait-time stats: time from first MSHR-full stall to MSHR slot allocation
	waitTimeTotal      float64
	waitTimeCount      uint64
	waitTimeTotalPerChip map[int]float64
	waitTimeCountPerChip map[int]uint64
}

func NewMSHRFailTracer() *MSHRFailTracer {
	return &MSHRFailTracer{
		chipletIDCache:        make(map[string]int),
		readFailPerChip:       make(map[int]uint64),
		writeFailPerChip:      make(map[int]uint64),
		readMergeFailPerChip:  make(map[int]uint64),
		writeMergeFailPerChip: make(map[int]uint64),
		mergeHistogram:        make(map[int]uint64),
		mergeBySource:         make(map[string]uint64),
		mergeReqsBySource:     make(map[string]uint64),
		mergeTotalPerChip:     make(map[int]uint64),
		mergeReqsPerChip:      make(map[int]uint64),
		waitTimeTotalPerChip:  make(map[int]float64),
		waitTimeCountPerChip:  make(map[int]uint64),
	}
}

func (t *MSHRFailTracer) StartTask(task Task) {
	switch task.Kind {
	case "mshr-fail":
		t.recordFail(task)
	case "mshr-merge-fail":
		t.recordMergeFail(task)
	case "mshr-merge":
		t.recordMerge(task)
	case "mshr-wait-time":
		t.recordWaitTime(task)
	}
}

func (t *MSHRFailTracer) recordFail(task Task) {

	srcChip := t.chipletIDLocked(task.Where)

	switch task.What {
	case "read":
		t.readFailCount++
		t.readFailPerChip[srcChip]++
	case "write":
		t.writeFailCount++
		t.writeFailPerChip[srcChip]++
	}
}

func (t *MSHRFailTracer) recordMergeFail(task Task) {

	srcChip := t.chipletIDLocked(task.Where)

	switch task.What {
	case "read":
		t.readMergeFailCount++
		t.readMergeFailPerChip[srcChip]++
	case "write":
		t.writeMergeFailCount++
		t.writeMergeFailPerChip[srcChip]++
	}
}

func (t *MSHRFailTracer) recordMerge(task Task) {
	count, ok := task.Detail.(int)
	if !ok || count <= 0 {
		return
	}

	srcChip := t.chipletIDLocked(task.Where)

	t.mergeEventCount++
	t.mergeRequestsTotal += uint64(count)
	t.mergeHistogram[count]++
	t.mergeBySource[task.What]++
	t.mergeReqsBySource[task.What] += uint64(count)
	t.mergeTotalPerChip[srcChip]++
	t.mergeReqsPerChip[srcChip] += uint64(count)
}

func (t *MSHRFailTracer) recordWaitTime(task Task) {
	wait, ok := task.Detail.(float64)
	if !ok || wait < 0 {
		return
	}
	srcChip := t.chipletIDLocked(task.Where)
	t.waitTimeTotal += wait
	t.waitTimeCount++
	t.waitTimeTotalPerChip[srcChip] += wait
	t.waitTimeCountPerChip[srcChip]++
}

func (t *MSHRFailTracer) chipletIDLocked(where string) int {
	srcChip, ok := t.chipletIDCache[where]
	if !ok {
		srcChip = parseChipletIDFromComponentName(where)
		t.chipletIDCache[where] = srcChip
	}
	return srcChip
}

func (t *MSHRFailTracer) StepTask(task Task) {
	// Do nothing
}

func (t *MSHRFailTracer) EndTask(task Task) {
	// Do nothing
}

func (t *MSHRFailTracer) GetReadFailCount() uint64 {
	return t.readFailCount
}

func (t *MSHRFailTracer) GetWriteFailCount() uint64 {
	return t.writeFailCount
}

func (t *MSHRFailTracer) GetReadFailPerChiplet() map[int]uint64 {
	return t.readFailPerChip
}

func (t *MSHRFailTracer) GetWriteFailPerChiplet() map[int]uint64 {
	return t.writeFailPerChip
}

func (t *MSHRFailTracer) GetReadMergeFailCount() uint64 {
	return t.readMergeFailCount
}

func (t *MSHRFailTracer) GetWriteMergeFailCount() uint64 {
	return t.writeMergeFailCount
}

func (t *MSHRFailTracer) GetReadMergeFailPerChiplet() map[int]uint64 {
	return t.readMergeFailPerChip
}

func (t *MSHRFailTracer) GetWriteMergeFailPerChiplet() map[int]uint64 {
	return t.writeMergeFailPerChip
}

func (t *MSHRFailTracer) GetMergeEventCount() uint64 {
	return t.mergeEventCount
}

func (t *MSHRFailTracer) GetMergeRequestsTotal() uint64 {
	return t.mergeRequestsTotal
}

func (t *MSHRFailTracer) GetMergeHistogram() map[int]uint64 {
	return t.mergeHistogram
}

func (t *MSHRFailTracer) GetMergeEventsBySource() map[string]uint64 {
	return t.mergeBySource
}

func (t *MSHRFailTracer) GetMergeRequestsBySource() map[string]uint64 {
	return t.mergeReqsBySource
}

func (t *MSHRFailTracer) GetMergeEventsPerChiplet() map[int]uint64 {
	return t.mergeTotalPerChip
}

func (t *MSHRFailTracer) GetMergeRequestsPerChiplet() map[int]uint64 {
	return t.mergeReqsPerChip
}

func (t *MSHRFailTracer) GetWaitTimeTotal() float64 {
	return t.waitTimeTotal
}

func (t *MSHRFailTracer) GetWaitTimeCount() uint64 {
	return t.waitTimeCount
}

func (t *MSHRFailTracer) GetWaitTimeAvg() float64 {
	if t.waitTimeCount == 0 {
		return 0
	}
	return t.waitTimeTotal / float64(t.waitTimeCount)
}

func (t *MSHRFailTracer) GetWaitTimeTotalPerChiplet() map[int]float64 {
	return t.waitTimeTotalPerChip
}

func (t *MSHRFailTracer) GetWaitTimeCountPerChiplet() map[int]uint64 {
	return t.waitTimeCountPerChip
}
