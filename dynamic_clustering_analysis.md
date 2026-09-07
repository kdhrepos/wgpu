# Dynamic Clustering 플랫폼 — 동작 분석 (논문 Outline 초안용)

> 목적: Wafer-scale GPU의 어떤 문제를, 어떤 방식으로 푸는지를 논문 구조로 넘기기 위한 정리.
> 근거: `mgpusim/platform/dynamic_clustering.go`, `mgpusim/rdma/dynamic_clustering_rdmaengine.go`,
> `mgpusim/rdma/pctable.go`, `mgpusim/rdma/clusterinfo.go`, `mem/cache/writeback-dynamic-dist`.

---

## 1. 배경: Wafer-scale GPU 베이스라인과 그 한계

### 1.1 시스템 모델
- Wafer-scale GPU = `xDim × yDim` 크기의 **chiplet mesh**. (예: 6×6 = 36 chiplet, 단일 GPU로 노출)
- 각 chiplet은 자기 몫을 **분산 소유**한다:
  - 자기 CU 클러스터 (연산),
  - 자기 **L2 슬라이스** (실효 약 4 MB/chiplet),
  - 자기 **local DRAM 파티션** (4 GB/chiplet 고정).
- 물리주소는 chiplet들에 interleave/파티션되어, 모든 주소는 **global home**(그 주소의 DRAM+L2를 소유한 chiplet) 하나를 가진다. RDMA 엔진의 `RdmaReqAddressTable.Find(addr)`가 이 global home을 돌려준다.
- chiplet 간 통신은 **mesh NoC** 위의 hop으로 이뤄지고, RDMA 엔진이 원격 요청/응답을 라우팅한다.

### 1.2 핵심 문제 — inter-chiplet 트래픽이 지배적 비용
어떤 chiplet이 자기 것이 아닌 데이터를 필요로 하면, 요청은 mesh를 가로질러 global home까지 갔다 와야 한다. mesh가 커질수록:
- **latency**: global home이 멀리 있다 (6×6 mesh 지름 = 10 hop). 원격 접근 왕복이 길다.
- **NoC bandwidth**: 원격 트래픽이 mesh를 포화시킨다.
- **scaling**: mesh가 커질수록 평균 hop 거리가 늘어 문제가 악화된다.

즉 wafer-scale GPU의 성능은 **"공유 데이터를 위해 얼마나 멀리 가야 하는가"** 에 좌우된다.

### 1.3 기존 두 접근과 각자의 실패 모드
이 연구가 직접 비교/극복 대상으로 삼는 두 baseline:

| 접근 | 방식 | 장점 | 실패 모드 |
|---|---|---|---|
| **Single global home** (`mesh`) | 복제 없음. 모든 원격 접근이 global home까지 감 | 용량 낭비 0 | 공유 데이터마다 **매번 full 거리** 지불. 평균 hop 김 |
| **Per-chiplet 복제** (`nearfetch` 계열) | 각 chiplet이 fetch한 것을 자기 L2에 캐시 | working set이 4 MB에 들어가면 매우 빠름 | 공유 footprint > 4 MB면 **thrash**. N개 chiplet이 같은 라인을 N벌 복제 → 용량 낭비 + 재fetch 폭주 |

**두 접근 모두 "어떤 데이터를 / 얼마나 넓게" 복제할지 적응하지 못한다.** Single-home은 아무것도 복제 안 하고, per-chiplet은 무차별 복제한다. Dynamic Clustering은 정확히 이 두 축(선택성 + 넓이)을 도입한다.

---

## 2. 핵심 아이디어 (한 문장)

> **mesh chiplet들을 cluster로 묶어 cluster의 L2 용량을 하나의 풀로 쓰고, "핫하고 공유되며 footprint가 풀에 들어가는" 데이터만 cluster마다 딱 한 벌씩, requestor→global home 라우팅 경로 위의 local home에 분산 배치한다.**

두 가지가 이 방식을 "dynamic"하게 만든다:

