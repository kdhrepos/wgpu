package rdma

import (
	"math"
)

type CountMinSketch struct {
	depth int       // 해시 함수 개수 (d)
	width int       // 각 행의 버킷 개수 (w)
	table [][]uint8 // 카운트 테이블
}

func NewCountMinSketch(depth, width int) *CountMinSketch {
	table := make([][]uint8, depth)
	for i := range table {
		table[i] = make([]uint8, width)
	}
	return &CountMinSketch{depth, width, table}
}

func (cms *CountMinSketch) hash(item uint64, i int) int {
	const (
		offset64 = 14695981039346656037
		prime64  = 1099511628211
	)

	h := uint64(offset64)
	// i (seed) 반영
	h ^= uint64(i)
	h *= prime64
	// item 반영
	h ^= item
	h *= prime64

	return int(h % uint64(cms.width))
}

func (cms *CountMinSketch) Update(item uint64) {
	for i := 0; i < cms.depth; i++ {
		idx := cms.hash(item, i)

		cms.table[i][idx]++

		// to prevent uint8 overflow
		if cms.table[i][idx] >= math.MaxUint8 {
			cms.table[i][idx] = math.MaxUint8
		}
	}
}

func (cms *CountMinSketch) Query(item uint64) uint32 {
	var minVal uint8 = math.MaxUint8
	for i := 0; i < cms.depth; i++ {
		idx := cms.hash(item, i)
		if cms.table[i][idx] < minVal {
			minVal = cms.table[i][idx]
		}
	}
	return uint32(minVal)
}

func (cms *CountMinSketch) Decay() {
	for i := 0; i < cms.depth; i++ {
		for j := 0; j < cms.width; j++ {
			cms.table[i][j] -= 2 // FIXME: need to be considered

			// to prevent underflow
			if cms.table[i][j] <= 0 {
				cms.table[i][j] = 0
			}
		}
	}
}

func (cms *CountMinSketch) Reset() {
	for i := 0; i < cms.depth; i++ {
		for j := 0; j < cms.width; j++ {
			cms.table[i][j] = 0
		}
	}
}
