# 계획: `processReadReqFromL2`의 다중 클러스터 Local-Home 열거

## 배경

`DynamicClusteringRdmaEngine`은 캐시라인 복제본을 칩 전체에 분산시키는데, 그 단위가 클러스터마다 1개씩 정해지는 "local home(LH)" 칩렛이다. L2를 우회한 read는 global home(GH)으로 가기 전에 LH를 먼저 찔러봐서 복제본을 가로챌 수 있다. 현재는 `processReadReqFromL2`가 **requestor 자기 클러스터의 LH 1개(Hot)** + 선택적으로 **자기 클러스터의 Warm LH 1개**(`xDim>=6`일 때만)만 후보로 둔다. 6×6 mesh 기준 Hot 스킴 9개 클러스터 중 8개의 복제본은 모두 미사용. `bypass-hash-inter-cluster` 브랜치의 목표는 **PC의 hotness 스킴에 해당하는 모든 클러스터의 LH**까지 lookup을 확장하되, 추가 hop이 발생하지 않도록(= 모든 LH는 R→GH 최단 경로 위, 그리고 chain 자체도 단조롭게) 필터링하는 것.

사용자와 확정된 설계 선택 사항:

- Hotness 검사: `e.PCTable.ReqFromInside(pc)` 사용 (Oracle PC table은 항상 설치되어 nil 불가 — `mgpusim/rdma/dynamic_clustering_rdmaengine.go:1058`).
- PC의 hotness와 **일치하는 스킴만** 열거 (Hot 또는 Warm 중 하나만, 둘 다 X).
- Warm 스킴은 기존 게이트 그대로 `e.xDim >= 6` 일 때만 활성 (현재 line 117/147과 동일).
- 후보 수 제한 없음, R 기준 가까운 순 정렬, 자기 클러스터 LH는 열거에 자연스럽게 포함.

## 수정 대상 파일

- `mgpusim/rdma/dynamic_clustering_rdmaengine.go` — 주요 수정
  - `processReadReqFromL2` (line 89-297): LH 선택 블록 재작성
  - `processLookupMissRspFromL2` (line 329-398): chain 전진 로직 일반화
  - 신규 헬퍼 `GetLocalHomeIdForCluster` 추가 (`GetLocalHomeId` 바로 옆, line 1145 근처)

수정 불필요:

- `protocol/distprotocol.go:259-385` — 이미 임의 길이 `LookupDst`를 지원 (`Build()`이 `LookupLeft = len(b.lookupDst)`로 세팅).
- `IsOnMinimalPath` (`mgpusim/rdma/clusterinfo.go:101`) — 개별 LH의 geodesic 게이트는 이미 구현됨.

## 변경 1 — 신규 헬퍼 `GetLocalHomeIdForCluster`

`GetLocalHomeId` 직후(line 1209) 삽입. body는 line 1150-1208을 mirror하되 requestor에서 cluster ID를 유도하지 않고 인자로 직접 받음:

```go
func (e *DynamicClusteringRdmaEngine) GetLocalHomeIdForCluster(
    clusterId int, addr uint64, hotness protocol.Hotness,
) int {
    // ... line 1150-1184와 동일한 clusterWidth switch
    // ... 분산 불가 hotness는 line 1186-1189처럼 globalHome 반환
    clustersPerRow := e.xDim / clusterWidth
    startX := (clusterId % clustersPerRow) * clusterWidth
    startY := (clusterId / clustersPerRow) * clusterWidth
    offset := int(hashAddrForLocalHome(addr) % uint64(clusterWidth*clusterWidth))
    mappedX := startX + offset%clusterWidth
    mappedY := startY + offset/clusterWidth
    return mappedY*e.xDim + mappedX
}
```

기존 `GetLocalHomeId`(line 1145)는 한 줄로 위임: `return e.GetLocalHomeIdForCluster(e.GetClusterId(requestorId, hotness), addr, hotness)`. 기존 호출부 그대로 두고 placement 로직만 통합.

