package tracing

type FlushTracer struct {
	taskFilter      TaskFilter
	xDim            int
	yDim            int
	inflightFlushes map[uint64]*Task
}

func NewFlushTracer(
	taskFilter TaskFilter,
	xDim int,
	yDim int,
) *FlushTracer {
	t := &FlushTracer{
		taskFilter:      taskFilter,
		xDim:            xDim,
		yDim:            yDim,
		inflightFlushes: make(map[uint64]*Task),
	}

	return t
}

func (t *FlushTracer) StartTask(task Task) {
	if !t.taskFilter(task) {
		return
	}

	t.inflightFlushes[task.ID] = &task
}

func (t *FlushTracer) StepTask(task Task) {
	// Do Nothing
}

func (t *FlushTracer) EndTask(task Task) {

	flush, ok := t.inflightFlushes[task.ID]
	if !ok {
		return
	}

	flush.EndTime = task.EndTime
}

func (t *FlushTracer) GetTotalFlushes() map[uint64]*Task {
	return t.inflightFlushes
}
