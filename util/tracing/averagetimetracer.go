package tracing

import (
	"gitlab.com/akita/akita"
)

// AverageTimeTracer can collect the total time of executing a certain type of
// task. If the execution of two tasks overlaps, this tracer will simply add
// the two task processing time together.
type AverageTimeTracer struct {
	filter        TaskFilter
	averageTime   akita.VTimeInSec
	inflightTasks map[uint64]akita.VTimeInSec // task ID -> start time
	taskCount     uint64
}

// NewAverageTimeTracer creates a new AverageTimeTracer
func NewAverageTimeTracer(filter TaskFilter) *AverageTimeTracer {
	t := &AverageTimeTracer{
		filter:        filter,
		inflightTasks: make(map[uint64]akita.VTimeInSec),
	}
	return t
}

// AverageTime returns the total time has been spent on a certain type of tasks.
func (t *AverageTimeTracer) AverageTime() akita.VTimeInSec {
	time := t.averageTime
	return time
}

// TotalCount returns the total number of tasks.
func (t *AverageTimeTracer) TotalCount() uint64 {

	return t.taskCount
}

// StartTask records the task start time
func (t *AverageTimeTracer) StartTask(task Task) {
	if !t.filter(task) {
		return
	}

	t.inflightTasks[task.ID] = task.StartTime
}

// StepTask does nothing
func (t *AverageTimeTracer) StepTask(task Task) {
	// Do nothing
}

// EndTask records the end of the task
func (t *AverageTimeTracer) EndTask(task Task) {
	startTime, ok := t.inflightTasks[task.ID]
	if !ok {
		return
	}

	taskTime := task.EndTime - startTime
	t.averageTime = akita.VTimeInSec(
		(float64(t.averageTime)*float64(t.taskCount) + float64(taskTime)) /
			float64(t.taskCount+1))
	delete(t.inflightTasks, task.ID)
	t.taskCount++
}