클러스터 개수는 `GetClusterId`(line 1078-1110)의 `div` 값으로부터 유도: xDim=6 → Hot은 `div=3` → 9 클러스터; Warm은 `div=2` → 4 클러스터. switch 중복을 피하기 위해 `numClustersForHotness(hotness) int` 헬퍼를 추가하여 `div*div`를 반환.

## 변경 2 — `processReadReqFromL2`의 LH 선택부 재작성

line 113-224(hot/warm 쌍 계산, eligibility 체크, 2개짜리 chain monotonicity, hot/warm 수동 swap) 전체를 hotness 게이트 기반 열거로 교체. line 89-112(front matter, `addr`, `requestorId`, `globalHome`, `globalHomeId`, `toGlobalHome`)와 line 215 이후(`shouldLookup` dispatch, tracing)는 그대로 유지.

교체 블록 의사코드:

```go
pc, _ := req.Info.(uint64)
hotness := e.PCTable.ReqFromInside(pc)

var scheme protocol.Hotness
var numClusters int
switch {
case hotness == protocol.Hot:
    scheme = protocol.Hot
    numClusters = e.numClustersForHotness(scheme)
case hotness == protocol.Warm && e.xDim >= 6:
    scheme = protocol.Warm
    numClusters = e.numClustersForHotness(scheme)
default:
    // Cold / Global / Bypass / Streaming / Warm@xDim<6 → LH lookup 안 함.
    // line 239-244의 direct-GH 분기로 그대로 fall-through.
}

type cand struct {
    id   int
    port akita.Port
    dist int
}
seen := map[int]bool{requestorId: true, globalHomeId: true}
cands := []cand{}

if scheme != 0 { // hotness 게이트 통과
    for c := 0; c < numClusters; c++ {
        lh := e.GetLocalHomeIdForCluster(c, addr, scheme)
        if seen[lh] { continue }
        if !IsOnMinimalPath(requestorId, globalHomeId, lh, e.xDim) { continue }
        port := e.RdmaReqPortByChipletId[lh]
        dist := e.GetDistance(port.Component().(RDMAEngine))
        if dist >= toGlobalHome { continue } // line 143의 "GH보다 가까워야 한다" 게이트 보존
        seen[lh] = true
        cands = append(cands, cand{lh, port, dist})
    }
    sort.Slice(cands, func(i, j int) bool { return cands[i].dist < cands[j].dist })
}

// Chain monotonicity 패스: R → c0 → c1 → ... → GH가 단조 geodesic이 되도록
// 후보를 추려냄. 2-D mesh에서 같은 거리에 있는 두 LH가 둘 다 R→GH geodesic
// 위에 있어도 서로에 대해서는 "측면"일 수 있어 detour 발생 가능.
// (예: R=(0,0), GH=(3,3), P1=(1,2), P2=(2,1) — R→P1→P2 경로는 2-hop detour.)
// 정렬된 거리 기준으로 greedy하게 단조 chain 유지.
filtered := cands[:0]
prev := requestorId
for _, c := range cands {
    if !IsOnMinimalPath(prev, globalHomeId, c.id, e.xDim) { continue }
    filtered = append(filtered, c)
    prev = c.id
}

lookupPorts := make([]akita.Port, len(filtered))
for i, c := range filtered {
    lookupPorts[i] = c.port
}
```

기존 dispatch 블록(line 215-244)은 유지 — `shouldLookup := len(lookupPorts) > 0`이 빈 후보의 경우 직접 GH로 ReadReq를 보내는 분기로 자연스럽게 빠진다.

참고: line 220-224의 수동 hot/warm swap은 이제 불필요(정렬이 처리).

## 변경 3 — `processLookupMissRspFromL2` 일반화

현재(line 346-376): handler가 `originalReq.LookupLeft`를 감소시키고, `nextDst = originalReq.LookupDst[1]`로 인덱스 1을 하드코딩, 그리고 forwarded request의 builder에서 `WithLookupDst(...)` 호출을 **누락**. 결과적으로 다음 hop은 `LookupDst=nil, LookupLeft=0`을 받아 chain이 두 번째 hop 이후 끊김. LH가 2개일 때만 동작. N>2일 때 깨짐.

