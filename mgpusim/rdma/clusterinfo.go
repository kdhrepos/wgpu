package rdma

import "fmt"

func GetClusterMap(clusterSize int) [][][]int {
	switch clusterSize {
	case 9:
		// 1st dim: requesting cluster id
		// 2nd dim: sender cluster id
		// 3rd dim: chiplet ID
		return [][][]int{
			{
				{0, 1, 6, 7},
				{1, 2, 7, 8},
				{6, 7, 12, 13},
				{7, 8, 13, 14},
			},
			{
				{3, 4, 9, 10},
				{4, 5, 10, 11},
				{9, 10, 15, 16},
				{10, 11, 16, 17},
			},
			{
				{18, 19, 24, 25},
				{19, 20, 25, 26},
				{24, 25, 30, 31},
				{25, 26, 31, 32},
			},
			{
				{21, 22, 27, 28},
				{22, 23, 28, 29},
				{27, 28, 33, 34},
				{28, 29, 34, 35},
			},
		}
	case 4:
		return [][][]int{
			{{0}, {1}, {6}, {7}},
			{{2}, {3}, {8}, {9}},
			{{4}, {5}, {10}, {11}},
			{{12}, {13}, {18}, {19}},
			{{14}, {15}, {20}, {21}},
			{{16}, {17}, {22}, {23}},
			{{24}, {25}, {30}, {31}},
			{{26}, {27}, {32}, {33}},
			{{28}, {29}, {34}, {35}},
		}
	// case 2:
	// 	return [][][]int{
	// 		{{0}, {1}},
	// 		{{2}, {3}},
	// 		{{4}, {5}},
	// 		{{6}, {7}},
	// 		{{8}, {9}},
	// 		{{10}, {11}},
	// 		{{12}, {13}},
	// 		{{14}, {15}},
	// 		{{16}, {17}},
	// 		{{18}, {19}},
	// 		{{20}, {21}},
	// 		{{22}, {23}},
	// 		{{24}, {25}},
	// 		{{26}, {27}},
	// 		{{28}, {29}},
	// 		{{30}, {31}},
	// 		{{32}, {33}},
	// 		{{34}, {35}},
	// 	}
	default:
		fmt.Println("Cluster Size: ", clusterSize)
		panic("unsupported cluster size")
	}
}

func GetWindowsInCluster(baseX, baseY int) [][]int {
	meshSize := 6
	var windows [][]int

	// 3x3 내부에서 2x2 윈도우가 시작될 수 있는 상대 위치는 (0,0), (1,0), (0,1), (1,1) 뿐임
	for yOffset := 0; yOffset <= 1; yOffset++ {
		for xOffset := 0; xOffset <= 1; xOffset++ {

			// 하나의 2x2 윈도우 생성
			var currentWindow []int
			for dy := 0; dy < 2; dy++ {
				for dx := 0; dx < 2; dx++ {
					// 글로벌 좌표 계산
					gx := baseX + xOffset + dx
					gy := baseY + yOffset + dy
					nodeID := gy*meshSize + gx
					currentWindow = append(currentWindow, nodeID)
				}
			}
			windows = append(windows, currentWindow)
		}
	}
	return windows
}

func IsOnMinimalPath(
	srcId int,
	dstId int,
	visitingNodeId int,
	xDim int,
) bool {
	srcToDst := GetDistanceById(srcId, dstId, xDim)
	srcToVisiting := GetDistanceById(srcId, visitingNodeId, xDim)
	visitingToDst := GetDistanceById(visitingNodeId, dstId, xDim)

	return srcToDst == srcToVisiting+visitingToDst
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}
