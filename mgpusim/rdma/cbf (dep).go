package rdma

// deprecated cbf since it uses "state", not counter

// import (
// 	"encoding/binary"

// 	"github.com/spaolacci/murmur3"
// 	"gitlab.com/akita/protocol"
// )

// const (
// 	CBF_BITS         = 8
// 	MAX_COUNT        = 2 ^ (CBF_BITS) - 1
// 	GLOBAL_THRESHOLD = 2 ^ (CBF_BITS) - 1
// 	HOT_THRESHOLD    = 2 ^ (CBF_BITS - 1) - 1
// 	WARM_THRESHOLD   = 2 ^ (CBF_BITS - 2) - 1
// 	NUM_CLUSTERS     = 4 // num cluster can be used for iteration number of hash functions.
// )

// const (
// 	DECAY_INTERVAL = 10000
// )

// type CBFTable struct {
// 	cbfSize   int
// 	counters  []uint8
// 	states    []protocol.Hotness
// 	clusterId int
// }

// func NewCBFTable(
// 	cbfSize int,
// 	clusterId int,
// ) *CBFTable {
// 	return &CBFTable{
// 		cbfSize:   cbfSize,
// 		clusterId: clusterId,
// 		counters:  make([]uint8, cbfSize),
// 		states:    make([]protocol.Hotness, cbfSize),
// 	}
// }

// func (t *CBFTable) hashWithClsId(addr uint64, clusterId int) uint32 {
// 	bs := make([]byte, 8)
// 	binary.LittleEndian.PutUint64(bs, addr)

// 	// ClusterID를 Seed로 사용하여 서로 다른 해시 함수 효과를 냄
// 	h32 := murmur3.New32WithSeed(uint32(clusterId))
// 	h32.Write(bs)
// 	return h32.Sum32() % uint32(t.cbfSize)
// }

// func (t *CBFTable) ReqFromOutside(
// 	addr uint64,
// 	srcClusterId int,
// ) {
// 	index := t.hashWithClsId(addr, srcClusterId)

// 	if t.counters[index] < MAX_COUNT {
// 		t.counters[index]++
// 	}
// }

// func (t *CBFTable) RspFromInside(
// 	addr uint64,
// ) []protocol.Hotness {
// 	hotnessList := make([]protocol.Hotness, NUM_CLUSTERS)

// 	for clsId := range NUM_CLUSTERS {
// 		if clsId == t.clusterId {
// 			continue
// 		}

// 		index := t.hashWithClsId(addr, clsId)
// 		count := t.counters[index]

// 		if count >= GLOBAL_THRESHOLD {
// 			hotnessList[clsId] = protocol.Global
// 		} else if count >= HOT_THRESHOLD {
// 			hotnessList[clsId] = protocol.Hot
// 		} else if count >= WARM_THRESHOLD {
// 			hotnessList[clsId] = protocol.Warm
// 		} else {
// 			hotnessList[clsId] = protocol.Cold
// 		}
// 	}

// 	return hotnessList
// }

// func (t *CBFTable) ReqFromInside(
// 	addr uint64,
// 	homeClusterId int,
// ) protocol.Hotness {
// 	globalState := 0
// 	hotState := 0
// 	warmState := 0

// 	for clsId := range NUM_CLUSTERS {
// 		if clsId == homeClusterId {
// 			continue
// 		}

// 		index := t.hashWithClsId(addr, clsId)
// 		state := t.states[index]

// 		switch state {
// 		case protocol.Global:
// 			globalState++
// 		case protocol.Hot:
// 			hotState++
// 		case protocol.Warm:
// 			warmState++
// 		}
// 	}

// 	if globalState >= 2 {
// 		return protocol.Global
// 	} else if (hotState + globalState) >= 2 {
// 		return protocol.Hot
// 	} else if (hotState + globalState + warmState) >= 2 {
// 		return protocol.Warm
// 	}

// 	return protocol.Cold
// }

// func (t *CBFTable) RspFromOutside(
// 	addr uint64,
// 	hotness []protocol.Hotness,
// 	homeClusterId int,
// ) {
// 	for clsId := range NUM_CLUSTERS {
// 		if clsId == homeClusterId {
// 			continue
// 		}

// 		index := t.hashWithClsId(addr, clsId)
// 		t.states[index] = hotness[clsId]
// 	}
// }

// func (t *CBFTable) Decay() {
// 	for i := range t.cbfSize {
// 		t.counters[i] >>= 1
// 	}
// }

// func (t *CBFTable) Reset() {
// 	for i := range t.cbfSize {
// 		t.counters[i] = 0
// 		t.states[i] = protocol.Cold
// 	}
// }