"strip-head" 컨벤션으로 변경 — `LookupDst[0]`은 항상 "내가 지금 있는 곳":

```go
remaining := originalReq.LookupDst[1:] // 방금 방문한 head 제거
if len(remaining) > 0 {
    nextDst = remaining[0]
    req = protocol.LookupReqBuilder{}.
        WithSendTime(now).
        WithSrc(originalReq.Meta().Src).
        WithDst(nextDst).
        WithLookupDst(remaining). // <-- 누락되어 있던 호출
        WithPID(originalReq.PID).
        WithAddress(originalReq.Address).
        WithByteSize(originalReq.AccessByteSize).
        WithInfo(originalReq.Info).
        Build()
    req.(*protocol.LookupReq).IsPtw = originalReq.IsPtw
} else {
    // 마지막 hop → global home으로 ReadReq fall-back (기존 line 366-375)
    nextDst = e.RdmaReqAddressTable.Find(originalReq.Address)
    // ... 기존 ReadReqBuilder 블록 그대로
}
```

line 346의 `originalReq.LookupLeft -= 1`은 제거 가능 — `Build()`가 `LookupLeft = len(b.lookupDst)`로 자동 세팅(`protocol/distprotocol.go:385`), 즉 derived state. line 351의 `LookupLeft > 0` 체크는 `len(remaining) > 0` 체크로 대체.

기존 2-port case와 backwards-compatible: `LookupDst=[A,B]`로 도착 → A에서 `remaining=[B]` → B로 forward(`LookupDst=[B], LookupLeft=1`) → B 도착 후 `remaining=[]`이 되어 final `ReadReq`로 GH 전송. 관찰 가능한 동작 동일, 단 이제 N hop 일반화.

## 엣지 케이스

- **후보 0개** (모든 클러스터 필터 탈락 또는 hotness 게이트로 direct-GH 진입): 기존 direct-GH 분기(line 238-244)로 자연 fall-through. 별도 처리 불필요.
- **후보 1개**: `LookupReqBuilder` 경로 그대로 사용. 1 hop 후 `remaining=[]`이 되어 patched handler에서 `ReadReq` fall-through.
- **여러 클러스터가 같은 칩렛 ID로 매핑**: `seen` set이 이미 추가된 ID, `requestorId`, `globalHomeId`를 제외하므로 self-lookup이나 GH-lookup 발생 X.
- **Tracing**: 기존 `hop-trace` / `pte-caching-remote-ptw-trace` task(line 272-290)는 cloned request당 1개씩 `srcChip → globalHome`으로 시작; chain 길이는 task lifetime에 영향 없음. 변경 없음.
- **PCTable nil**: 발생 불가 — `NewDynamicClusteringRdmaEngine`이 line 1058에서 무조건 초기화(line 1065의 조건문은 중복 재할당).

## 검증

1. **컴파일**: 워크트리 루트에서 `cd mgpusim/samples/pagerank && go build` 성공해야 함. `cd mgpusim && go test ./rdma/...`로 chain protocol 회귀 확인(필요 시 `go generate ./...`로 mock 재생성).
2. **End-to-end 실행** — `.vscode/launch.json`의 `pagerank dynamic-clustering 6x6` 구성을 그대로 따라 함:

   ```bash
   mgpusim/samples/pagerank/pagerank \
     -timing -report-all -verify -no-progress-bar \
     -platform-type=dynamic-clustering \
     -xDim=6 -yDim=6 \
     -use-oracle-pc-table \
     -scheduling=lasp -sched-partition=Xdiv \
     -mem-allocator-type=lasp -use-lasp-mem-alloc
   ```

   `~/mgvm/CLAUDE.md` 메모대로 pagerank가 canonical sanity benchmark. `MEMORY.md`의 slurm-permission 메모에 따라 **명시적 승인 없이 slurm 제출 금지**.
