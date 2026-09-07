// workgroup count tracer reports the number of workgroup per a kernel
package tracing

type WorkgroupDimension struct {
	gridSizeX  int
	gridSizeY  int
	gridSizeZ  int
	workgroupX int
	workgroupY int
	workgroupZ int
	gridNumX   int
	gridNumY   int
	gridNumZ   int
}

type WorkgroupDimensionTracer struct {
	filter                      TaskFilter
	workgroupDimensionPerKernel map[uint64]*WorkgroupDimension
}

func NewWorkgroupDimensionTracer(filter TaskFilter) *WorkgroupDimensionTracer {
	t := &WorkgroupDimensionTracer{
		filter:                      filter,
		workgroupDimensionPerKernel: make(map[uint64]*WorkgroupDimension),
	}

	return t
}

func (t *WorkgroupDimensionTracer) StartTask(task Task) {
	if !t.filter(task) {
		return
	}

	value := task.Detail.(int)

	switch task.What {
	case "workgroup-dimension-grid-size-x":
		t.workgroupDimensionPerKernel[task.ID] = &WorkgroupDimension{}
		t.workgroupDimensionPerKernel[task.ID].gridSizeX = value
	case "workgroup-dimension-grid-size-y":
		t.workgroupDimensionPerKernel[task.ID].gridSizeY = value
	case "workgroup-dimension-grid-size-z":
		t.workgroupDimensionPerKernel[task.ID].gridSizeZ = value
	case "workgroup-size-x":
		t.workgroupDimensionPerKernel[task.ID].workgroupX = value
	case "workgroup-size-y":
		t.workgroupDimensionPerKernel[task.ID].workgroupY = value
	case "workgroup-size-z":
		t.workgroupDimensionPerKernel[task.ID].workgroupZ = value
	case "grid-dimension-x":
		t.workgroupDimensionPerKernel[task.ID].gridNumX = value
	case "grid-dimension-y":
		t.workgroupDimensionPerKernel[task.ID].gridNumY = value
	case "grid-dimension-z":
		t.workgroupDimensionPerKernel[task.ID].gridNumZ = value
	}
}

func (t *WorkgroupDimensionTracer) StepTask(task Task) {
	// Do nothing
}

func (t *WorkgroupDimensionTracer) EndTask(task Task) {
	// Do nothing
}

func (t *WorkgroupDimensionTracer) GetNumKernels() int {
	return len(t.workgroupDimensionPerKernel)
}

func (t *WorkgroupDimensionTracer) GetGridSizeX(kernelID int) int {
	return t.workgroupDimensionPerKernel[uint64(kernelID)].gridSizeX
}

func (t *WorkgroupDimensionTracer) GetGridSizeY(kernelID int) int {
	return t.workgroupDimensionPerKernel[uint64(kernelID)].gridSizeY
}

func (t *WorkgroupDimensionTracer) GetGridSizeZ(kernelID int) int {
	return t.workgroupDimensionPerKernel[uint64(kernelID)].gridSizeZ
}

func (t *WorkgroupDimensionTracer) GetWorkgroupX(kernelID int) int {
	return t.workgroupDimensionPerKernel[uint64(kernelID)].workgroupX
}

func (t *WorkgroupDimensionTracer) GetWorkgroupY(kernelID int) int {
	return t.workgroupDimensionPerKernel[uint64(kernelID)].workgroupY
}

func (t *WorkgroupDimensionTracer) GetWorkgroupZ(kernelID int) int {
	return t.workgroupDimensionPerKernel[uint64(kernelID)].workgroupZ
}

func (t *WorkgroupDimensionTracer) GetGridNumX(kernelID int) int {
	return t.workgroupDimensionPerKernel[uint64(kernelID)].gridNumX
}

func (t *WorkgroupDimensionTracer) GetGridNumY(kernelID int) int {
	return t.workgroupDimensionPerKernel[uint64(kernelID)].gridNumY
}

func (t *WorkgroupDimensionTracer) GetGridNumZ(kernelID int) int {
	return t.workgroupDimensionPerKernel[uint64(kernelID)].gridNumZ
}
