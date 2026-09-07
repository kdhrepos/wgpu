// NetworkFlightTracer measures one-way endpoint-to-endpoint flight time
// of RDMA packets — the time a msg spent inside the inter-chiplet network
// boundary, from source EndPoint.Send admission to destination EndPoint
// delivery into the receiving RDMA engine's port buffer. Pair with
// RDMAFlightTracer which adds the RDMA engine's own queuing/processing
// time on top of this network-only interval.
package tracing

import (
	"strconv"
	"strings"
)

const (
	NetworkFlightKindReq = "network-req-flight"
	NetworkFlightKindRsp = "network-rsp-flight"
)

type networkFlightDir int

const (
	networkFlightDirReq networkFlightDir = iota
	networkFlightDirRsp
)

// networkFlightInflightInfo holds only what we need between StartTask and
// EndTask. After EndTask aggregates into the streaming counters this entry
// is deleted, so the map size is bounded by concurrent in-flight count,
// not by total completed events.
type networkFlightInflightInfo struct {
	startTime   float64
	direction   networkFlightDir
	hopRequired uint64
	srcChipId   int
}

type NetworkFlightTracer struct {
	filter   TaskFilter
	inflight map[uint64]*networkFlightInflightInfo
	xDim     int
	yDim     int

	// Streaming aggregates updated on EndTask. Reading these is O(numChiplets^2)
	// or O(numHopBuckets); writing is O(1). No per-completed-task memory.
	countHopReq []uint64
	sumHopReq   []float64
	countHopRsp []uint64
	sumHopRsp   []float64
	c2cReq      [][]int
	c2cRsp      [][]int
	totalReq    int
	totalRsp    int
}

func NewNetworkFlightTracer(
	filter TaskFilter,
	xDim int,
	yDim int,
) *NetworkFlightTracer {
	t := &NetworkFlightTracer{
		filter:   filter,
		inflight: make(map[uint64]*networkFlightInflightInfo),
		xDim:     xDim,
		yDim:     yDim,
	}
	buckets := xDim + yDim - 1
	t.countHopReq = make([]uint64, buckets)
	t.sumHopReq = make([]float64, buckets)
	t.countHopRsp = make([]uint64, buckets)
	t.sumHopRsp = make([]float64, buckets)
	num := xDim * yDim
	t.c2cReq = make([][]int, num)
	t.c2cRsp = make([][]int, num)
	for i := 0; i < num; i++ {
		t.c2cReq[i] = make([]int, num)
		t.c2cRsp[i] = make([]int, num)
	}
	return t
}

func (t *NetworkFlightTracer) StartTask(task Task) {
	if !t.filter(task) {
		return
	}

	if task.What == "retry" {
		if info, ok := t.inflight[task.ID]; ok {
			info.startTime = float64(task.StartTime)
		}
		return
	}

	// EP.Send dedups by msg.ID already; this guard ensures that if a
	// duplicate StartTask slips through (e.g., from a code path we have
	// not anticipated), the earliest first-observation time is kept
	// rather than silently overwritten.
	if _, exists := t.inflight[task.ID]; exists {
		return
	}

	srcChipId := chipletIdFromWhere(task.Where)

	t.inflight[task.ID] = &networkFlightInflightInfo{
		startTime:   float64(task.StartTime),
		direction:   networkDirOfKind(task.Kind),
		hopRequired: t.hopRequiredFromDetail(task),
		srcChipId:   srcChipId,
	}
}

func (t *NetworkFlightTracer) StepTask(_ Task) {
	// Flight time is StartTime->EndTime; per-hop counting is not needed.
}

