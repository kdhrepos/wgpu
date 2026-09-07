package rdma

import (
	"encoding/binary"

	"github.com/spaolacci/murmur3"
	"gitlab.com/akita/protocol"
)

const (
	CBF_BITS         = 4
	MAX_COUNT        = 1<<(CBF_BITS) - 1
	GLOBAL_THRESHOLD = 1<<(CBF_BITS) - 1
	HOT_THRESHOLD    = 1<<(CBF_BITS-1) - 1
	WARM_THRESHOLD   = 1<<(CBF_BITS-2) - 1
	NUM_HASHES       = 4
)

const (
	DECAY_INTERVAL = 10000
)

type CBFTable struct {
	cbfSize  int
	counters []uint8
	states   []protocol.Hotness
}

func NewCBFTable(
	cbfSize int,
) *CBFTable {
	return &CBFTable{
		cbfSize:  cbfSize,
		counters: make([]uint8, cbfSize),
		states:   make([]protocol.Hotness, cbfSize),
	}
}

func (t *CBFTable) hash(addr uint64, seed int) uint32 {
	bs := make([]byte, 8)
	binary.LittleEndian.PutUint64(bs, addr)

	h32 := murmur3.New32WithSeed(uint32(seed))
	h32.Write(bs)

	return h32.Sum32() % uint32(t.cbfSize)
}

func (t *CBFTable) ReqFromOutside(
	addr uint64,
	hopCount int,
) {
	increment := uint8(hopCount / 3)

	for i := range NUM_HASHES {
		index := t.hash(addr, i)

		// 현재 값에 가중치를 더함
		newCount := uint16(t.counters[index]) + uint16(increment)

		// 포화 카운터(Saturating Counter) 로직
		if newCount > uint16(MAX_COUNT) {
			t.counters[index] = MAX_COUNT
		} else {
			t.counters[index] = uint8(newCount)
		}
	}
}

func (t *CBFTable) RspFromInside(
	addr uint64,
) protocol.Hotness {
	minState := protocol.Global

	for i := range NUM_HASHES {
		index := t.hash(addr, i)
		count := t.counters[index]

		state := protocol.Cold
		if count >= GLOBAL_THRESHOLD {
			state = protocol.Hot
		} else if count >= HOT_THRESHOLD {
			state = protocol.Hot
		} else if count >= WARM_THRESHOLD {
			state = protocol.Warm
		}

		if minState > state {
			minState = state
		}
	}

	return minState
}

func (t *CBFTable) ReqFromInside(
	addr uint64,
) protocol.Hotness {
	minState := protocol.Global

	for i := range NUM_HASHES {
		index := t.hash(addr, i)
		state := t.states[index]

		if minState > state {
			minState = state
		}
	}

	return minState
}

func (t *CBFTable) RspFromOutside(
	addr uint64,
	hotness protocol.Hotness,
) {
	for i := range NUM_HASHES {
		index := t.hash(addr, i)
		t.states[index] = hotness
	}
}

func (t *CBFTable) Decay() {
	for i := range t.cbfSize {
		t.counters[i] = t.counters[i] - (t.counters[i] >> 4) // 6.25% 감소
	}
}

func (t *CBFTable) Reset() {
	for i := range t.cbfSize {
		t.counters[i] = 0
		t.states[i] = protocol.Cold
	}
}
