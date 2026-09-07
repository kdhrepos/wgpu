package csr

import (
	"math"
	"math/rand"

	"gitlab.com/akita/util/global"
)

// seed drives the generator's private RNG. Go 1.20+ auto-seeds the global
// math/rand source per process, so relying on it made the generated matrix
// differ on every run — and therefore made runs non-comparable and offline
// address replay impossible. Fixed by default; override with SetSeed.
var seed int64 = 1

// SetSeed overrides the matrix-generation seed. Call before GenerateMatrix.
func SetSeed(s int64) {
	seed = s
}

// MatrixGenerator defines a matrix generator
type MatrixGenerator struct {
	numNode, numConnection   uint32
	xCoords, yCoords         []uint32
	values                   []float32
	positionOccupied         map[uint64]bool
	xCoordIndex, yCoordIndex map[uint32][]uint32
	rng                      *rand.Rand

	// zipfS > 0 skews column (x) selection to a Zipf power-law so a few
	// columns — and thus a few shared vector elements — become heavily-read
	// hubs. 0 = uniform (default, original behavior). Pulled from the global
	// config so all generator call sites pick it up without signature changes.
	zipfS    float64
	zipfCDF  []float64 // normalized cumulative rank weights (only if zipfS > 0)
	zipfPerm []int     // rank -> column, scatters hubs off the low indices
}

// MakeMatrixGenerator returns a matrixGenerator
func MakeMatrixGenerator(numNode, numConnection uint32) MatrixGenerator {
	return MatrixGenerator{
		numNode:       numNode,
		numConnection: numConnection,
		zipfS:         global.GetInstance().GetZipfS(),
	}
}

// GenerateMatrix generates matrix
func (g MatrixGenerator) GenerateMatrix() Matrix {
	g.init()
	g.generateConnections()
	g.normalize()
	m := g.outputCSRFormat()
	return m
}

func (g *MatrixGenerator) init() {
	g.rng = rand.New(rand.NewSource(seed))
	g.xCoords = make([]uint32, 0, g.numConnection)
	g.yCoords = make([]uint32, 0, g.numConnection)
	g.values = make([]float32, 0, g.numConnection)
	g.positionOccupied = make(map[uint64]bool)
	g.xCoordIndex = make(map[uint32][]uint32)
	g.yCoordIndex = make(map[uint32][]uint32)

	if g.zipfS > 0 {
		g.buildZipf()
	}
}

func (g *MatrixGenerator) generateConnections() {
	for i := uint32(0); i < g.numConnection; i++ {
		g.generateOneConnection()
	}
}

func (g *MatrixGenerator) normalize() {
	for i := uint32(0); i < g.numNode; i++ {
		sum := g.sumColumn(i)
		if sum == 0 {
			continue
		}

		indexes := g.xCoordIndex[i]
		for _, index := range indexes {
			g.values[index] /= sum
		}
	}
}

func (g MatrixGenerator) outputCSRFormat() Matrix {
	m := Matrix{}
	rowOffset := uint32(0)

	for i := uint32(0); i < g.numNode; i++ {
		cols, values := g.selectRowData(i)
		g.sortRowData(cols, values)

		m.RowOffsets = append(m.RowOffsets, rowOffset)
		m.ColumnNumbers = append(m.ColumnNumbers, cols...)
		m.Values = append(m.Values, values...)
		rowOffset += uint32(len(cols))
	}
	m.RowOffsets = append(m.RowOffsets, rowOffset)

	return m
}

func (g MatrixGenerator) selectRowData(
	row uint32,
) (
	cols []uint32,
	values []float32,
) {
	indexes := g.yCoordIndex[row]
	for _, index := range indexes {
		cols = append(cols, g.xCoords[index])
		values = append(values, g.values[index])
	}
	return
}

func (g MatrixGenerator) sortRowData(cols []uint32, values []float32) {
	for i := 0; i < len(cols); i++ {
		for j := i; j < len(cols); j++ {
			if cols[i] >= cols[j] {
				cols[i], cols[j] = cols[j], cols[i]
				values[i], values[j] = values[j], values[i]
			}
		}
	}
}

func (g MatrixGenerator) sumColumn(i uint32) float32 {
	sum := float32(0)
	indexes := g.xCoordIndex[i]
	for _, index := range indexes {
		sum += g.values[index]
	}
	return sum
}

func (g *MatrixGenerator) generateOneConnection() {
	x, y := g.generateUnoccupiedPosition()
	v := g.rng.Float32()
	g.xCoords = append(g.xCoords, x)
	g.yCoords = append(g.yCoords, y)
	g.values = append(g.values, v)

	if _, ok := g.xCoordIndex[x]; !ok {
		g.xCoordIndex[x] = make([]uint32, 0)
	}
	if _, ok := g.yCoordIndex[y]; !ok {
		g.yCoordIndex[y] = make([]uint32, 0)
	}
	g.xCoordIndex[x] = append(g.xCoordIndex[x], uint32(len(g.values)-1))
	g.yCoordIndex[y] = append(g.yCoordIndex[y], uint32(len(g.values)-1))
}

func (g MatrixGenerator) generateUnoccupiedPosition() (x, y uint32) {
	for {
		if g.zipfS > 0 {
			x = g.zipfColumn()
		} else {
			x = uint32(g.rng.Int()) % g.numNode
		}
		y = uint32(g.rng.Int()) % g.numNode
		if !g.isPositionOccupied(x, y) {
			g.markPositionOccupied(x, y)
			return
		}
	}
}

// buildZipf precomputes the normalized Zipf CDF over ranks [0,numNode) for the
// configured skew, plus a random rank->column permutation so the heavily-read
// hubs land on random columns rather than always the low indices. Only built
// when zipfS > 0; the uniform path never touches this.
func (g *MatrixGenerator) buildZipf() {
	n := g.numNode
	g.zipfCDF = make([]float64, n)
	sum := 0.0
	for i := uint32(0); i < n; i++ {
		sum += 1.0 / math.Pow(float64(i+1), g.zipfS)
		g.zipfCDF[i] = sum
	}
	inv := 1.0 / sum
	for i := uint32(0); i < n; i++ {
		g.zipfCDF[i] *= inv
	}
	g.zipfPerm = g.rng.Perm(int(n))
}

// zipfColumn samples a rank by inverse-CDF (binary search), then maps it
// through the permutation to a column id. Rank 0 is the hottest hub.
func (g MatrixGenerator) zipfColumn() uint32 {
	r := g.rng.Float64()
	lo, hi := uint32(0), g.numNode-1
	for lo < hi {
		mid := (lo + hi) / 2
		if g.zipfCDF[mid] < r {
			lo = mid + 1
		} else {
			hi = mid
		}
	}
	return uint32(g.zipfPerm[lo])
}

func (g MatrixGenerator) isPositionOccupied(x, y uint32) bool {
	_, ok := g.positionOccupied[uint64(y)*uint64(g.numNode)+uint64(x)]
	return ok
}

func (g MatrixGenerator) markPositionOccupied(x, y uint32) {
	g.positionOccupied[uint64(y)*uint64(g.numNode)+uint64(x)] = true
}
