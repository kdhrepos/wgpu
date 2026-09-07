package bptree

import (
	"flag"
	"math/rand/v2"

	"gitlab.com/akita/mgpusim/footprint"
)

// knodeBytesReplay is the per-knode byte stride of each flat array.
const knodeBytesReplay = KnodeStride * 4

// replay reproduces the address stream of findK
// (native/kernel/findK_kernel.cl:38-76) on the host.
//
// One work-group serves one query (bid = query index) with DefaultOrder lanes
// cooperatively scanning a single knode's key row per level. currKnodeD and
// offsetD are written by the kernel itself, but they never escape the launch
// and the host seeds both to 0 (benchmark.go:280-282), so the descent is
// carried here instead. The tree and the query keys are host-generated, so the
// whole traversal is known without executing anything.
type replay struct {
	size  *int
	count *int
	zipfS *float64
}

func init() {
	footprint.Register("bptree", func() footprint.Replay { return &replay{} })
}

func (r *replay) Name() string { return "bptree" }

func (r *replay) Flags(fs *flag.FlagSet) {
	r.size = fs.Int("size", 3000000,
		"number of records (leaf keys) in the B+ tree")
	r.count = fs.Int("count", 5000, "number of queries to issue")
	r.zipfS = fs.Float64("bptree-zipf-s", 0,
		"query key skew: <=1 (default) = uniform; >1 = Zipfian s")
}

// queryKeys transcribes the key draw in initMem (benchmark.go:169-195).
func (r *replay) queryKeys(size, count int) []int32 {
	keys := make([]int32, count)
	rng := rand.New(rand.NewPCG(42, 0))

	if *r.zipfS > 1.0 {
		zipf := rand.NewZipf(rng, *r.zipfS, 1.0, uint64(size-1))
		for i := range keys {
			keys[i] = int32(zipf.Uint64()) * 10
		}

		return keys
	}

	for i := range keys {
		keys[i] = int32(rng.IntN(size)) * 10
	}

	return keys
}

// descendSlot returns the lane that passes the key-window test at
// findK_kernel.cl:56-57. Keys[0] is -INF and the unused tail is +INF
// (benchmark.go:430-435), so at most one lane matches.
func descendSlot(k *Knode, key int32) int32 {
	for t := int32(0); t < DefaultOrder; t++ {
		if k.Keys[t] <= key && k.Keys[t+1] > key {
			return t
		}
	}

	return -1
}

// leafSlot returns the lane that passes the equality probe at
// findK_kernel.cl:73.
func leafSlot(k *Knode, key int32) int32 {
	for t := int32(0); t < DefaultOrder; t++ {
		if k.Keys[t] == key {
			return t
		}
	}

	return -1
}

func (r *replay) Run(rec *footprint.Recorder, cfg footprint.Config) {
	size, count := *r.size, *r.count

	knodes, _, height := buildSyntheticTree(size, DefaultOrder)
	numKnodes := int32(len(knodes))
	keys := r.queryKeys(size, count)

	// Allocation order matches initMem (benchmark.go:226-233), page-0 guard
	// included.
	space := footprint.NewSpace()
	space.Alloc(1 << 12)
	flatBytes := uint64(len(knodes)) * knodeBytesReplay
	knodesKeysBase := space.Alloc(flatBytes)
	knodesIndicesBase := space.Alloc(flatBytes)
	recordsBase := space.Alloc(uint64(size) * 4)
	currKnodeBase := space.Alloc(uint64(count) * 4)
	offsetBase := space.Alloc(uint64(count) * 4)
	keysBase := space.Alloc(uint64(count) * 4)
	ansBase := space.Alloc(uint64(count) * 4)

	grid := footprint.Grid{
		GlobalSize: [3]uint32{uint32(count) * DefaultOrder, 1, 1},
		LocalSize:  [3]uint16{DefaultOrder, 1, 1},
	}
	partition := cfg.PartitionFor(0)
	numChiplets := cfg.NumChiplets()

	for q := 0; q < count; q++ {
		chiplet := footprint.ChipletOfWG(q, 0, grid, partition, numChiplets)
		key := keys[q]
		queryOff := uint64(q) * 4
		curr := int32(0)

		for lvl := int32(0); lvl < height; lvl++ {
			// currKnodeD[bid], offsetD[bid] and keysD[bid]: every lane reads
			// the same word, so each is one coalesced request (.cl:53-57).
			// offsetD equals currKnodeD on entry to every level, so off_base
			// and curr_base always name the same knode.
			rec.Touch(chiplet, currKnodeBase+queryOff)
			rec.Touch(chiplet, offsetBase+queryOff)
			rec.Touch(chiplet, keysBase+queryOff)

			// Lane thid reads keys[thid] and keys[thid+1], so the work-group
			// covers slots 0..DefaultOrder of one knode (.cl:56-57).
			base := uint64(curr) * knodeBytesReplay
			rec.TouchRange(chiplet, knodesKeysBase+base, (DefaultOrder+1)*4)

			if t := descendSlot(&knodes[curr], key); t >= 0 {
				rec.Touch(chiplet,
					knodesIndicesBase+base+uint64(t)*4)

				if next := knodes[curr].Indices[t]; next < numKnodes {
					rec.Touch(chiplet, offsetBase+queryOff)
					curr = next
				}
			}

			// thid == 0: currKnodeD[bid] = offsetD[bid] (.cl:65-67).
			rec.Touch(chiplet, offsetBase+queryOff)
			rec.Touch(chiplet, currKnodeBase+queryOff)
		}

		// Leaf-level equality probe (.cl:72-75).
		rec.Touch(chiplet, currKnodeBase+queryOff)
		leafBase := uint64(curr) * knodeBytesReplay
		rec.TouchRange(chiplet, knodesKeysBase+leafBase, DefaultOrder*4)
		rec.Touch(chiplet, keysBase+queryOff)

		if t := leafSlot(&knodes[curr], key); t >= 0 {
			rec.Touch(chiplet, knodesIndicesBase+leafBase+uint64(t)*4)
			rec.Touch(chiplet,
				recordsBase+uint64(knodes[curr].Indices[t])*4)
			rec.Touch(chiplet, ansBase+queryOff)
		}
	}
}
