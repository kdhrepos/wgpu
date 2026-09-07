# ID string → uint64 리팩토링 인계문서

작업 워크트리: `/home/ehdgus7279/mgvm/.claude/worktrees/id-uint64-refactor`
브랜치: `id-uint64-refactor` (master 기준 4038a38에서 분기). **master 절대 수정 금지.**
목적: GC 압박 해소. ID가 string이라 (1) 매 Generate가 strconv 할당, (2) `map[string]` 키로 수백만 string backing array가 GC scan 대상, (3) tracing이 suffix concat으로 string 폭발.

---

## ✅ 완료 상태 (2026-07-06)

**전 모듈(akita/protocol/util/mem/noc/mgpusim) 포팅 완료 + 빌드/vet clean.**
커밋: 2661ec6(akita/protocol/util) → 558237d(mem) → cf51132(noc) → b06db0f(mgpusim finalize) → 1f63948(vet %s→%d) → **989fb72(kernel.go xid 버그 수정)**.

검증(mgpusim 드라이버 = 로컬 akita):
- `go build ./...` → baseline 에러 2개(histogram hsaco, samples/test dummy1)만. ID 관련 에러 0.
- `go vet ./...` → clean.
- 테스트: util(pipelining TaskID uint64/akitaext/util) 전부 pass, mgpusim(bptree/root) pass. DNN/mccl build-failed = master에도 있는 기존 test rot(`MakeR9NanoBuilder`/`MakeEmuBuilder`/`NewFullyConnectedLayer` API 드리프트, ID 무관).
- akita/util/protocol/mem/noc = 이 체크아웃에 유닛테스트 파일 없음.

**설계 확정: `Command.ID`/`wf.UID`/`wg.UID`/`d.simulationID`는 string 타입 유지**(reqhookinfo.CommandID·외부 API·로그·맵키용). 단 생성은 전부 `xid`→`strconv.FormatUint(akita.GetIDGenerator().Generate(),10)` **십진 숫자문자열**로, tracing엔 `tracing.DecimalTaskID(string)→uint64`(ParseUint base10)로 파싱해 넘김. generator는 1부터 시작(0 반환 없음) → ID 0 = "none/root" sentinel 안전. ∴ 이 4종은 전부 파싱 성공·유니크·nonzero.

**989fb72 버그(발견·수정)**: `driver/kernel.go:107 LaunchKernelCommand.ID`만 `xid.New().String()`(base32) 잔존 → `DecimalTaskID`가 base10 파싱 실패 → 커널런치 task ID 항상 0(서로+no-task sentinel과 충돌). 형제(api.go) 패턴으로 교체, xid import 제거. 드라이버 command 5개 생성자 전부 숫자 ID 확정.

**남은 xid 사용처(정상, 유지)**: `util/tracing/{json,mysql,mongodb}tracer.go`의 DB이름/파일명 — task/msg ID 아님.

⚠️ eventqueue tie-break 동작변경(아래) 여전히 유효 — master 대비 cycle 메트릭 불일치, pagerank verify는 Passed.

아래는 원본 인계 내용(작업 이력).

---

## 진행 상태 (커밋됨, 빌드 검증 완료)

**akita / protocol / util = 완료·검증.** mem/noc/mgpusim = 미완.

### akita (core)
- `idgenerator.go`: `IDGenerator.Generate() uint64`. seq/parallel 둘 다 `atomic.AddUint64`. xid import 제거.
- `msg.go`: `MsgMeta.ID uint64`.
- `event.go`: `EventBase.ID uint64`, `Event.GetId() uint64`.
- `portmsglogger.go:29`: ID `%s`→`%d`.
- `eventqueue.go`: **수정 안 함. 동작 변경 발생** (아래 ⚠️ 참고).

### protocol
- 전 Rsp의 `RespondTo`/빌더 `rspTo`/`WithRspTo(id)`/`GetRespondTo()`/`HopTraceID` → uint64.
  (perl 일괄: protocol.go, distprotocol.go, distprotocol2.go, rcrprotocol.go, common_distrsp_protocol.go)

