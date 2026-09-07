// EvictTracer reports re-access after cacheline eviction
package tracing

import (
	"strconv"

	"gitlab.com/akita/protocol"
)

type EvictTracer struct {
	localEvictCount     uint64
	localOverwriteCount uint64

	remoteEvictCount     uint64
	remoteOverwriteCount uint64

	localEvictPteCount      uint64
	localOverwritePteCount  uint64
	remoteEvictPteCount     uint64
	remoteOverwritePteCount uint64

	localEvictCountPerChiplet     []uint64
	localOverwriteCountPerChiplet []uint64
	remoteEvictCountPerChiplet    []uint64
	remoteOverwriteCountPerChiplet []uint64

	chipletIDCache map[string]int
}

func NewEvictTracer(numChiplets int) *EvictTracer {
	t := &EvictTracer{
		chipletIDCache:                 make(map[string]int),
		localEvictCountPerChiplet:      make([]uint64, numChiplets),
		localOverwriteCountPerChiplet:  make([]uint64, numChiplets),
		remoteEvictCountPerChiplet:     make([]uint64, numChiplets),
		remoteOverwriteCountPerChiplet: make([]uint64, numChiplets),
	}
	return t
}

func (t *EvictTracer) StartTask(task Task) {
	if task.Kind != "evict" &&
		task.Kind != "overwrite" {
		return
	}

	srcChip, ok := t.chipletIDCache[task.Where]
	if !ok {
		srcChip = parseChipletIDFromComponentName(task.Where)
		t.chipletIDCache[task.Where] = srcChip
	}

	owner, _ := strconv.Atoi(task.What)

	isPte := false
	if req, ok := task.Detail.(*protocol.ReadReq); ok && req.IsPtw {
		isPte = true
	}

	inRange := srcChip >= 0 && srcChip < len(t.localEvictCountPerChiplet)
	if srcChip == owner {
		switch task.Kind {
		case "evict":
			t.localEvictCount++
			if inRange {
				t.localEvictCountPerChiplet[srcChip]++
			}
			if isPte {
				t.localEvictPteCount++
			}
		case "overwrite":
			t.localOverwriteCount++
			if inRange {
				t.localOverwriteCountPerChiplet[srcChip]++
			}
			if isPte {
				t.localOverwritePteCount++
			}
		}
	} else {
		switch task.Kind {
		case "evict":
			t.remoteEvictCount++
			if inRange {
				t.remoteEvictCountPerChiplet[srcChip]++
			}
			if isPte {
				t.remoteEvictPteCount++
			}
		case "overwrite":
			t.remoteOverwriteCount++
			if inRange {
				t.remoteOverwriteCountPerChiplet[srcChip]++
			}
			if isPte {
				t.remoteOverwritePteCount++
			}
		}
	}
}

func (t *EvictTracer) StepTask(task Task) {
	// Do nothing
}

func (t *EvictTracer) EndTask(task Task) {
	// Do nothing
}

func (t *EvictTracer) GetLocalEvictCount() uint64 {
	return t.localEvictCount
}

func (t *EvictTracer) GetRemoteEvictCount() uint64 {
	return t.remoteEvictCount
}

func (t *EvictTracer) GetLocalOverwriteCount() uint64 {
	return t.localOverwriteCount
}

func (t *EvictTracer) GetRemoteOverwriteCount() uint64 {
	return t.remoteOverwriteCount
}

func (t *EvictTracer) GetLocalEvictPteCount() uint64 {
	return t.localEvictPteCount
}

func (t *EvictTracer) GetRemoteEvictPteCount() uint64 {
	return t.remoteEvictPteCount
}

func (t *EvictTracer) GetLocalOverwritePteCount() uint64 {
	return t.localOverwritePteCount
}

func (t *EvictTracer) GetRemoteOverwritePteCount() uint64 {
	return t.remoteOverwritePteCount
}

func (t *EvictTracer) GetLocalEvictCountPerChiplet() []uint64 {
	return t.localEvictCountPerChiplet
}

func (t *EvictTracer) GetLocalOverwriteCountPerChiplet() []uint64 {
	return t.localOverwriteCountPerChiplet
}

func (t *EvictTracer) GetRemoteEvictCountPerChiplet() []uint64 {
	return t.remoteEvictCountPerChiplet
}

func (t *EvictTracer) GetRemoteOverwriteCountPerChiplet() []uint64 {
	return t.remoteOverwriteCountPerChiplet
}
