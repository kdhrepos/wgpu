package global

import (
	"fmt"
	"os"
	"sync"
)

type Range struct {
	Start uint64
	End   uint64
}

type Global struct {
	verbose bool

	benchmarkName string

	wgPartitioning   []string
	dataPartitioning []string
	dataStrides      []int

	LogDataAllocation         bool
	LogDataAllocationFilename string
	// data id -> chiplet id -> physical address range
	dataAllocatedLocation map[int]map[int][]Range
	// page base physical address -> data id (reverse index of dataAllocatedLocation)
	pageToDataID map[uint64]int

	timeLimit      int // in hours
	colSizeForLasp int

	// Experimental hardware knobs for the "expose hop-count gap" study.
	// Each is an override: 0 means "use the built-in default", so unset
	// flags leave behaviour unchanged. Set from runner CLI flags; read in
	// builders/common.go (TLB, L2) and timing/cu (vmem egress).
	//   l1TLBTputMult        — multiplies L1 V TLB MSHR(8) and req/cycle(2)
	//   l2TLBTputMult        — multiplies L2 TLB MSHR(64) and req/cycle(4)
	//   l2MSHREntry          — overrides L2 cache MSHR entries per bank (def 32)
	//   vmemEgressWidth      — CU vector-mem egress lanes/cycle (def 16)
	//   robNumReqPerCycle    — L1V ROB topDown/parseBottom/bottomUp per cycle (def 4)
	//   l1vCacheNumReqs      — L1V cache req/cycle accepted from ROB (def 2)
	//   atNumReqPerCycle     — address translator req/cycle (def 4)
	//   robBufferSize        — L1V ROB MLP window (in-flight transaction list cap, def 128)
	l1TLBTputMult     int
	l2TLBTputMult     int
	l2MSHREntry       int
	zipfS             float64
	vmemEgressWidth   int
	robNumReqPerCycle int
	l1vCacheNumReqs   int
	atNumReqPerCycle  int
	robBufferSize     int

	// prioritizePTEAtL2 — when true, L2 LookupBuffer puts PTW transactions
	// ahead of data transactions within each destination tier. Default true
	// preserves current behaviour; set false to ablate the PTE-priority effect.
	prioritizePTEAtL2 bool

	// robPerWfSubFifo — when true, L1V/L1S/L1I ROBs split transactions into
	// one in-order sub-FIFO per wavefront. Eliminates cross-WF head-of-line
	// blocking while keeping per-WF program order (so InFlight[0]-style
	// scalar/inst handlers stay correct). Default true (no HoL); set false to
	// ablate.
	robPerWfSubFifo bool

	// oooDebug enables verbose [ooo-dbg]/[ooo-bug] prints in the CU vmem
	// path used while diagnosing the OOO ROB corruption (see commit history).
	// Effective only when verbose is also on — see GetOOODebug.
	oooDebug bool
}

var (
	instance *Global
	once     sync.Once
)

func GetInstance() *Global {
	once.Do(func() {
		instance = &Global{
			verbose:               false,
			wgPartitioning:        []string{},
			dataPartitioning:      []string{},
			dataStrides:           []int{},
			dataAllocatedLocation: make(map[int]map[int][]Range),
			pageToDataID:          make(map[uint64]int),
			prioritizePTEAtL2:     true,
			robPerWfSubFifo:       true,
		}
	})
	return instance
}

func (g *Global) SetTimeLimit(hours int) {
	// time limit in hours
	g.timeLimit = hours
}

func (g *Global) GetTimeLimit() int {
	return g.timeLimit
}

// SetL1TLBTputMult / GetL1TLBTputMult — L1 V TLB throughput multiplier.
func (g *Global) SetL1TLBTputMult(m int) { g.l1TLBTputMult = m }

// GetL1TLBTputMult returns the multiplier, clamped to >= 1.
func (g *Global) GetL1TLBTputMult() int {
	if g.l1TLBTputMult < 1 {
		return 1
	}
	return g.l1TLBTputMult
}

// SetL2TLBTputMult / GetL2TLBTputMult — L2 TLB throughput multiplier.
func (g *Global) SetL2TLBTputMult(m int) { g.l2TLBTputMult = m }

