package tracing

import (
	"strings"
)

type InterClusterTracer struct {
	interClusterReqCount map[string]map[string]int
}

func NewInterClusterTracer() *InterClusterTracer {
	return &InterClusterTracer{
		interClusterReqCount: make(map[string]map[string]int),
	}
}

func (t *InterClusterTracer) StartTask(task Task) {
	if task.Kind != "inter-cluster-trace" {
		return
	}

	srcClusterId := strings.Split(task.What, ".")[0]
	dstClusterId := strings.Split(task.What, ".")[1]

	if t.interClusterReqCount[srcClusterId] == nil {
		t.interClusterReqCount[srcClusterId] = make(map[string]int)
	}
	t.interClusterReqCount[srcClusterId][dstClusterId]++
}

func (t *InterClusterTracer) StepTask(task Task) {
	// Do nothing
}

func (t *InterClusterTracer) EndTask(task Task) {
	// Do nothing
}

func (t *InterClusterTracer) GetInterClusterReqCount() map[string]map[string]int {
	return t.interClusterReqCount
}
