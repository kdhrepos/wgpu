// LocalRemoteTracer reports every access count per memory address
package tracing

import (
	"strconv"
)

type LocalRemoteTracer struct {
	// time -> addr -> PC -> metrics
	localAccessCount  int
	remoteAccessCount int

	chipletIDCache map[string]int
}

func NewLocalRemoteTracer() *LocalRemoteTracer {
	t := &LocalRemoteTracer{
		chipletIDCache: make(map[string]int),
	}

	return t
}

func (t *LocalRemoteTracer) StartTask(task Task) {
	if task.Kind != "memory-trace" {
		return
	}

	srcChip, ok := t.chipletIDCache[task.Where]
	if !ok {
		srcChip = parseChipletIDFromComponentName(task.Where)
		t.chipletIDCache[task.Where] = srcChip
	}

	owner, _ := strconv.Atoi(task.What)

	if srcChip == owner {
		t.localAccessCount++
	} else {
		t.remoteAccessCount++
	}
}

func (t *LocalRemoteTracer) StepTask(task Task) {
	// Do nothing
}

func (t *LocalRemoteTracer) EndTask(task Task) {
	// Do nothing
}

func (t *LocalRemoteTracer) GetLocalAccessCount() int {
	return t.localAccessCount
}

func (t *LocalRemoteTracer) GetRemoteAccessCount() int {
	return t.remoteAccessCount
}
