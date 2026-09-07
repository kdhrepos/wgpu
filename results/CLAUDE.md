# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Purpose

Output directory for `scripts/run.py`. **Not under version control** (the repo root `.gitignore` excludes `*.csv`, `*.trace`, `*.png`, and the result subtrees are sibling to the source tree). Every Slurm sweep dumps its CSV metrics and binary traces here, partitioned by timestamp, platform, and grid size. Analysis scripts in `scripts/` read from this layout.

## Subdirectory layout

```
~/mgvm/results/
├── reports/                        # CSV metrics (-report-* flags)
├── memory-traces/                  # -trace-memory-access (binary)
├── pte-traces/                     # -trace-pte-access (binary)
├── access-counts/                  # -trace-access-count-per-addrs (binary)
├── dist-traces/                    # -trace-dist-filename (dynamic-cluster / dynamic-clustering only)
└── pc-state-traces/                # PC-state traces (currently disabled in run.py)
```

Each subdirectory holds run directories named:
```
<YYYYMMDD-HHMMSS>-<platform>-<xDim>x<yDim>[-small][-round-robin]
```
…or for `static-cluster`:
```
<timestamp>-staticcluster-<cluster_size>
```
The timestamp is set once per `run.py` invocation, so all benchmarks in one sweep land in the same dated directory.

## File naming inside a run directory

Every file is `<benchmark>_<platform>_<xDim>x<yDim>_<kind>.<ext>`, e.g.:
- `reports/<run>/spmv_mesh_4x4.csv` — metrics
- `reports/<run>/logs/spmv_log` — per-component log if `--log`
- `reports/<run>/data-allocation-logs/spmv_mesh_4x4_data_allocation_log` — if `--log-data-allocation`
- `memory-traces/<run>/spmv_mesh_4x4_memory_trace` — `.trace` binary
- `pte-traces/<run>/spmv_mesh_4x4_pte_trace`
- `access-counts/<run>/spmv_mesh_4x4_access_count`
- `dist-traces/<run>/spmv_<dynamic-platform>_<x>x<y>_dist_trace`

Aliased benchmarks (`gpt3_175B_gemm`, `llama3_70B_gemm`, ...) use the `alias` as the filename prefix even though the underlying binary is `gemm` / `matrixmultiplication`. See `scripts/params.py:benchmark_configs[*]['alias']`.

## Reading a report CSV

CSV columns are emitted by the `-report-*` flags in `mgpusim/runner/runner.go`. Column names come from the tracer that registered them (`util/tracing/*tracer.go`). When a new metric appears or disappears between runs, look at the flag-set diff in the sweep command rather than guessing — `run.py` always passes `-report-all`, but the tracer list can change with the platform (e.g. `-report-hop-count-per-rdma` only makes sense on multi-chiplet platforms).

## Metric dictionary (`where, what, value`)

각 행 = `where`(컴포넌트 경로), `what`(metric 이름), `value`(숫자). 소스 = `mgpusim/runner/runner_report_trace.go`. 접미사 `-N`/` N`/`-S-D`/`-H` = per 칩렛 / 커널 / buffer / CU / hop / (src,dst)쌍 인덱스. 아래 설명은 한국어, metric·컴포넌트 이름은 코드 고유명사라 영어 유지.

### `where` 컴포넌트 경로
- `driver` (GPUDriver) — 시뮬 전역 metric.
- `GPU1.CommandProcessor` — 커맨드 프로세서 (커널 busy time).
- `GPU1.chiplet_N.SA_M.CU_K` — 칩렛 N · 셰이더어레이 M · 컴퓨트유닛 K.
- `...SA_M.LVCache_K` = L1 Vector 캐시(CU별). `...SA_M.LSCache` = L1 Scalar(SA별). `...SA_M.LICache` = L1 Instruction(SA별).
- `GPU1.chiplet_N.L2_M` — L2 캐시 뱅크 M. `GPU1.chiplet_N.DRAM_M` — DRAM 컨트롤러. `GPU1.chiplet_N.ChipRDMA` — 칩렛 RDMA 엔진.
- `GPU1.chiplet_all.{Caches,ChipRDMA,DRAM_all}` — 전 칩렛 집계. `chiplet-N` — 칩렛 N(WG 종료시간). `Chip-to-chip-traced` — `what`이 `"S-D"`인 src→dst RDMA 카운트. `Parameters` — 실행 플래그 덤프.