1. **무엇을 분산할지 (What)** = **PC 기반 hotness 분류기**. instruction PC가 hot+shared로 분류되고, 그 PC의 실제 working set이 cluster 풀에 들어갈 때만 분산한다. cold/streaming/non-shared는 global home에 그대로 둔다(복제 오버헤드 0).
2. **얼마나 넓게 분산할지 (How wide)** = **hotness tier별로 cluster 크기를 다르게** 잡는다. 더 뜨겁고 footprint 작은 데이터 → 작고 촘촘한 cluster (복제본 많지만 각각 가까움). "Global" 데이터 → 큰 cluster (복제본 적지만 각 복제본 뒤에 더 큰 풀 용량).

---

## 3. 동작 메커니즘

전체 흐름: **원격 L2 miss → PC hotness 조회 → (분산 대상이면) local home들을 경로 위로 lookup → 최초 hit에서 조기 반환, 아니면 global home 도달 → global home이 응답하며 cluster의 local home에 복제본 설치(distribution).**

### 3.1 What — PC 기반 hotness 분류 (`pctable.go`)
- RDMA 요청마다 **발원 instruction의 PC**가 실려 온다 (read req의 flit 여유 공간에 ~16bit 해시 PC piggyback → 실질 무충돌, HW 오버헤드 극소).
- `PCTable`이 PC별·원격 owner별로 세 신호를 누적:
  - **reuse** = `ReqCount / unique` (cacheline당 평균 접근 수 = 재사용 강도),
  - **sharing** = `tuples / unique` (라인당 접근한 서로 다른 chiplet 수 = 공유 폭),
  - **hop** = 정규화된 평균 거리.
  - `unique`(distinct 라인 수), `tuples`(distinct (addr,chiplet) 쌍)는 **HyperLogLog** sketch로 추정 → PC당 ~1.4 KB, chiplet당 ~17 KB ≈ L2의 0.4% 수준의 HW 비용.
- `classify` 파이프라인: warm-up gate → streaming 필터(reuse<1.5면 Streaming) → decision-tree 또는 weighted-score 분류 → **footprint clamp** → hop promote.
- hotness tier: **Cold / Warm / Hot / Global** (shared track), **Warm_NonShared / Hot_NonShared** (private track), **Streaming / Bypass** (아예 캐시 안 함).
- **footprint clamp (핵심, thrash 방지 장치)**: reuse/sharing이 아무리 높아도, PC의 활성 working set(`unique × fan-in`)이 풀린 L2 용량(65536 라인 = 4 MB/64 B)을 넘으면 tier를 한 단계씩 강등(Global→Hot→Warm→Cold). **즉 "footprint가 풀에 안 들어가면 분산 안 한다"** — 이게 per-chiplet 복제의 thrash 실패 모드를 원천 차단한다.

> hotness → RDMA 효과: Global/Hot/Warm = local home lookup ✓ + 분산 ✓ + L2 설치 ✓. NonShared = 복제 없이 자기 L2에만(eviction 우선순위만 조정). Cold = 일반. Streaming/Bypass = L2 설치조차 안 함(invalidate).

### 3.2 How wide — hotness별 cluster 기하 (`clusterWidthForHotness`, `GetClusterId`)
- hotness tier → cluster 한 변 길이 매핑. 6×6 기준:
  - **Global / Hot → 2×2 cluster** (9개 cluster × 4 chiplet): 작고 촘촘. 복제본 많되 각각 매우 가까움.
  - **Warm → 3×3 cluster** (4개 cluster × 9 chiplet): 넓음. 복제본 적되 각 복제본 뒤 풀 용량 큼.
- `GetClusterId(chipletId, hotness)`가 chiplet을 cluster로 매핑, `numClustersForHotness`가 cluster 수를 준다. **한 cluster는 한 주소당 local home 하나**를 가진다.
- 트레이드오프의 본질: **hotter/small → cluster 작게(복제본 수↑, 접근 거리↓)** vs **warmer/big → cluster 크게(복제본 수↓, 풀 용량↑)**. 복제본 개수를 cluster 수(≤ N_chiplet)로 상한 지어, per-chiplet 복제의 용량 폭발을 막는다.

