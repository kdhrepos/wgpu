package rdma

import "math"

func Abs(x int) int {
	if x < 0 {
		return -x
	}
	return x
}

func simpleHash(paddr uint64) uint64 {
	paddr = paddr >> 6 // 64B cache line size
	hash := uint64(14695981039346656037)
	hash ^= paddr
	hash *= 1099511628211

	// 최종 결과에 믹싱 추가 (짝수/홀수가 골고루 섞임)
	hash ^= hash >> 33
	return hash
}

func GetClusterId(
	chipletId int,
	xDim int,
) int {
	clusterDim := xDim / 2

	row := chipletId / xDim
	col := chipletId % xDim

	partRow := row / clusterDim
	partCol := col / clusterDim

	return partRow*2 + partCol
}

// GetBranchNodeId picks the corner of the bounding box {dstChipId,
// dstChip2Id} closest to srcChipId and returns its chiplet ID.
//
// Coordinate convention (matches GetClusterId / GetDistanceById in this
// same file): X = id % xDim (column), Y = id / xDim (row).
// Chiplet ID = Y*xDim + X (row-major).
func GetBranchNodeId(
	xDim int,
	srcChipId int,
	dstChipId int,
	dstChip2Id int,
) int {
	srcChipX := srcChipId % xDim
	srcChipY := srcChipId / xDim
	dstChipX := dstChipId % xDim
	dstChipY := dstChipId / xDim
	dstChipX2 := dstChip2Id % xDim
	dstChipY2 := dstChip2Id / xDim

	corners := [4][2]int{
		{dstChipX, dstChipY},
		{dstChipX2, dstChipY},
		{dstChipX, dstChipY2},
		{dstChipX2, dstChipY2},
	}

	minDist := math.MaxInt
	branchX, branchY := 0, 0

	for _, corner := range corners {
		dist := Abs(srcChipX-corner[0]) + Abs(srcChipY-corner[1])
		if dist < minDist {
			minDist = dist
			branchX, branchY = corner[0], corner[1]
		}
	}

	return branchY*xDim + branchX
}

func GetDistanceById(
	srcId int,
	dstId int,
	xDim int,
) int {
	srcX := srcId % xDim
	srcY := srcId / xDim
	dstX := dstId % xDim
	dstY := dstId / xDim

	return Abs(srcX-dstX) + Abs(srcY-dstY)
}
