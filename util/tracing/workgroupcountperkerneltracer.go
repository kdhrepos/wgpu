// workgroup count tracer reports the number of workgroup per a kernel
package tracing

type WorkgroupCountPerKernelTracer struct {
	filter                  TaskFilter
	workgroupCountPerKernel map[string]uint64
}

func NewWorkgroupCountPerKernelTracer(filter TaskFilter) *WorkgroupCountPerKernelTracer {
	t := &WorkgroupCountPerKernelTracer{
		filter:                  filter,
		workgroupCountPerKernel: make(map[string]uint64),
	}

	return t
}

func (t *WorkgroupCountPerKernelTracer) StartTask(task Task) {
	if !t.filter(task) {
		return
	}
	numWorkgroup := task.Detail.(uint64)

	t.workgroupCountPerKernel[task.What] = numWorkgroup
}

func (t *WorkgroupCountPerKernelTracer) StepTask(task Task) {
	// Do nothing
}

func (t *WorkgroupCountPerKernelTracer) EndTask(task Task) {
	// Do nothing
}

func (t *WorkgroupCountPerKernelTracer) GetWorkgroupCountPerKernel() map[string]uint64 {
	return t.workgroupCountPerKernel
}