3. **확인할 메트릭** (runner의 `-metric-file-name`에 해당하는 CSV):
   - `total-distribution-hit-count`, `lookup-distribution-hit-count` — 같은 입력 대비 **증가**해야 함. 더 많은 클러스터가 geodesic 위에서 reach 가능하므로 hit 확률 상승.
   - RDMA 트랜잭션 카운트(`Runner.reportRDMATransactionCount` in `mgpusim/runner/runner_report_trace.go:1470`) — 비례적으로 폭증해서는 안 됨. geodesic 필터가 chain 길이를 R→GH의 L1 거리 이내로 cap하므로 request당 hop은 bounded.
   - 전체 simulated cycles — 회귀 없어야 함. 오히려 일찍 hit하는 chain은 평균 miss latency를 줄일 수 있음.
4. **Distribution-* 메트릭 sanity** (`MEMORY.md` reference 메모 참조): `distribution-hit`, `evict-by-distribution`, `evict-owned`이 새 정책 하에서도 self-consistent해야 함(hits ≤ evicts-owned 등).
5. **작은 mesh에서 diff 확인**: `-xDim=4 -yDim=4` 단발 실행으로 현재 동작과 사실상 같은지 확인(Warm 스킴은 게이트 off; Hot은 4 클러스터 모두 2×2이며 inner-cluster LH는 기존 코드가 on-geodesic일 때 선택하던 것과 동일). 소형 케이스 회귀 없음을 확인.

---

# Plan v2 추가 — X-Y Routing 정렬 tiebreaker

## 배경 (v2)

`bypass-hash-inter-cluster` 정책에서 chain 방문 순서가 코드베이스의 X-Y 라우팅 컨벤션과 일치하도록 정렬을 보강. 사용자 결정 사항(v2):

> 같은 L1 거리에 있는 LH 후보들이 있을 때, X 방향 진행이 더 많은 쪽(Δx가 큰 쪽)을 먼저 방문한다.

코드베이스 라우팅 컨벤션 확인:

- `noc/networking/routing/meshroutingtable.go:74` — 주석 "Find next port with X-Y routing"; X축 분기를 Y축보다 먼저 검사(line 81-106).
- `mgpusim/rdma/rcrdmaengine.go:394-416` (`getNextHopChipletId`) — X-first dimension-order routing 구현(srcX→dstX 먼저, 그다음 Y).
- 칩렛 ID 인코딩은 row-major: `chipletId = y*xDim + x` (`mgpusim/rdma/util.go:29-30`, `dynamic_clustering_rdmaengine.go:1208`).

즉 X-first 컨벤션이 이미 확립. v2 정렬도 X-first tiebreak으로 동일하게 맞춤.

## 변경 4 — 정렬 비교 함수에 Δx tiebreaker 추가

`processReadReqFromL2`(현재 line ~161, Plan v1 직후 구현된 sort 부분)의 sort 비교 함수를 L1 거리만 비교하던 것에서 **(L1 거리 오름차순, Δx 내림차순)** lex 비교로 확장.

### 4.1 `lhCand` 구조체에 Δx 추가

기존:

```go
type lhCand struct {
    id   int
    port akita.Port
    dist int
}
```

변경:

```go
type lhCand struct {
    id   int
    port akita.Port
    dist int
    dx   int // |lhX - requestorX| — X-Y tiebreaker용
}
```

### 4.2 후보 생성 루프에서 `dx` 계산

좌표 디코딩은 row-major 컨벤션(`y = id/xDim`, `x = id%xDim`)을 그대로 따른다 — `mgpusim/rdma/util.go:77-80` (`GetDistanceById`)에서 확인됨.

후보를 append하기 직전에 추가:

```go
lhX := lhId % e.xDim
reqX := requestorId % e.xDim
dx := Abs(lhX - reqX) // Abs는 mgpusim/rdma/util.go:5 에 이미 존재
cands = append(cands, lhCand{lhId, port, dist, dx})
```

`Abs` 헬퍼는 이미 있는 `mgpusim/rdma/util.go:5`의 `Abs(x int) int` 재사용. 신규 헬퍼 불필요.

