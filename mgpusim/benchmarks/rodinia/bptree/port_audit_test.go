package bptree

import (
	"math"
	"math/rand/v2"
	"testing"
)

// buildFlat mirrors exactly what exec() copies to the device.
func buildFlat(knodes []Knode) (flatKeys, flatIndices []int32) {
	n := len(knodes)
	flatKeys = make([]int32, n*KnodeStride)
	flatIndices = make([]int32, n*KnodeStride)
	for i := range flatKeys {
		flatKeys[i] = math.MaxInt32
	}
	for k := 0; k < n; k++ {
		base := k * KnodeStride
		for i := 0; i < DefaultOrder+1; i++ {
			flatKeys[base+i] = knodes[k].Keys[i]
			flatIndices[base+i] = knodes[k].Indices[i]
		}
	}
	return
}

// simQuery replicates findK_kernel.cl for a single work-group (one query) and
// panics on any out-of-bounds flat/record access. Returns the answer and the
// knode the traversal landed on.
func simQuery(
	flatKeys, flatIndices, records []int32,
	height, knodesElem, key int32,
) (ans, landed int32) {
	flatLen, recLen := len(flatKeys), len(records)
	flat := func(idx int) int32 {
		if idx < 0 || idx >= flatLen {
			panic("FLAT OOB")
		}
		return flatKeys[idx]
	}
	idxAt := func(idx int) int32 {
		if idx < 0 || idx >= flatLen {
			panic("FLAT OOB")
		}
		return flatIndices[idx]
	}
	rec := func(idx int32) int32 {
		if idx < 0 || int(idx) >= recLen {
			panic("REC OOB")
		}
		return records[idx]
	}

	curr, off := int32(0), int32(0)
	for i := int32(0); i < height; i++ {
		cb := int(curr) << KnodeStrideLog2
		ob := int(off) << KnodeStrideLog2
		for thid := 0; thid < DefaultOrder; thid++ {
			if flat(cb+thid) <= key && flat(cb+thid+1) > key {
				if next := idxAt(ob + thid); next < knodesElem {
					off = next
				}
			}
		}
		curr = off
	}

	ans = -1
	lb := int(curr) << KnodeStrideLog2
	for thid := 0; thid < DefaultOrder; thid++ {
		if flat(lb+thid) == key {
			ans = rec(idxAt(lb + thid))
		}
	}
	return ans, curr
}

// TestPortAudit builds the synthetic B+ tree at the size that triggers the
// reported failure and proves, over a dense query sweep, that every traversal
// (1) lands on a leaf, (2) returns the correct record, (3) never reads any
// array out of bounds. Regression guard for the separator-key bug.
func TestPortAudit(t *testing.T) {
	const size = 30000000
	knodes, records, height := buildSyntheticTree(size, DefaultOrder)
	numKnodes := len(knodes)
	t.Logf("size=%d numKnodes=%d height=%d (%.1f MB/flat array)",
		size, numKnodes, height, float64(numKnodes*KnodeStride*4)/(1<<20))

	// Structural: stored indices are all valid.
	for k := 0; k < numKnodes; k++ {
		kn := &knodes[k]
		if kn.IsLeaf == 1 {
			for j := 1; j <= DefaultOrder; j++ {
				if kn.Keys[j] != math.MaxInt32 && (kn.Indices[j] < 0 || int(kn.Indices[j]) >= size) {
					t.Fatalf("leaf %d slot %d record idx %d out of [0,%d)", k, j, kn.Indices[j], size)
				}
			}
		} else {
			for j := 0; j <= DefaultOrder; j++ {
				if kn.Indices[j] < 0 || int(kn.Indices[j]) >= numKnodes {
					t.Fatalf("interior %d slot %d child idx %d out of [0,%d)", k, j, kn.Indices[j], numKnodes)
				}
			}
		}
	}

	flatKeys, flatIndices := buildFlat(knodes)

	wrong, interior, total := 0, 0, 0
	q := func(recIdx int) {
		key := int32(recIdx * 10)
		ans, landed := simQuery(flatKeys, flatIndices, records, height, int32(numKnodes), key)
		total++
		if ans != key {
			wrong++
		}
		if knodes[landed].IsLeaf != 1 {
			interior++
		}
	}

	q(0)
	q(size - 1)
	for r := 0; r < size; r += 137 { // stride sweep across every leaf
		q(r)
	}
	rng := rand.New(rand.NewPCG(1, 2))
	for i := 0; i < 300000; i++ {
		q(rng.IntN(size))
	}

	if wrong != 0 {
		t.Fatalf("%d/%d queries returned the wrong record (separator-key bug)", wrong, total)
	}
	if interior != 0 {
		t.Fatalf("%d/%d traversals ended on an interior node", interior, total)
	}
	t.Logf("OK: %d queries, all correct, all in-bounds, all landed on leaves", total)
}
