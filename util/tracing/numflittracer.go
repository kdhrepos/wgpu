package tracing

// NumFlitTracer counts the total number of flits that unique messages are
// split into. The same msg ID is recorded at most once, so emissions from
// multiple switches along a hop chain do not double-count.
type NumFlitTracer struct {
	taskFilter TaskFilter
	perMsg     map[uint64]uint64
}

func NewNumFlitTracer(taskFilter TaskFilter) *NumFlitTracer {
	return &NumFlitTracer{
		taskFilter: taskFilter,
		perMsg:     make(map[uint64]uint64),
	}
}

func (t *NumFlitTracer) StartTask(task Task) {
	if !t.taskFilter(task) {
		return
	}
	if _, ok := t.perMsg[task.ID]; ok {
		return
	}
	if n, ok := task.Detail.(uint64); ok {
		t.perMsg[task.ID] = n
	}
}

func (t *NumFlitTracer) StepTask(task Task) {}

func (t *NumFlitTracer) EndTask(task Task) {}

func (t *NumFlitTracer) GetNumFlits() uint64 {
	var total uint64
	for _, n := range t.perMsg {
		total += n
	}
	return total
}