### 3.3 Where — local home 배치 (`GetLocalHomeIdForCluster`)
- cluster 안에서 local home은 **global home의 cluster-내 offset을 그 cluster에 미러링**해서 정한다(position-based placement).
- 이 배치의 결정적 성질: local home이 항상 **requestor → global home 최소 경로(geodesic) 위**에 놓인다. 즉 lookup chain이 어차피 지나갈 chiplet들을 정확히 probe하게 되어, **우회(detour) 없이** 복제본을 발견한다. (address-hash 배치는 경로와 무관한 슬롯에 흩뿌려 hit율이 낮아짐 — 그래서 position-based를 씀.)

### 3.4 읽기 경로 — lookup chain (`processReadReqFromL2` → `processLookupReqFromOutside`)
원격으로 나가야 하는 L2 miss에서 RDMA 엔진은:
1. PC hotness 조회 → 분산 **scheme**(Global/Hot/Warm) 하나를 고름. (분산 대상 아니면 scheme 없음)
2. scheme가 있으면 **모든 cluster의 local home 후보를 열거**하고 다음을 만족하는 것만 남김:
   (a) requestor도 global home도 아님, (b) requestor→GH 최소 경로 위(`IsOnMinimalPath`), (c) global home보다 엄격히 가까움. 그다음 가까운 순 정렬(X-Y 라우팅 tiebreak) + **chain 단조성 필터**로 `R → c0 → c1 → … → GH`가 곧은 geodesic이 되게 함.
3. 정렬된 chain(`LookupDst`)을 실은 **`LookupReq`**를 보냄. 요청이 chiplet을 타고 global home 쪽으로 전진하며 각 local home이 자기 L2를 **peek**한다:
   - **hit** → 짧은 거리에서 조기 데이터 반환 (full 거리 대신 intra-cluster hop).
   - **miss** → `LookupMissRsp`가 다음 hop / 최종 global home으로 전진.
4. 경로 위에 유효한 local home이 없으면 → 평범한 `ReadReq`를 global home으로 직송(=mesh baseline과 동일, **패널티 없음**).

> 무데드락 보장의 핵심: on-path lookup은 GH 방향 단조 DAG라 coalesce 안전. (요청자 own-cluster local home이 경로에서 벗어난 경우 강제 probe하는 변형에서는, off-path lookup을 **peek-only**로 만들어(coalesce 금지) 상호 MSHR circular wait를 차단.)

### 3.5 쓰기 경로 — distribution (`processDataReadyRspFromL2`)
- global home이 데이터를 서비스할 때 `shouldDistribute`를 판정: requestor와 GH가 **다른 cluster**이고, 그 요청 자체가 lookup이 아니었고, hotness ∈ (Cold, Global]일 때만.
- 참이면 **`DistributeRsp`**를 mesh의 **branch node**에서 분기시켜 두 목적지로 보냄:
  - 하나는 **requestor**(원래 miss 충족),
  - 하나는 requestor cluster의 **local home**(복제본 설치) → 이후 그 cluster 어디서 오는 요청이든 lookup chain으로 이 복제본에 hit.
  - (requestor == local home이면 `CommonDistRsp`로 처리.)
- local home 설치는 L2(`writeback-dynamic-dist` 캐시 변형)에서 일어나고, distribution 전용 지표를 남김: `distribution-hit`, `evict-by-distribution`, `evict-owned`.

---

## 4. 문제 → 해결 매핑 (논문 Contribution 뼈대)

| Wafer-scale GPU 문제 | Dynamic Clustering의 해법 | 메커니즘 근거 |
|---|---|---|
| 공유 데이터마다 global home까지 **긴 거리** (single-home) | 경로 위 local home에서 **조기 hit** → intra-cluster hop으로 단축 | §3.3 position-based 배치 + §3.4 on-path lookup chain |
| per-chiplet 복제의 **용량 thrash** (footprint>4 MB) | **footprint clamp**로 풀에 안 들어가면 분산 자체를 금지 | §3.1 footprint clamp |
| N벌 복제의 **용량 폭발** | cluster당 **딱 한 벌** → 복제본 수를 cluster 수로 상한 | §3.2 cluster 기하 + §3.5 단일 local home 설치 |
| "어떤 데이터를 복제할지" **적응 불가** | **PC hotness 분류**로 hot+shared만 선택 | §3.1 PCTable |
| "얼마나 넓게 복제할지" **고정** | hotness tier별 **가변 cluster 크기** | §3.2 clusterWidthForHotness |
| 복제/분산의 **오버헤드가 무조건 발생** | 분산 대상 아니면 mesh baseline으로 **무손실 fallback** | §3.4 step 4 (plain ReadReq 직송) |

