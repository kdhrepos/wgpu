package tracing

type PCTableTracer struct {
	lookupCount     uint64
	lookupMissCount uint64
}

func NewPCTableTracer() *PCTableTracer {
	t := &PCTableTracer{}

	return t
}

func (t *PCTableTracer) StartTask(task Task) {

	switch task.Kind {
	case "pc-table-lookup":
		t.lookupCount++
	case "pc-table-lookup-miss":
		t.lookupMissCount++
	}
}

func (t *PCTableTracer) StepTask(task Task) {
	// Do Nothing
}

func (t *PCTableTracer) EndTask(task Task) {
	// Do Nothing
}

func (t *PCTableTracer) GetLookupCount() uint64 {
	return t.lookupCount
}

func (t *PCTableTracer) GetLookupMissCount() uint64 {
	return t.lookupMissCount
}
