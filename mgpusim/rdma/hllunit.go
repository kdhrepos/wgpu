package rdma

import (
	"math"
	"math/bits"
	"strconv"

	"github.com/spaolacci/murmur3" // murmur3 32비트 해시 라이브러리
)

type HLLUnit struct {
	b         uint32  // 인덱스 비트 수
	m         uint32  // 버킷 개수 (1 << b)
	registers []uint8 // 각 버킷의 rho 값 저장
}

// NewHLLUnit은 지정된 b값을 가진 HLL 유닛을 생성합니다.
func NewHLLUnit(b uint32) *HLLUnit {
	m := uint32(1 << b)
	return &HLLUnit{
		b:         b,
		m:         m,
		registers: make([]uint8, m),
	}
}

// Add는 새로운 주소를 입력받아 레지스터를 업데이트합니다.
func (h *HLLUnit) Add(addr uint64) {
	// 1. 32비트 MurmurHash3 생성 (Python의 mmh3.hash와 호환)
	hasher := murmur3.New32()
	hasher.Write([]byte(strconv.FormatUint(addr, 10)))
	x := hasher.Sum32()

	// 2. 버킷 인덱스 (하위 b 비트)
	idx := x & (h.m - 1)

	// 3. rho 계산 (나머지 비트에서 첫 번째 1의 위치)
	w := x >> h.b
	var rho uint8

	if w > 0 {
		// math/bits.LeadingZeros32는 전체 32비트 기준이므로,
		// 우리가 사용한 하위 b비트를 제외한 나머지 비트 폭 내에서 계산해야 함.
		leadingZeros := uint32(bits.LeadingZeros32(w))

		// 실제 상위 비트들 중에서의 Leading Zero 위치 계산
		// 예: 32비트 중 하위 6비트가 b라면, w는 26비트 데이터임.
		// bits.LeadingZeros32(w)는 앞의 6비트(0으로 채워진)를 포함해서 셈.
		rho = uint8(leadingZeros-h.b) + 1
	} else {
		rho = uint8(32 - h.b + 1)
	}

	// 4. Max Update
	if rho > h.registers[idx] {
		h.registers[idx] = rho
	}
}

// Estimate는 고유 주소 수(Cardinality)를 추정합니다.
func (h *HLLUnit) Estimate() uint64 {
	m := float64(h.m)
	v := 0 // 빈 버킷 수
	var termSum float64 = 0

	for _, val := range h.registers {
		if val == 0 {
			v++
		}
		termSum += 1.0 / math.Pow(2, float64(val))
	}

	if v == int(h.m) {
		return 0
	}

	// 표준 알파 계수
	alpha := 0.7213 / (1 + 1.079/m)
	est := alpha * m * m / termSum

	// Small/Large Range 보정 로직
	if est <= 2.5*m && v > 0 {
		// Small Range: Linear Counting
		est = m * math.Log(m/float64(v))
	} else if est > (1.0/30.0)*math.Pow(2, 32) {
		// Large Range
		est = -math.Pow(2, 32) * math.Log(1.0-est/math.Pow(2, 32))
	}

	res := uint64(math.Round(est))
	if res < 1 {
		return 1
	}
	return res
}

// DecayRanks decrements every register by 1 (floor 0). Since the HLL
// estimate is ∝ m²/Σ2^(−M[j]), a uniform −1 across registers roughly
// halves the cardinality estimate — an exponential ("EMA"-style) forget
// of the sketch with zero extra per-register state. Live addresses restore
// their register on the next Add(); registers whose max-rank address is not
// re-added keep decaying toward 0, so the estimate tracks a temporal
// working set instead of the monotonic lifetime cardinality.
func (h *HLLUnit) DecayRanks() {
	for i := range h.registers {
		if h.registers[i] > 0 {
			h.registers[i]--
		}
	}
}
