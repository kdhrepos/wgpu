package tracing

import (
	"gitlab.com/akita/akita"
)

// TotalTimeTracer can collect the total time of executing a certain type of
// task. If the execution of two tasks overlaps, this tracer will simply add
// the two task processing time together.
type TotalTimeTracer struct {
	filter        TaskFilter
	totalTime     akita.VTimeInSec
	inflightTasks map[uint64]Task
}

// NewTotalTimeTracer creates a new TotalTimeTracer
func NewTotalTimeTracer(filter TaskFilter) *TotalTimeTracer {
	t := &TotalTimeTracer{
		filter:        filter,
		inflightTasks: make(map[uint64]Task),
	}
	return t
}

// TotalTime returns the total time has been spent on a certain type of tasks.
func (t *TotalTimeTracer) TotalTime() akita.VTimeInSec {
	time := t.totalTime
	return time
}

// StartTask records the task start time
func (t *TotalTimeTracer) StartTask(task Task) {
	if !t.filter(task) {
		return
	}

	t.inflightTasks[task.ID] = task
}

// StepTask does nothing
func (t *TotalTimeTracer) StepTask(task Task) {
	// Do nothing
}

// EndTask records the end of the task
func (t *TotalTimeTracer) EndTask(task Task) {
	originalTask, ok := t.inflightTasks[task.ID]
	if !ok {
		return
	}

	t.totalTime += task.EndTime - originalTask.StartTime
	delete(t.inflightTasks, task.ID)
}