### util/tracing (핵심 설계)
- **신규 `taskid.go`** = task ID 비트 인코딩 + domain 레지스트리.
  레이아웃: `base[0:46] | kind[46:50] | domain[50:64]`.
  base = msg/event/transaction ID (전부 generator, < 2^46이라 안전).
  kind = req_out(1)/req_in(2)/trans_req(3)/pipeline(4). req_in만 domain 사용.
  domain = `domainID(name)` 레지스트리(per-run 단조 카운터, mutex). string concat 대체.
  헬퍼: `ReqOutTaskID/TransReqTaskID/PipelineTaskID(base uint64) uint64`, `encodeTaskID`, `domainID`.
- `api.go`: StartTask/AddTaskStep/EndTask/TraceReqInitiate 시그니처 `string`→`uint64`.
  `MsgIDAtReceiver` → `encodeTaskID(msgID, kindReqIn, domainID(name))`.
  concat(`+"_req_out"`, `Sprintf %s@%s`, `+"-trace-trans-req"`) 전부 헬퍼 호출로 교체.
- `task.go`: `Task.ID`/`ParentID` uint64.
- `pipelining/pipeline.go`: `PipelineItem.TaskID() uint64`, concat `+"pipeline"` → `tracing.PipelineTaskID(...)`.
- 트레이서 ~20개: task.ID/msgID 키 `map[string]`→`map[uint64]` (inflightTasks/tracingTasks/inflight/entries/perMsg/inFlight 등). **name/kind/What/PC 키 맵은 string 유지** (안 건드림).
- 특수 처리:
  - `conditionalaveragetimetracer.go`: `strings.Contains(task.ID,"addr-translator-stats")` panic guard 제거(dead), strings import 제거.
  - `translationreqtracer.go`: line 92 `strings.Contains(task.ID,"trace-trans-req")` dead 블록 제거(strings는 60/63 컴포넌트명 Split에 여전히 씀, import 유지).
  - `memoryaccesstracer.go`: `accessInfo.isVector/accessType` string→uint64, CSV `%s,%s`→`%d,%d`. (이 둘은 task ID가 아니라 type/vector **정수 페이로드**다 — 아래 참고)
  - `workgroupdimensiontracer.go`: 맵 키가 task.ID와 `strconv.Itoa(kernelID)` 둘 다였음 → 둘 다 kernelID 정수 페이로드. `uint64(kernelID)`로 통일, strconv import 제거.
  - `lookupuniquetracer.go`: entries/entriesPte map + markHit/markMiss(id) uint64.
  - `portqueuetracer.go`: inFlight 맵만 uint64(perPort는 string 유지).

---

## ⚠️ 시뮬레이션 동작 변경 (사용자 반드시 인지)

**`eventqueue.go:62-64` Less() tie-break.** 같은 VTime 이벤트 정렬 시 `GetId()` 비교:
- 이전: string 사전순 (예: "10" < "2").
- 이후: uint64 수치순 (10 > 2 = 생성순/FIFO).

→ **같은 시각 이벤트 처리 순서가 master와 달라짐 → cycle-level 메트릭이 master와 불일치.**
기능 정확성(pagerank verify Passed)은 유지됨. ID 리팩토링 중 **유일하게** 시뮬 의미에 영향 주는 지점.
수치순이 더 합리적(생성순)이라 그대로 둠. master 대비 회귀 비교 시 이 점 감안.

---

## ⚠️ 빌드/검증 토폴로지 (중요 — 안 그러면 헛수고)

- **akita는 standalone 빌드 불가** (`serialengine.go`가 `tebeka/atexit` import하는데 akita/go.mod에 없음 — master도 동일). 시도 금지.
- **util/protocol/mem/noc/lmf의 go.mod는 akita replace가 주석/없음** → standalone `go build ./...`는 **캐시 akita v1.12.1(string ID)** 대상이라 내 변경 미반영, **검증 무의미**.
- **mgpusim go.mod만** 전 sibling을 `../`로 replace (akita,noc,noc/networking,mem,util,lmf,protocol,writebackrcr).
- **∴ 유일한 검증 = mgpusim 모듈을 드라이버로**:
  ```bash
  cd mgpusim
  go build gitlab.com/akita/mem/...      # 임의 패키지 타겟 검증 (로컬 akita 대상)
  go build ./...                          # 전체
  ```