### 시간 / 커널 (driver, CommandProcessor)
- `simulation_time` = wall-clock 시뮬 소요시간(시간 단위). `simulation_time_sec` = 동일, 초.
- `kernel_time` = 타이밍 busy time 누적(초). `total_time` = 시뮬 종료 시각(engine). `kernel_time N` = N번 커널 시간. `kernel_time (force stop) N` = 강제종료 기준.
- `kernel-launch-time-N` / `kernel-terminate-time-N` = N번 커널 시작/종료 시각. `kernel-execution-time-N` = 종료−시작. `kernel-N-PC` = base PC.
- `last-kernel-launch-time` = 마지막 커널 launch 시각. `last-traced-time` = 마지막 trace 시각. `total-kernel-time` = 전 커널 실행시간 합.
- `last_WG_finish_time`(CU별) = 마지막 WG 종료 시각. `avg_last_WG_finish_time`(chiplet-N) = 칩렛 평균(load imbalance 지표).

### 명령어 / 워크그룹 (driver, CU)
- `total_inst_count` = 전체 실행 명령어 수. `inst_count`(CU별) = CU 명령어 수. `max-instruction` = `-max-inst` 값(0=무제한).
- `kernel-N-workgroup-count` = N번 커널 WG 수. `Chiplet-N-workgroup-count` = 칩렛 N에 스케줄된 WG 수(LASP 분배 확인용).
- `kernel-N-grid-dimension-{x,y,z}` = 차원별 WG 개수. `kernel-N-workgroup-size-{x,y,z}` = WG당 워크아이템 수. `kernel-N-workgroup-dimension-grid-size-{x,y,z}` = 전체 워크아이템 수(grid×wgsize).
- `total-memory-usage` = 전체 할당 메모리(MB). `memory-usage N` = N번 buffer 할당량.

### 캐시 per-cache (LVCache/LSCache/LICache/L2)
- `read-hit` = valid line 존재해 즉시 서비스. `read-mshr-hit` = line 없지만 같은 line에 in-flight miss 존재 → MSHR coalesce(secondary miss). `read-miss` = line·in-flight 둘 다 없음 → MSHR 신규 할당 + 다음 레벨 fetch.
- `write-hit` / `write-mshr-hit` / `write-miss` = 쓰기 동일 의미.
- `read-hit-rate` = (read-hit+read-mshr-hit)/전체 read. `write-hit-rate` = 동일(write). `hit-rate` = 전체 hit+mshr-hit / 전체 trans. `req_average_latency` = 요청 평균 latency(cycle).

### 캐시 집계 (`chiplet_all.Caches`)
- `total-transaction` = L1+L2 전체 trans. `total-l2-transaction` = L2만.
- `total-l1-{read,write}-{hit,miss,mshr-hit}` / `total-l2-...` = 레벨별 분류 합. `total-l1-hit`·`total-l1-miss`(및 l2) = 레벨 전체 hit/miss.
- `total-l2-hit-rate` / `total-l2-miss-rate` = L2 전역 비율. `l2-total-hit-rate` / `l2-total-miss-rate`(per chiplet) = 칩렛별 L2 비율.
- `total-l1v-avg-latency` / `total-l1s-avg-latency` / `total-l2-avg-latency` = 레벨별 캐시 평균 latency 평균.
- evict: `local-evict-count`/`remote-evict-count` = local/remote home 라인 eviction, `*-overwritten-count` = overwrite, `*-pte-*` = PTE 라인 대상.

### RDMA / CDMA (per-engine + `chiplet_all.ChipRDMA`)
- `outgoing_trans_count`/`incoming_trans_count` = 송신(이 칩렛→원격 요청)/수신 트랜잭션 수. `outgoing_trans_latency`/`incoming_trans_latency` = 평균 latency.
- `total_{incoming,outgoing}_trans_count` = 합. `total_{...}_trans_latency` = per-engine 평균의 합. `avg_{incoming,outgoing}_trans_latency` = count-가중 평균(실제 평균).
- `inter_chiplet_traffic: chiplet_S:DataAccessToChiplet:chiplet_D` = S→D data access 수. `...:PageAccessToChiplet:...` = PTW access 수.

