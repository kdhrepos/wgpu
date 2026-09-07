package tracing

type MemoryUsageTracer struct {
	taskFilter         TaskFilter
	memoryUsagePerData []float64 // data id : memory usage in mb
	numDataAllocated   uint64
}

func NewMemoryUsageTracer(taskFilter TaskFilter) *MemoryUsageTracer {
	t := &MemoryUsageTracer{
		taskFilter:         taskFilter,
		memoryUsagePerData: make(([]float64), 10000),
	}

	return t
}

func (t *MemoryUsageTracer) StartTask(task Task) {
	if !t.taskFilter(task) {
		return
	}

	memoryUsage := task.Detail.(float64)
	memoryUsage /= (1 << 20) // in MB

	t.memoryUsagePerData[t.numDataAllocated] = memoryUsage
	t.numDataAllocated++
}

func (t *MemoryUsageTracer) StepTask(task Task) {
	// Do nothing
}

func (t *MemoryUsageTracer) EndTask(task Task) {
	// Do nothing
}

func (t *MemoryUsageTracer) GetTotalMemoryUsage() float64 {
	totalMemoryUsage := float64(0)

	for i := 0; i < int(t.numDataAllocated); i++ {
		totalMemoryUsage += t.memoryUsagePerData[i]
	}

	return totalMemoryUsage
}

func (t *MemoryUsageTracer) GetNumDataAllocated() uint64 {
	return t.numDataAllocated
}

func (t *MemoryUsageTracer) GetMemoryUsagePerData() []float64 {
	return t.memoryUsagePerData
}