### 4.3 sort.Slice 비교 함수 교체

기존:

```go
sort.Slice(cands, func(i, j int) bool { return cands[i].dist < cands[j].dist })
```

변경:

```go
sort.Slice(cands, func(i, j int) bool {
    if cands[i].dist != cands[j].dist {
        return cands[i].dist < cands[j].dist
    }
    // X-Y routing tiebreaker: 같은 L1 거리면 Δx가 큰 쪽(= X로 더 많이 전진한 쪽)을
    // 먼저 방문한다. 코드베이스의 X-first dimension-order 컨벤션과 일치
    // (noc/networking/routing/meshroutingtable.go:74).
    return cands[i].dx > cands[j].dx
})
```

### 4.4 chain monotonicity 패스 영향

기존 chain monotonicity 필터(현재 line ~170-178)는 그대로 유지. 새 정렬 키 하에서도 동일하게 작동:

- 예시 R=(0,0), GH=(3,3), 후보 P1=(1,2)·dx=1, P2=(2,1)·dx=2, 둘 다 dist=3.
- v1(L1만): 어떤 순서가 먼저 올지 불확실(stable이 아닐 수 있음). greedy filter가 먼저 온 쪽을 keep.
- v2(Δx tiebreak): P2가 먼저. greedy filter가 P2 keep, P1은 `IsOnMinimalPath(P2, GH, P1) == false`로 drop. 결과 chain R→P2→GH가 X-Y routing에 부합.

## 변경 영향 없음

- 변경 1 (헬퍼들): 그대로.
- 변경 3 (`processLookupMissRspFromL2` strip-head): 그대로.

## 추가 검증

- `cd mgpusim/samples/pagerank && go build` 재검증.
- 동일 입력으로 v1 vs v2 결과 비교: dist-trace에서 chain 첫 hop의 좌표가 X 방향에 더 가까운지 확인. `dist-trace`의 hop sequence가 X-Y 형태(가능하면 dy=0인 hop 먼저)를 띠어야 함.
- `total-distribution-hit-count` 같은 메트릭은 v1 대비 큰 변화 없어야 함(후보 집합 자체는 동일, 순서만 다름; chain monotonicity 필터링 결과만 일부 달라짐).

---

# Plan v3 — Distributed cacheline 보존 강화 (확률적 hotness 승급)

## 배경 (v3)

`writeback-dynamic-dist` 캐시는 second-chance 알고리즘으로 hot 라인을 보존하지만 (`mem/cache/writeback-dynamic-dist/directory.go:191`, `LRUVictimFinder.FindVictim`), distributed 라인이 자주 evict되는 현상 관찰. 원인: read hit 시 `directorystage.go:177-178`에서 `directory.Visit(block); block.ReadCount++` 만 일어나고 **hotness 승급은 없음**. 따라서 자주 접근되는 distributed Hot(2) 라인도 두 번의 victim-scan 강등 패스로 곧 evict.

해결: read hit 시 **확률적으로 hotness 승급** (LFU-ish second chance). 자주 접근되는 라인이 자연스럽게 Global(3)까지 올라가서 강등 사이클을 더 견딤. 사용자 결정 사항:

- 승급 상한: `protocol.Global(3)` — 기존 enum 내, 추가 비트 X.
- 대상: 모든 Warm+ 블록 (distributed 여부 무관). Cold/Bypass/Streaming/Global은 변동 X. `_NonShared` variants 는 캐시에 저장될 때 plain으로 매핑되므로(`bypasspolicy.go:53` `storableRdmaHotness`, `writebufferstage.go:658,660,698,700`) 사실상 Warm/Hot/Global 만 등장.
- Trigger: **read hit만** (`directorystage.go:177` 경로). write hit (`directorystage.go:622` `writeToBank`) 제외.
- 확률 n%: CLI 플래그 `-hotness-promote-percent=N` (기본 25). 0이면 disable.
- 자매 모듈 `mem/cache/writeback-dynamic-clustering-rep/directory.go:122`이 거의 동일한 패턴(`if block.LFUCounter < 3 && rand.Float64() < 0.25`) — convention 재사용.

