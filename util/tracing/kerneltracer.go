// KernelTracer reports every access count per memory address
package tracing

import (
	"gitlab.com/akita/akita"
)

type kernelInfo struct {
	launchTime    akita.VTimeInSec
	terminateTime akita.VTimeInSec
	basePC        uint64
}

type KernelTracer struct {
	taskFilter TaskFilter
	kernel     []*kernelInfo
	numKernels int
}

func NewKernelTracer(taskFilter TaskFilter) *KernelTracer {
	t := &KernelTracer{
		taskFilter: taskFilter,
		kernel:     []*kernelInfo{},
	}

	return t
}

func (t *KernelTracer) StartTask(task Task) {
	if !t.taskFilter(task) {
		return
	}

	switch task.What {
	case "launch":
		kInfo := &kernelInfo{
			launchTime: task.StartTime,
		}
		t.kernel = append(t.kernel, kInfo)
		t.numKernels++

		basePC, ok := task.Detail.(uint64)
		if ok {
			kInfo.basePC = basePC
		}

	case "terminate":
		t.kernel[t.numKernels-1].terminateTime = task.StartTime
	}
}

func (t *KernelTracer) StepTask(task Task) {
	// Do nothing
}

func (t *KernelTracer) EndTask(task Task) {
	// Do nothing
}

func (t *KernelTracer) GetKernelLaunchTimes() []akita.VTimeInSec {
	launchTimes := make([]akita.VTimeInSec, t.numKernels)
	for i := 0; i < t.numKernels; i++ {
		launchTimes[i] = t.kernel[i].launchTime
	}
	return launchTimes
}

func (t *KernelTracer) GetKernelTerminateTimes() []akita.VTimeInSec {
	terminateTimes := make([]akita.VTimeInSec, t.numKernels)
	for i := 0; i < t.numKernels; i++ {
		terminateTimes[i] = t.kernel[i].terminateTime
	}
	return terminateTimes
}

func (t *KernelTracer) GetBasePCList() []uint64 {
	pcList := make([]uint64, t.numKernels)
	for i := 0; i < t.numKernels; i++ {
		pcList[i] = t.kernel[i].basePC
	}
	return pcList
}

func (t *KernelTracer) GetNumKernels() int {
	return t.numKernels
}