**핵심 주장 한 줄**: Dynamic Clustering은 *복제의 재사용 지역성*을 그 *용량 비용 없이* 취하고, single-home baseline의 평균 hop 거리를 *정확히 이득 나는 트래픽에 대해서만* 줄인다.

---

## 5. 동작 영역 (Operating Regime) — 실험 설계 포인트

- **승리 조건**: `per-chiplet L2 (4 MB) < 공유 footprint < cluster-pool 용량`.
  - footprint < 4 MB → per-chiplet 복제(nearfetch)로 충분 → 동점.
  - footprint > 풀 용량 → 아무도 못 담음 → 다시 thrash. (WSS를 무작정 키우는 방향은 **틀림**.)
  - 그 사이 밴드에서만 dynamic clustering이 이긴다 (nearfetch는 thrash, single-home은 멀다).
- 검증된 성공 사례: LLM 형태 GEMM/MM (gpt3_qkv/ffn, llama70b_ffn 등). 공유 피연산자(활성값 M×K)를 cluster 풀 밴드 안에 들어오게 M(토큰 수)만 줄이고 K/N은 실제 hidden/feature로 둔 shape.
- **LASP 정렬 대칭 필요**: local(비공유) 피연산자의 분할 축이 mesh chiplet 수의 배수여야 home 누수가 없음. (Xdiv → 공유=활성값 M×K, 짧은 M으로 현실적 LLM shape 가능 → 권장. Ydiv → 공유=가중치 K×N, 현실 크기론 밴드 불가 → 억지 직사각형만.)

---

## 6. 한계 · 열린 이슈 (Limitation / Future Work 후보)

- **cluster 크기가 정적**: 현재는 hotness tier별 고정 기하. 이상적으로는 (PC, global home)별 footprint 기반 **동적 cluster 크기**. 벤치별 L2 잔류시간 τ_L2가 37배까지 차이나므로 적응형이 바람직(미구현).
- **footprint clamp의 단일 상수 가정**: L2=65536 라인을 상수로 가정. L2 fill 카운터 기반 적응형 감쇠(windowed working set)가 부분 도입됨(`WSAddrs` rank-decay), 완전 적응형은 미완.
- **online PCTable은 cross-PC packing 불가**: 런타임 분류기는 sibling PC들의 합산 footprint를 싸게 못 봐서 per-PC clamp만 함. Oracle 테이블은 cross-PC packing까지 함(offline).

---

## 7. 코드 앵커 (검증/재현용)

- 플랫폼 빌더: `mgpusim/platform/dynamic_clustering.go`, `-platform-type=dynamic-clustering -cluster-size=<n>`
- RDMA 엔진(심장): `mgpusim/rdma/dynamic_clustering_rdmaengine.go`
  - 읽기/lookup: `processReadReqFromL2` (L145), `processLookupReqFromOutside` (L1306)
  - 쓰기/distribution: `processDataReadyRspFromL2` (L557, `shouldDistribute` L622)
  - 기하/배치: `GetClusterId` (L1449), `clusterWidthForHotness` (L1518), `GetLocalHomeIdForCluster` (L1566)
- hotness 분류: `mgpusim/rdma/pctable.go` (`classify`, `clampByFootprint`)
- cluster map: `mgpusim/rdma/clusterinfo.go` (`GetClusterMap`, `IsOnMinimalPath`)
- L2 캐시 변형: `mem/cache/writeback-dynamic-dist` (distribution 설치 + 지표)
- NoC: `noc/networking/chipnetwork/dynamic_clustering_network.go`, `.../switching/dynamic_clustering_multiflitchipletswitch.go`