func (t *NetworkFlightTracer) EndTask(task Task) {
	info, ok := t.inflight[task.ID]
	if !ok {
		return
	}
	delete(t.inflight, task.ID)

	latency := float64(task.EndTime) - info.startTime
	dstChipId := chipletIdFromWhere(task.Where)
	hop := int(info.hopRequired)
	buckets := t.numHopBuckets()

	if info.direction == networkFlightDirReq {
		t.totalReq++
		if hop >= 0 && hop < buckets {
			t.countHopReq[hop]++
			t.sumHopReq[hop] += latency
		}
		if info.srcChipId >= 0 && info.srcChipId < t.xDim*t.yDim &&
			dstChipId >= 0 && dstChipId < t.xDim*t.yDim {
			t.c2cReq[info.srcChipId][dstChipId]++
		}
	} else {
		t.totalRsp++
		if hop >= 0 && hop < buckets {
			t.countHopRsp[hop]++
			t.sumHopRsp[hop] += latency
		}
		if info.srcChipId >= 0 && info.srcChipId < t.xDim*t.yDim &&
			dstChipId >= 0 && dstChipId < t.xDim*t.yDim {
			t.c2cRsp[info.srcChipId][dstChipId]++
		}
	}
}

func networkDirOfKind(kind string) networkFlightDir {
	if kind == NetworkFlightKindRsp {
		return networkFlightDirRsp
	}
	return networkFlightDirReq
}

func (t *NetworkFlightTracer) hopRequiredFromDetail(task Task) uint64 {
	detail, ok := task.Detail.(string)
	if !ok {
		return 0
	}
	parts := strings.Split(detail, ".")
	if len(parts) < 2 {
		return 0
	}
	srcChip, _ := strconv.Atoi(strings.TrimPrefix(parts[0], "chiplet_"))
	dstChip, _ := strconv.Atoi(strings.TrimPrefix(parts[1], "chiplet_"))

	srcX := srcChip % t.xDim
	srcY := srcChip / t.xDim
	dstX := dstChip % t.xDim
	dstY := dstChip / t.xDim
	return uint64(Abs(srcY-dstY) + Abs(srcX-dstX))
}

// GetAvgLatencyPerHop returns mean flight latency for each hop bucket
// (index = Manhattan hop count). Buckets with no samples are zero.
func (t *NetworkFlightTracer) GetAvgLatencyPerHop(
	kind string,
) []float64 {
	buckets := t.numHopBuckets()
	out := make([]float64, buckets)
	var counts []uint64
	var sums []float64
	if networkDirOfKind(kind) == networkFlightDirReq {
		counts, sums = t.countHopReq, t.sumHopReq
	} else {
		counts, sums = t.countHopRsp, t.sumHopRsp
	}
	for h := 0; h < buckets; h++ {
		if counts[h] > 0 {
			out[h] = sums[h] / float64(counts[h])
		}
	}
	return out
}

// GetCountPerHop returns the number of completed flights per hop bucket.
func (t *NetworkFlightTracer) GetCountPerHop(
	kind string,
) []uint64 {
	buckets := t.numHopBuckets()
	out := make([]uint64, buckets)
	var counts []uint64
	if networkDirOfKind(kind) == networkFlightDirReq {
		counts = t.countHopReq
	} else {
		counts = t.countHopRsp
	}
	copy(out, counts)
	return out
}

// GetTotalCompleted returns the number of completed flights for a given
// direction. Pass NetworkFlightKindReq or NetworkFlightKindRsp.
func (t *NetworkFlightTracer) GetTotalCompleted(kind string) int {
	if networkDirOfKind(kind) == networkFlightDirReq {
		return t.totalReq
	}
	return t.totalRsp
}

// GetC2C returns a [src][dst] matrix of completed flight counts for a
// given direction.
func (t *NetworkFlightTracer) GetC2C(kind string) [][]int {
	var src [][]int
	if networkDirOfKind(kind) == networkFlightDirReq {
		src = t.c2cReq
	} else {
		src = t.c2cRsp
	}
	out := make([][]int, len(src))
	for i := range src {
		out[i] = make([]int, len(src[i]))
		copy(out[i], src[i])
	}
	return out
}

func (t *NetworkFlightTracer) numHopBuckets() int {
	return t.xDim + t.yDim - 1
}
