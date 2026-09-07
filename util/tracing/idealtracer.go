package tracing

// IdealTracer tracks direct vs normal request sends in IdealRdmaEngine
type IdealTracer struct {
	directCount uint64
	normalCount uint64
}

func NewIdealTracer() *IdealTracer {
	t := &IdealTracer{}
	return t
}

func (t *IdealTracer) StartTask(task Task) {
	switch task.Kind {
	case "ideal-direct":
		t.directCount++
	case "ideal-normal":
		t.normalCount++
	}
}

func (t *IdealTracer) StepTask(task Task) {
	// Do Nothing
}

func (t *IdealTracer) EndTask(task Task) {
	// Do Nothing
}

func (t *IdealTracer) GetDirectCount() uint64 {
	return t.directCount
}

func (t *IdealTracer) GetNormalCount() uint64 {
	return t.normalCount
}
