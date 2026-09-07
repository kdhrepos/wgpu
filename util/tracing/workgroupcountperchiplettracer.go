// workgroup count tracer reports the number of workgroup per a kernel
package tracing

import (
	"strconv"
)

type WorkgroupCountPerChipletTracer struct {
	filter                  TaskFilter
	workgroupCountPeChiplet []uint64
}

func NewWorkgroupCountPerChipletTracer(
	numChiplets int,
	filter TaskFilter,
) *WorkgroupCountPerChipletTracer {
	t := &WorkgroupCountPerChipletTracer{
		filter:                  filter,
		workgroupCountPeChiplet: make([]uint64, numChiplets),
	}

	return t
}

func (t *WorkgroupCountPerChipletTracer) StartTask(task Task) {
	if !t.filter(task) {
		return
	}
	numWorkgroup := task.Detail.(uint64)
	chipletId, _ := strconv.Atoi(task.What)

	t.workgroupCountPeChiplet[chipletId] += numWorkgroup
}

func (t *WorkgroupCountPerChipletTracer) StepTask(task Task) {
	// Do nothing
}

func (t *WorkgroupCountPerChipletTracer) EndTask(task Task) {
	// Do nothing
}

func (t *WorkgroupCountPerChipletTracer) GetWorkgroupCountPerChiplet() []uint64 {
	return t.workgroupCountPeChiplet
}

// func (t *WorkgroupCountTracer) Dump(name string) {
// 	f, err := os.Create(name + ".csv")
// 	if err != nil {
// 		panic(fmt.Sprintf("%e at WorkgroupCountTracer, Dump", err))
// 	}
// 	defer f.Close()

// 	addrs := make([]uint64, 0, len(t.accessCountPerAddrs))
// 	for addr := range t.accessCountPerAddrs {
// 		addrs = append(addrs, addr)
// 	}
// 	sort.Slice(addrs, func(i, j int) bool {
// 		return addrs[i] < addrs[j]
// 	})

// 	fmt.Fprintf(f, ", address, count\n")
// 	for i, addr := range addrs {
// 		fmt.Fprintf(f, "%d, %d, %d\n",
// 			i,
// 			addr,
// 			t.accessCountPerAddrs[addr])
// 	}
// }