- **mgpusim 베이스라인 기존 에러 2개(무관, 무시)**:
  - `benchmarks/parboil/histogram/benchmark.go:82 kernels.hsaco: no matching files` (ROCm 빌드 산출물 없음)
  - `samples/test/main.go:6 dummy1 패키지 없음`
- 빌드 워닝 `both GOPATH and GOROOT...` 무시.

---

## 남은 작업 (mem → noc → mgpusim → lmf 순, 컴파일러 가이드)

### A. 새 suffix-concat kind 추가 (taskid.go에)
현재 미처리 concat 스킴 (전부 `<base>.ID + "<suffix>"`):
| suffix | 횟수 | 위치 |
|--------|------|------|
| `-addr-translator-stats` | 9 | mem/vm/addresstranslator/{,ideal}addresstranslator.go |
| `_fetch` | 3 | mem/cache/* |
| `_tlb_mshr_wait` | 2 | mem/vm/tlb/tlb.go |
| `_simd_exec` | 2 | mgpusim/timing/cu/* |
| `_L2TLB_stats` | 1 | mem/vm/tlb |

처리: taskid.go에 kind 5~9 상수 + 헬퍼 추가 (예 `AddrTransStatsTaskID`, `FetchTaskID`, `TLBMshrWaitTaskID`, `SimdExecTaskID`, `L2TLBStatsTaskID`). 호출부 `X.Meta().ID+"_fetch"` → `tracing.FetchTaskID(X.Meta().ID)`.
주의: 이 stat 태스크들의 분류는 트레이서 filter가 task.Kind/What로 함(ID suffix는 유일성용)이라 kind 비트로 충분.

### B. mem/device 패키지 (별도 패키지)
- `mem/device/protocol.go`: `TranslationRsp.RespondTo`, `WithRspTo(rspTo string)`, `GetRespondTo()` → uint64.
  (TranslationRspBuilder. `r.ID = Generate()`는 이미 OK.)

### C. DRAM signal
- `mem/dram/internal/signal/command.go:62`: Command 구조체 string ID 필드 → uint64.

### D. xid.New().String() → Generate() (필드도 uint64)
이건 **msg/command ID**라 uint64화:
- mem/cache/protocol.go:179,234
- mgpusim/driver/api.go:210,222,235
- mgpusim/driver/kernel.go:107
- mgpusim/timing/cu/computeunit.go:1407,1461
- 각 파일 xid import 제거 여부 확인.
**예외 — `mgpusim/driver/driver.go:97 d.simulationID = xid.New().String()`는 string 유지** (msg ID 아님, 시뮬 식별자).

### E. PipelineItem.TaskID() string → uint64 (구현체 12개)
mem/cache/{writeback,writebackrcr,writeback-dynamic-dist,writethrough}/*.go의 cachePipelineItem/pwcPipelineItem,
mem/vm/tlb/tlb_with_latency.go tlbPipelineItem,
noc/networking/switching/{switch.go flitPipelineItem, directconnectionwithlatency.go connectionPipelineItem},
mgpusim/timing/caches/l1v/bankstage.go bankTransaction, mgpusim/timing/cu/{memaccessinfo.go,vectormemoryunit.go}.
각 TaskID()가 반환하는 base(보통 trans.id 또는 msg.ID)가 string이면 그 필드도 uint64화.

### F. transaction `id` 필드 + 헬퍼 시그니처
- cache/addresstranslator의 transaction `id`(string, xid 또는 msgID 복사)들 → uint64.
- 헬퍼들 `id string`→`uint64`: findInflightFetchByFetchReadReqID, isReqInBottomByID, findReqToBottomByID, removeReqToBottomByID, findTranslationByReqID, removeReqFromPendingReqList (mgpusim/timing/cp/dma.go), idealaddresstranslator 동등 함수들.

### G. memory-trace 생산자 (type/vector 정수 페이로드)
`tracing.StartTask(<type>, <vec>, now, cache, "memory-trace", ...)`의 첫 2 인자가 task ID가 아니라 **정수 페이로드**(type 0~9, vector 0/1). string 리터럴/변수 → uint64:
- mem/cache/writeback/directorystage.go: `"0".."9"` 리터럴 + `isVec`(`"0"/"1"`)
- mem/cache/writeback-dynamic-dist/directorystage.go: 동일
- mem/cache/writebackrcr/directorystage.go: `typeCode` 변수 + `isVec`
- mem/cache/writeback-dynamic-dist/writebufferstage.go: `"8"/"9"` + `memTraceID`/`victimMemTraceID` 변수
변환 패턴: `"0"`→`0`, `isVec := "0"; isVec="1"` → `var isVec uint64; isVec=1`, typeCode/memTraceID 선언부 uint64화.
(CSV 출력은 정수라 동일 — memoryaccesstracer는 이미 `%d`로 수정됨.)

### H. 익명 task `StartTask("", "", ...)` → `StartTask(0, 0, ...)`
mem/cache/*/directorystage.go, writebufferstage.go, mem/vm/mmu/mmu.go:437, tlb 등 다수. 빈 ID = 매칭 안 쓰는 카운팅 태스크.

