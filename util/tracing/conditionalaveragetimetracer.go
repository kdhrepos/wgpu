package tracing

import (
	"sort"

	//"fmt"

	"gitlab.com/akita/akita"
)

// ConditionalAverageTimeTracer can collect the total time of executing a certain type of
// task. If the execution of two tasks overlaps, this tracer will simply add
// the two task processing time together.
type ConditionalAverageTimeTracer struct {
	filter        TaskFilter
	averageTime   map[string]akita.VTimeInSec
	inflightTasks map[uint64]Task
	taskCount     map[string]uint64
}

// NewConditionalAverageTimeTracer creates a new ConditionalAverageTimeTracer
func NewConditionalAverageTimeTracer(filter TaskFilter) *ConditionalAverageTimeTracer {
	t := &ConditionalAverageTimeTracer{
		filter:        filter,
		inflightTasks: make(map[uint64]Task),
		averageTime:   make(map[string]akita.VTimeInSec),
		taskCount:     make(map[string]uint64),
	}
	return t
}

// AverageTime returns the total time has been spent on a certain type of tasks.
func (t *ConditionalAverageTimeTracer) AverageTime(taskType string) akita.VTimeInSec {
	time := t.averageTime[taskType]
	return time
}

// TotalCount returns the total number of tasks.
func (t *ConditionalAverageTimeTracer) TotalCount(taskType string) uint64 {

	return t.taskCount[taskType]
}

// StartTask records the task start time
func (t *ConditionalAverageTimeTracer) StartTask(task Task) {
	if !t.filter(task) {
		return
	}

	t.inflightTasks[task.ID] = task
}

// StepTask does nothing
func (t *ConditionalAverageTimeTracer) StepTask(task Task) {
	// Do nothing
	originalTask, ok := t.inflightTasks[task.ID]
	if !ok {
		return
	}
	taskType := task.Steps[0].What
	//fmt.Println(taskType)
	taskTime := task.Steps[0].Time - originalTask.StartTime
	// if taskType == "translation-latency" && strings.Contains(originalTask.Where, "L1VAddrTrans") {
	// fmt.Println(taskType, originalTask.Where, taskTime*1000000000)
	// }
	t.averageTime[taskType] = akita.VTimeInSec(
		(float64(t.averageTime[taskType])*float64(t.taskCount[taskType]) + float64(taskTime)) /
			float64(t.taskCount[taskType]+1))
	t.taskCount[taskType]++
}

// EndTask records the end of the task
func (t *ConditionalAverageTimeTracer) EndTask(task Task) {

	delete(t.inflightTasks, task.ID)
}

// GetStepNames returns all the step names collected.
func (t *ConditionalAverageTimeTracer) GetStepNames() (keys []string) {
	keys = make([]string, len(t.averageTime))
	i := 0
	for k := range t.averageTime {
		keys[i] = k
		i++
	}
	sort.Strings(keys)
	return
}