### Hop (`chiplet_all.ChipRDMA`) — request hop만 측정
- `total-chip-rdma-num` = 전체 RDMA request 수. `number-hop-required-H` = 라우팅상 H hop(manhattan) 필요한 수. `number-hop-traced-H` = 실제 H hop 거친 수.
- `average-hop-traced` = traced hop 가중평균. `average-latency-hop-traced-H` = H hop request 평균 latency. `average-latency-hop-traced` = 전체 가중평균.
- `number-rdma-per-chiplet-N` = 칩렛 N발 RDMA. `number-rdma-per-pc-X` = PC X발 RDMA. `rdma-reqs-received-chiplet-N` = 칩렛 N 수신 RDMA. `Chip-to-chip-traced`의 `what="S-D"` = S→D RDMA 수.
- remote PTW(pte-caching off): `total-remote-ptw-num`, `number-ptw-hop-{required,traced}-H`, `average-latency-ptw-hop-traced-H`, `Chip-to-chip-ptw-traced`.

### L2 miss trace (`-report-l2-miss-trace`)
- `l2-miss-rdma-count` = L2 miss 유발 RDMA 수. `l2-miss-hop-required-H` = H hop 필요한 L2 miss 수.
- `l2-miss-c2c-S-D`(chiplet_S) = S→D L2 miss 트래픽. `hop-count-per-chiplet-S-H` = 칩렛 S의 H hop 가중 트래픽(count×hop).
- `total-rdma-count-per-chiplet-S` = 칩렛 S발 RDMA 합. `total-hop-count-per-chiplet-S` = S발 총 hop 합(네트워크 부하 지표).

### DRAM / NoC / Flush
- `total-trans-count`(`DRAM_all`) = 전체 DRAM 트랜잭션. `num-flits`(driver) = 전체 flit 수.
- `flush-<id>-elapsed-time` = flush 작업 소요시간. `total-flush-time` = flush 누적시간.

### Distribution (dynamic-cluster/-dist 플랫폼만) — 의미 상세는 [[reference_distribution_metrics]]
- `total-distribution-count`/`-hit-count` = 분배 횟수 / 분배된 라인 재사용. `total-evict-by-distribution-count`·`-overwrite-by-distribution-count` = 분배가 유발한 eviction/overwrite. `total-{evict,overwrite}-local-home-cacheline-count` = local home 라인 영향.
- `distribution-count-per-chiplet-N`·`-per-pc-X` = 칩렛/PC별 분배. `total-distribution-prefetch-count` = prefetch 분배. `total-distribution-bypassed-by-{streaming,lookup,hotness}-count` = 분배 스킵 사유별. `hotness-count-N` = hotness 값 N 라인 수.

## When to keep / discard a run

- Each `reports/<run-id>/manifest.json` is the authoritative provenance record for that run — `submitted_at`, every benchmark's full simulator command line, all `run.py` sweep options. The same manifest is also backlinked at `~/mgvm/slurm/<slurm-dir>/manifests/manifest_<run-id>.json` so the sweep can be located from either side. Prefer reading the manifest over inferring options from filenames or directory naming.
- For runs predating the manifest feature (older timestamped dirs without `manifest.json`), the timestamp+platform+size in the directory name is all the provenance you get — capture the Slurm `*.out` (under `~/mgvm/slurm/<slurm-dir>/outputs/`) before discarding such a run.
- `-small` runs (0.25× `max_inst`) should not be compared head-to-head with full runs; the suffix is in the directory name to prevent that.
- `-round-robin` runs forced `data-partitioning='kernel-wide'`; see `scripts/run.py` for the override logic.

## Disk usage

Memory and PTE traces are large (hundreds of MB per benchmark). Prefer running with `--memory-trace=False` / `--pte-trace=False` unless an analysis script explicitly needs them. Old sweep directories can be deleted wholesale — nothing else points into them.

## For Claude

- **Treat files here as read-only research artifacts.** Do not edit, regenerate, or "clean up" CSVs or traces — they are the experimental record. If a CSV looks malformed, fix the producing tracer in `util/tracing/`, not the file on disk.
- **Do not delete run directories without asking.** Even ones that look obsolete may be referenced in pending paper figures.
- **Do not commit anything from this directory.** It is gitignored for good reason (size + reproducibility from `scripts/run.py`).
- Path roots assume `~/mgvm/results/`. The directory lives next to the source tree, not inside it, and is not present in git worktrees by default.
- **Worktree Naming Convention** The name of worktree needs to be a process related thing. DO NOT USE RANDOMLY GENERATED NAMES.