// GetL2TLBTputMult returns the multiplier, clamped to >= 1.
func (g *Global) GetL2TLBTputMult() int {
	if g.l2TLBTputMult < 1 {
		return 1
	}
	return g.l2TLBTputMult
}

// SetL2MSHREntry / GetL2MSHREntry — L2 cache MSHR entries per bank.
func (g *Global) SetL2MSHREntry(n int) { g.l2MSHREntry = n }

// GetL2MSHREntry returns the override, or 0 if unset (caller uses default).
func (g *Global) GetL2MSHREntry() int { return g.l2MSHREntry }

// SetZipfS / GetZipfS — Zipf skew exponent for the CSR matrix/graph generator
// (spmv / pagerank / pannotia). 0 = uniform (default, original behavior); >0
// concentrates column (shared vector-element) fan-in into power-law hubs.
func (g *Global) SetZipfS(s float64) { g.zipfS = s }

// GetZipfS returns the skew exponent, or 0 (uniform) if unset.
func (g *Global) GetZipfS() float64 { return g.zipfS }

// SetVmemEgressWidth / GetVmemEgressWidth — CU vmem egress lanes/cycle.
func (g *Global) SetVmemEgressWidth(w int) { g.vmemEgressWidth = w }

// GetVmemEgressWidth returns the width, clamped to >= 1.
func (g *Global) GetVmemEgressWidth() int {
	if g.vmemEgressWidth < 1 {
		return 1
	}
	return g.vmemEgressWidth
}

// SetROBNumReqPerCycle / GetROBNumReqPerCycle — L1V ROB pipeline width/cycle.
func (g *Global) SetROBNumReqPerCycle(n int) { g.robNumReqPerCycle = n }

func (g *Global) GetROBNumReqPerCycle() int {
	if g.robNumReqPerCycle < 1 {
		return 4
	}
	return g.robNumReqPerCycle
}

// SetOOODebug / GetOOODebug — toggle [ooo-dbg]/[ooo-bug] verbose prints in
// the CU vmem path. Effective only when both -verbose AND -ooo-debug are on,
// so omitting -verbose suppresses all OOO debug output regardless of the
// -ooo-debug flag.
func (g *Global) SetOOODebug(on bool) { g.oooDebug = on }
func (g *Global) GetOOODebug() bool   { return g.verbose && g.oooDebug }

// SetL1VCacheNumReqs / GetL1VCacheNumReqs — L1V cache req/cycle.
func (g *Global) SetL1VCacheNumReqs(n int) { g.l1vCacheNumReqs = n }

func (g *Global) GetL1VCacheNumReqs() int {
	if g.l1vCacheNumReqs < 1 {
		return 2
	}
	return g.l1vCacheNumReqs
}

// SetATNumReqPerCycle / GetATNumReqPerCycle — address translator req/cycle.
func (g *Global) SetATNumReqPerCycle(n int) { g.atNumReqPerCycle = n }

func (g *Global) GetATNumReqPerCycle() int {
	if g.atNumReqPerCycle < 1 {
		return 4
	}
	return g.atNumReqPerCycle
}

// SetROBBufferSize / GetROBBufferSize — L1V ROB MLP window (in-flight cap).
func (g *Global) SetROBBufferSize(n int) { g.robBufferSize = n }

func (g *Global) GetROBBufferSize() int {
	if g.robBufferSize < 1 {
		return 128
	}
	return g.robBufferSize
}

// SetPrioritizePTEAtL2 / GetPrioritizePTEAtL2 — toggle PTW-first priority in
// L2 LookupBuffer. true (default) keeps current behaviour.
func (g *Global) SetPrioritizePTEAtL2(v bool) { g.prioritizePTEAtL2 = v }
func (g *Global) GetPrioritizePTEAtL2() bool  { return g.prioritizePTEAtL2 }

// SetROBPerWfSubFifo / GetROBPerWfSubFifo — toggle per-wavefront sub-FIFO
// mode in L1V/L1S/L1I ROBs. true (default) removes cross-WF HoL.
func (g *Global) SetROBPerWfSubFifo(v bool) { g.robPerWfSubFifo = v }
func (g *Global) GetROBPerWfSubFifo() bool  { return g.robPerWfSubFifo }

func (g *Global) SetVerbose(v bool) {
	g.verbose = v
}

func (g *Global) GetVerbose() bool {
	return g.verbose
}