## 수정 대상 파일

7개 파일 변경, plumbing 5단계 (flag → platform builder → GPU builder → cache builder → directory):

1. `mgpusim/runner/runner.go` — flag 선언 + dispatch wire
2. `mgpusim/platform/dynamic_clustering.go` — platform builder
3. `mgpusim/builders/dynamic_clustering.go` — GPU builder field + setter
4. `mgpusim/builders/dynamic_clustering_func.go` — l2Builder chain
5. `mem/cache/writeback-dynamic-dist/builder.go` — cache builder field + setter + `NewDirectory` arg
6. `mem/cache/writeback-dynamic-dist/directory.go` — `DirectoryImpl` 필드, `Directory` 인터페이스에 `PromoteOnHit`, 구현
7. `mem/cache/writeback-dynamic-dist/directorystage.go` — read hit 경로에서 호출
8. `mgpusim/samples/spmv/oracle_dynamic_clustering` — 셸 스크립트에 플래그 전달 추가

## 변경 5 — flag → cache directory plumbing

각 단계 패턴은 기존 `clusterSizeFlag` (runner.go:182 → 868 → platform.WithClusterSize:23 → builder.WithClusterSize:23) 와 동일.

### 5.1 `mgpusim/runner/runner.go`

- line 183 뒤에 추가:
  ```go
  var hotnessPromotePercentFlag = flag.Int("hotness-promote-percent",
      25, "Percent chance to promote a cached block's hotness on read hit "+
          "(0 disables, applies only to writeback-dynamic-dist L2)")
  ```
- line 866-868 (case `"dynamic-clustering"`) 를 다음으로 확장:
  ```go
  case "dynamic-clustering":
      b = platform.MakeDynamicClusteringGPUPlatformBuilder().
          WithClusterSize(*clusterSizeFlag).
          WithHotnessPromotePercent(*hotnessPromotePercentFlag)
  ```

### 5.2 `mgpusim/platform/dynamic_clustering.go`

- `DynamicClusteringGPUPlatformBuilder` 구조체(line 12)에 `hotnessPromotePercent int` 필드 추가.
- `WithClusterSize` (line 23) 옆에 setter:
  ```go
  func (b *DynamicClusteringGPUPlatformBuilder) WithHotnessPromotePercent(n int) *DynamicClusteringGPUPlatformBuilder {
      b.hotnessPromotePercent = n
      return b
  }
  ```
- `createGPUBuilder` (line 71) 의 line 94 (`WithClusterSize`) 뒤에 `gpuBuilder.WithHotnessPromotePercent(b.hotnessPromotePercent)` 추가.

### 5.3 `mgpusim/builders/dynamic_clustering.go`

- `DynamicClusteringGPUBuilder` 구조체(line 11)에 `hotnessPromotePercent int` 필드 추가.
- `WithClusterSize` (line 23) 옆에 setter:
  ```go
  func (b *DynamicClusteringGPUBuilder) WithHotnessPromotePercent(n int) {
      b.hotnessPromotePercent = n
  }
  ```

### 5.4 `mgpusim/builders/dynamic_clustering_func.go` (buildMemBanks)

- line 85-92의 `l2Builder := writeback_dynamic_dist.MakeBuilder()...` 체인에 `WithHotnessPromotePercent(b.hotnessPromotePercent)` 추가 (다른 chained `With*` 들과 동일 위치).

### 5.5 `mem/cache/writeback-dynamic-dist/builder.go`

- `Builder` 구조체 (line 13-30) 에 `hotnessPromotePercent int` 필드 추가.
- 다른 `WithMaxInflight*` setter (line 112-122) 옆에:
  ```go
  func (b Builder) WithHotnessPromotePercent(n int) Builder {
      b.hotnessPromotePercent = n
      return b
  }
  ```
- `configureCache` (line 146) 의 `NewDirectory` 호출(line 150-151)을 새 시그니처로:
  ```go
  directory := NewDirectory(
      numSet, b.wayAssociativity, blockSize, vimctimFinder, b.hotnessPromotePercent)
  ```

