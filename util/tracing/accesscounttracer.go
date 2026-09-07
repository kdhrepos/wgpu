// accesscounttracer reports every access count per memory address
package tracing

import (
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"

	"gitlab.com/akita/protocol"
)

type AccessCountTracer struct {
	accessCountPerAddrs map[uint64]uint64 // address : access_count
	sharedByChipPerAddr map[uint64]map[int]struct{}
}

func NewAccessCountTracer() *AccessCountTracer {
	t := &AccessCountTracer{
		accessCountPerAddrs: make(map[uint64]uint64),
		sharedByChipPerAddr: make(map[uint64]map[int]struct{}),
	}

	return t
}

func (t *AccessCountTracer) StartTask(task Task) {
	req, ok := task.Detail.(protocol.AccessReq)
	if !ok {
		return
	}

	addr := req.GetAddress()
	t.accessCountPerAddrs[addr]++

	srcChip, _ := strconv.Atoi(
		strings.TrimPrefix(
			strings.Split(task.Where, ".")[1],
			"chiplet_"))

	if _, ok := t.sharedByChipPerAddr[addr]; !ok {
		t.sharedByChipPerAddr[addr] = make(map[int]struct{})
	}
	t.sharedByChipPerAddr[addr][srcChip] = struct{}{}
}

func (t *AccessCountTracer) StepTask(task Task) {
	// Do nothing
}

func (t *AccessCountTracer) EndTask(task Task) {
	// Do nothing
}

func (t *AccessCountTracer) Dump(name string) {
	f, err := os.Create(name + ".csv")
	if err != nil {
		panic(fmt.Sprintf("%e at AccessCountTracer, Dump", err))
	}
	defer f.Close()

	addrs := make([]uint64, 0, len(t.accessCountPerAddrs))
	for addr := range t.accessCountPerAddrs {
		addrs = append(addrs, addr)
	}
	sort.Slice(addrs, func(i, j int) bool {
		return addrs[i] < addrs[j]
	})

	fmt.Fprintf(f, "address, count, shared\n")
	for _, addr := range addrs {
		fmt.Fprintf(f, "%d, %d, %d\n",
			addr,
			t.accessCountPerAddrs[addr],
			len(t.sharedByChipPerAddr[addr]),
		)
	}
}
