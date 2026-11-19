package tracing

import (
	"sort"
	"strings"

	"gitlab.com/akita/akita"
)

// TranslationReqTracer can collect the total time of executing a certain type of
// task. If the execution of two tasks overlaps, this tracer will simply add
// the two task processing time together.
type TranslationReqTracer struct {
	filter        TaskFilter
	averageTime   map[string]akita.VTimeInSec
	inflightTasks map[uint64]Task
	taskCount     map[string]uint64
}

// NewTranslationReqTracer creates a new TranslationReqTracer
func NewTranslationReqTracer(filter TaskFilter) *TranslationReqTracer {
	t := &TranslationReqTracer{
		filter:        filter,
		inflightTasks: make(map[uint64]Task),
		averageTime:   make(map[string]akita.VTimeInSec),
		taskCount:     make(map[string]uint64),
	}
	return t
}

// AverageTime returns the total time has been spent on a certain type of tasks.
func (t *TranslationReqTracer) AverageTime(taskType string) akita.VTimeInSec {
	time := t.averageTime[taskType]
	return time
}

// TotalCount returns the total number of tasks.
func (t *TranslationReqTracer) TotalCount(taskType string) uint64 {

	return t.taskCount[taskType]
}

// StartTask records the task start time
func (t *TranslationReqTracer) StartTask(task Task) {
	// if strings.Contains(task.ID, "trace-trans") {
	// 	fmt.Println("before filter", task)
	// }
	if !t.filter(task) {
		return
	}
	t.inflightTasks[task.ID] = task
}

// StepTask does nothing
func (t *TranslationReqTracer) StepTask(task Task) {
	// Do nothing
}

func getComponent(fullCompName string) (comp string) {
	split := strings.Split(fullCompName, ".")
	lenOfSplit := len(split)
	lastElement := split[lenOfSplit-1]
	comp = strings.Split(lastElement, "_")[0]
	return
}

func getTaskType(src Task, dst Task) (taskType string) {
	srcComponent := getComponent(src.Where)
	dstComponent := getComponent(dst.Where)
	taskType = srcComponent + "-" + dstComponent
	if srcComponent == dstComponent {
		panic("translation task with equal src/dst component: " + srcComponent)
	}
	return
}

// EndTask records the end of the task
func (t *TranslationReqTracer) EndTask(task Task) {
	originalTask, ok := t.inflightTasks[task.ID]
	// if strings.Contains(task.ID, "57847011") {
	// fmt.Println("boo")
	// }
	if !ok {
		return
	}
	taskType := getTaskType(originalTask, task)
	taskTime := task.EndTime - originalTask.StartTime
	t.averageTime[taskType] = akita.VTimeInSec(
		(float64(t.averageTime[taskType])*float64(t.taskCount[taskType]) + float64(taskTime)) /
			float64(t.taskCount[taskType]+1))
	t.taskCount[taskType]++
	delete(t.inflightTasks, task.ID)
}

// GetStepNames returns all the step names collected.
func (t *TranslationReqTracer) GetStepNames() (keys []string) {
	keys = make([]string, len(t.averageTime))
	i := 0
	for k := range t.averageTime {
		keys[i] = k
		i++
	}
	sort.Strings(keys)
	return
}