## 변경 6 — `DirectoryImpl` + `PromoteOnHit` 메서드

`mem/cache/writeback-dynamic-dist/directory.go`:

### 6.1 imports

`math/rand/v2` 추가 (sister module과 동일 convention — `writeback-dynamic-clustering-rep/directory.go:122`).

### 6.2 `Directory` 인터페이스에 메서드 추가 (line 33-42)

```go
type Directory interface {
    Lookup(pid ca.PID, address uint64) *Block
    FindVictim(address uint64) *Block
    FindVictimWithoutDemotion(address uint64) *Block
    Visit(block *Block)
    PromoteOnHit(block *Block) // <-- 신규
    TotalSize() uint64
    WayAssociativity() int
    GetSets() []Set
    Reset()
}
```

### 6.3 `DirectoryImpl` 필드 + 생성자 시그니처

```go
type DirectoryImpl struct {
    NumSets             int
    NumWays             int
    BlockSize           int
    Sets                []Set
    victimFinder        VictimFinder
    hotnessPromotePercent int // 0 disables
}

func NewDirectory(
    set, way, blockSize int,
    victimFinder VictimFinder,
    hotnessPromotePercent int,
) *DirectoryImpl {
    d := new(DirectoryImpl)
    d.victimFinder = victimFinder
    d.hotnessPromotePercent = hotnessPromotePercent
    d.Sets = make([]Set, set)
    d.NumSets = set
    d.NumWays = way
    d.BlockSize = blockSize
    d.Reset()
    return d
}
```

### 6.4 `PromoteOnHit` 구현

`Visit` (line 121) 옆에 추가:

```go
// PromoteOnHit probabilistically bumps a block's Hotness by one on read hit.
// Promotion only fires when the block is currently Warm or Hot (Cold blocks
// stay Cold per design; Global is already saturated; Bypass/Streaming and
// any _NonShared values never appear on a resident block — see
// bypasspolicy.go:storableRdmaHotness and writebufferstage.go install paths).
// Percent of 0 disables; sister convention at
// mem/cache/writeback-dynamic-clustering-rep/directory.go:122.
func (d *DirectoryImpl) PromoteOnHit(block *Block) {
    if d.hotnessPromotePercent <= 0 {
        return
    }
    if block.Hotness != protocol.Warm && block.Hotness != protocol.Hot {
        return
    }
    if rand.IntN(100) < d.hotnessPromotePercent {
        block.Hotness++ // Warm→Hot, Hot→Global
    }
}
```

`block.Hotness++` 는 enum 산술이지만 분기에서 Warm(1)과 Hot(2)만 통과시켰으므로 결과는 항상 Hot(2) 또는 Global(3). `_NonShared`(4, 5) 로 누출될 위험 없음.

## 변경 7 — read hit 경로에서 호출

`mem/cache/writeback-dynamic-dist/directorystage.go` line 177-178:

```go
ds.cache.directory.Visit(block) // update LRU list
block.ReadCount++
ds.cache.directory.PromoteOnHit(block) // <-- 신규
```

write hit 경로 (`writeToBank` line 607-622) 는 **변경 없음** — 사용자 명세 "read hit만" 유지.

## 변경 8 — `oracle_dynamic_clustering` 셸 스크립트 4개

다음 4개 모두 동일 패턴으로 수정:

- `mgpusim/samples/spmv/oracle_dynamic_clustering`
- `mgpusim/samples/mis/oracle_dynamic_clustering`
- `mgpusim/samples/pagerank/oracle_dynamic_clustering`
- `mgpusim/samples/gemm/oracle_dynamic_clustering` (llama3_70B_gemm 별칭 사용)

스크립트마다 기존 positional arg 패턴이 다름:

- spmv: `$1=X_DIM`, `$2=DIM`
- mis: `$1=X_DIM`, `$2=NUM_NODES`
- pagerank: `$1=X_DIM`, `$2=NODE`
- gemm: positional arg 없음 (XDIM/NI/NJ/NK 모두 하드코딩)

