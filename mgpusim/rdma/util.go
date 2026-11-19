package rdma

func Abs(x int) int {
	if x < 0 {
		return -x
	}
	return x
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