### I. 기타 string 연산/맵/출력
- `mem/vm/tlb/entropytracer.go:38`: `strconv.Atoi(task.ID)` → `int(task.ID)` (task.ID가 정수 페이로드).
- mgpusim cp/rob/dispatcher/pmc의 `map[string]`(msg ID 키) → `map[uint64]`.
- mgpusim/rdma/*: `rspTo` 지역변수 타입(보통 `GetRespondTo()` 반환=uint64라 자동), `cloned.Meta().ID = req.HopTraceID`(uint64=uint64 OK).
- `mgpusim/timing/cu/computeunit.go:747`: 디버그 `Printf("...respondTo=%s", rsp.RespondTo)` → `%d`.
- `mgpusim/runner/runner_add_tracer.go:795`: `task.ID != ""` → `task.ID != 0`.
- ID를 CSV/trace/log에 `%s`로 쓰는 곳 전수 점검(`%v`는 컴파일 에러 안 나니 grep 필요): `grep -rn '%s' ... | grep -i '\.ID\|RespondTo'`.

### J. mock 재생성
변경 패키지에 mockgen mock 있으면 `cd <pkg> && go generate ./...` (Ginkgo v1). RespondTo/TaskID 시그니처 바뀐 인터페이스의 mock 갱신 필요할 수 있음.

---

## 검증 체크리스트 (작업 범위 = 빌드+테스트까지, 시뮬은 사용자가)
1. `cd mgpusim && go build ./...` → 베이스라인 에러 2개만 남아야 함.
2. 핵심 패키지 테스트: akita(engine/eventqueue), util/tracing, protocol. (단 테스트도 mgpusim 드라이버 or 해당 모듈 standalone은 캐시 akita 주의 — 가능하면 `cd mgpusim && go test gitlab.com/akita/akita/...` 식.)
3. mock 깨지면 재생성.
4. 사용자에게: **eventqueue tie-break 동작변경**(cycle 메트릭 master와 불일치, 기능은 OK) 반드시 고지. pagerank(mesh)로 sanity.

## 진행 팁
- 컴파일러 가이드로 진행. `cd mgpusim && go build gitlab.com/akita/mem/...` 반복하며 카테고리별 일괄(perl) 수정.
- suffix concat은 반드시 헬퍼로(직접 비트연산 산재 금지) — kind 일관성 유지.
- 정수 페이로드(memory-trace type/vector, workgroup kernelID, entropy task.ID)와 진짜 task ID를 혼동 말 것. 페이로드는 그냥 작은 uint64.
