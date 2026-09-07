package tracing

import (
	"encoding/json"
	"fmt"
	"io"
	"os"

	"github.com/rs/xid"
	"github.com/tebeka/atexit"
)

// JSONTracer can write tasks into json format.
type JSONTracer struct {
	w             io.Writer
	firstTask     bool
	inflightTasks map[uint64]Task
}

// StartTask records the start of a task
func (t *JSONTracer) StartTask(task Task) {
	t.inflightTasks[task.ID] = task
}

// StepTask records the moment that a task reaches a milestone
func (t *JSONTracer) StepTask(task Task) {
	// Do nothing right now
}

// EndTask records the time that a task is completed.
func (t *JSONTracer) EndTask(task Task) {
	originalTask, ok := t.inflightTasks[task.ID]
	if !ok {
		return
	}
	originalTask.EndTime = task.EndTime

	delete(t.inflightTasks, task.ID)

	if t.firstTask {
		t.firstTask = false
	} else {
		t.w.Write([]byte(",\n"))
	}

	b, err := json.Marshal(originalTask)
	if err != nil {
		panic(err)
	}
	t.w.Write(b)
}

func (t *JSONTracer) finish() {
	t.w.Write([]byte("\n]"))
}

// NewJSONTracer creates a new JsonTracer, injecting a writer
// as dependency.
func NewJSONTracer() *JSONTracer {
	filename := xid.New().String() + ".json"
	f, err := os.Create(filename)
	if err != nil {
		panic(err)
	}
	fmt.Printf("Recodring tasks in %s\n", filename)

	f.Write([]byte("[\n"))

	t := &JSONTracer{
		w:             f,
		firstTask:     true,
		inflightTasks: make(map[uint64]Task),
	}

	atexit.Register(t.finish)

	return t
}