func (g *Global) AddWgPartition(wgPartitioning string) {
	g.wgPartitioning = append(g.wgPartitioning, wgPartitioning)
}

func (g *Global) GetWgPartition(kernelId int) string {
	if kernelId >= len(g.wgPartitioning) {
		return "Xdiv"
	}
	return g.wgPartitioning[kernelId]
}

func (g *Global) AddDataPartitioning(dataPartitioning string) {
	g.dataPartitioning = append(g.dataPartitioning, dataPartitioning)
}

func (g *Global) GetDataPartitioning(dataId int) string {
	if dataId >= len(g.dataPartitioning) {
		return "kernel-wide"
	}
	return g.dataPartitioning[dataId]
}

func (g *Global) GetDataStride(dataId int) int {
	if len(g.dataStrides) <= dataId {
		return 0
	}
	return g.dataStrides[dataId]
}

func (g *Global) SetDataStrides(strides []int) {
	g.dataStrides = strides
}

func (g *Global) SetBenchmarkName(name string) {
	g.benchmarkName = name
}

func (g *Global) GetBenchmarkName() string {
	return g.benchmarkName
}

func (g *Global) SetColSizeForLasp(colSize int) {
	g.colSizeForLasp = colSize
}

func (g *Global) GetColSizeForLasp() int {
	return g.colSizeForLasp
}

func (g *Global) SetDataAllocatedLocation(
	dataId int,
	chipletId int,
	pAddr uint64,
) {
	if g.dataAllocatedLocation == nil {
		g.dataAllocatedLocation = make(map[int]map[int][]Range)
	}
	if g.dataAllocatedLocation[dataId] == nil {
		g.dataAllocatedLocation[dataId] = make(map[int][]Range)
	}

	ranges := g.dataAllocatedLocation[dataId][chipletId]

	Range := Range{
		Start: pAddr,
		End:   pAddr + 4096,
	}

	ranges = append(ranges, Range)
	g.dataAllocatedLocation[dataId][chipletId] = ranges

	if g.pageToDataID == nil {
		g.pageToDataID = make(map[uint64]int)
	}
	g.pageToDataID[pAddr & ^uint64(0xFFF)] = dataId
}

func (g *Global) LookupDataID(pAddr uint64) int {
	if g.pageToDataID == nil {
		return -1
	}
	if id, ok := g.pageToDataID[pAddr & ^uint64(0xFFF)]; ok {
		return id
	}
	return -1
}

func (g *Global) PrintDataLocation() {
	if !g.LogDataAllocation {
		return
	}

	// g.dataAllocatedLocation 구조: map[int]map[int][]Range
	dataAllocatedLocation := g.dataAllocatedLocation

	// 파일명 설정
	fileName := fmt.Sprintf("%s.csv", g.LogDataAllocationFilename)

	// 파일 열기 (O_APPEND 대신 O_TRUNC를 사용하여 실행 시마다 새 파일로 갱신하거나,
	// 기존 데이터에 이어붙이려면 O_APPEND를 유지하세요)
	f, err := os.Create(fileName)
	if err != nil {
		panic(err)
	}
	defer f.Close()

	// 파일이 비어있다면 CSV 헤더 작성
	fi, _ := f.Stat()
	if fi.Size() == 0 {
		fmt.Fprintln(f, "DataID,ChipletID,StartAddr,EndAddr")
	}

	// 1. 모든 Data ID에 대해 반복
	for dId, chipletMap := range dataAllocatedLocation {

		// 2. 해당 데이터를 보유한 모든 Chiplet ID에 대해 반복
		for chipletId, ranges := range chipletMap {

			// 3. 해당 칩렛 내의 모든 주소 범위(Range)에 대해 반복
			for _, r := range ranges {
				// CSV 포맷으로 파일에 기록
				// %s(벤치마크), %d(ID), 0x%x(16진수 주소)
				_, err := fmt.Fprintf(f, "%d,%d,%d,%d\n",
					dId, chipletId, r.Start, r.End)

				if err != nil {
					fmt.Printf("Error writing to file: %v\n", err)
				}
			}
		}
	}

	fmt.Printf("Successfully saved all data allocation logs to %s\n", fileName)

	// 모든 출력이 완료된 후 시스템 종료
	os.Exit(0)
}