기존 위치 인자 패턴과 충돌하지 않도록 **환경 변수 `PROMOTE`** 로 통일:

각 스크립트 상단(go build 직후)에 추가:

```bash
PROMOTE=${PROMOTE:-25}
```

각 스크립트의 binary invocation flag 리스트(끝부분, `-yDim=$Y_DIM` / `-yDim=$YDIM` 근처)에 추가:

```bash
-hotness-promote-percent=$PROMOTE \
```

Usage 예시:

```bash
./oracle_dynamic_clustering                 # default PROMOTE=25
PROMOTE=0 ./oracle_dynamic_clustering       # 기존 동작 (승급 X)
PROMOTE=50 ./oracle_dynamic_clustering 6 2048
```

이 방식의 장점: 모든 스크립트의 기존 positional arg semantics 보존, 일관된 사용법. `PROMOTE=0` 한 줄로 회귀 테스트 가능.

## 엣지 케이스

- **`hotnessPromotePercent = 0`**: `PromoteOnHit` early return, 기존 동작과 완전 동일 → 회귀 위험 0.
- **다른 캐시 모듈**: writeback-dynamic-dist 만 수정. `writeback/`, `writebackrcr/`, `writeback-dynamic-clustering-rep/` 등 다른 모듈은 영향 없음.
- **RNG 결정론성**: `math/rand/v2`의 글로벌 RNG는 런타임 시드 사용 → 같은 입력에 대해 run-to-run variance 존재. sister module과 동일 convention이므로 수용. 만약 결정론 필요시 추후 `rand.New(rand.NewPCG(seed, seed))` 로 per-directory RNG로 전환 가능 (현재 plan에는 포함 안 함).
- **`virtualBitForLocalHome`**: 본 plan에서는 활용 X (사용자 명세 "모든 Warm+ 블록"). distributed-only로 좁히고 싶다면 `PromoteOnHit` 첫 줄에 `if !block.virtualBitForLocalHome { return }` 추가하면 됨.
- **다른 캐시 변종의 Directory 인터페이스**: 인터페이스에 `PromoteOnHit` 메서드를 추가하므로 이 인터페이스를 implement하는 **모든 타입에 메서드 필요**. `writeback-dynamic-dist` 디렉토리 안에 다른 구현이 있는지 확인 필요 — 현재 `DirectoryImpl` 단일 구현으로 보임 (`grep -n "type.*Directory" mem/cache/writeback-dynamic-dist/` 로 검증).

## 검증

1. **컴파일**: `cd mgpusim/samples/spmv && go build` 통과. `cd mgpusim && go test ./...` 으로 mock 회귀 확인 (만약 `Directory` 인터페이스 mock이 있으면 `go generate ./...` 재생성 필요).
2. **회귀 확인** — `-hotness-promote-percent=0` 으로 실행 시 기존 동작과 동일한 결과 (`total-distribution-hit-count`, evict 카운트 등) 가 나와야 함.
3. **효과 확인** — `-hotness-promote-percent=25` (default) vs `=50` vs `=75` 로 동일 입력 비교:
   - **`evict-by-distribution`** (`MEMORY.md` reference) 감소 — distributed 라인이 더 오래 살아남음.
   - **`total-distribution-hit-count`** / **`lookup-distribution-hit-count`** 증가.
   - 전체 simulated cycles는 큰 차이 없거나 약간 감소 (miss latency 감소 효과).
4. **확률성 sanity** — `=0`, `=100` 두 극단:
   - `=0`: 승급 0회, 기존 동작.
   - `=100`: 모든 read hit이 승급 → Warm/Hot 라인이 빠르게 Global로 올라가 거의 evict 안 됨 (set 포화 가능성). 이건 stress test 용 ad-hoc 검증.
5. **자매 모듈 비교**: `writeback-dynamic-clustering-rep` 의 LFUCounter 메커니즘과 비교해 비슷한 trend (hit↑, evict↓) 가 나오는지 확인.
