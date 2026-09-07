# analyze/ 구조와 출력 규칙

분석 스크립트를 **분석 주제(도메인)별 폴더**로 나눠 둔 구조와, 각 스크립트가 결과를 **어디에 쓰는지**를 정리한 문서. (Claude용 짧은 지침은 `CLAUDE.md`, 이건 사람이 읽는 설명.)

## 1. 폴더 형식

`common/` 한 곳이 공유 라이브러리고, 나머지는 전부 도메인 폴더(실행 가능한 스크립트 모음)다. 각 스크립트는 자기 안에 `if __name__ == '__main__'` 진입점을 가진다.

| 폴더 | 들어있는 것 | 파일 수 |
|------|-------------|:---:|
| `common/` | 모든 스크립트가 가져다 쓰는 공유 코드. `_paths.py`(실행 id → 결과 폴더 경로 해석 + `motivation_out_dir`/`address_stats_dir`/`local_remote_ratio_xlsx`), `formatter.py`(칩렛/클러스터 맵, 데이터프레임 헬퍼), `plot_style.py`(논문급 matplotlib 스타일 — Okabe-Ito 팔레트, `apply_paper_style`, `color_for`, `save_fig`, `SIZE_*`), `motivation_common.py`(리포트 long-form 로더, 칩렛 간 트래픽 행렬, Manhattan 거리, 트레이스 chunk 읽기), `pc_hotness.py`(memory trace 로 `rdma/pctable.go` tier 분류 재현 — 청크 `PCAccum`, `classify_signals`, `hot_pcs`; 두 PC-hotness motivation 그림이 hot 정의를 공유) | 5 |
| `access-count/` | 주소별 접근 횟수 분포, 읽기/쓰기·공유 비율, motivation 로렌츠 곡선(`access_skew_cdf`) | 7 |
| `address/` | 주소(캐시라인) 단위 지표와 원격(RDMA) 지표, 그 그림 코드, motivation 공유도 분포(`sharing_degree_hist`) | 9 |
| `pc/` | 명령어 위치(PC) 단위 핵심 지표 | 7 |
| `pc-stats/` | PC별 통계 모음: home(소유 칩렛)별·L2 미스별·PTE별·RDMA별 통계, PC별 공유 분포, motivation η² 분산 분해(`pc_predicts_characteristics`)·시간 안정성(`pc_temporal_stability`)·PC hotness 서명(`pc_hotness_signature`: 정렬 곡선 + reuse-sharing 산포 격자)·라인해상도 산포(`pc_line_scatter`: 점=캐시라인, 색=PC, 공분산 타원) | 23 |
| `cacheline/` | 캐시라인 단위: 재사용 간격, 읽기/쓰기 공유, 충돌 미스, 칩렛/소유자 친화도, MSHR 묶음, motivation 원격 비율(`remote_ratio_plot`)·오라클 한계선(`oracle_placement_headroom`)·hot 조기축출(`premature_hot_eviction`)·hotness별 축출 churn(`eviction_churn_by_hotness`)·hot-PC 라인 hit/miss(`hot_pc_line_hit_ratio`) | 18 |
| `top-n/` | 가장 뜨겁거나 가장 많이 공유되는 상위 일부 선별, 축출 비중 | 12 |
| `memory-trace/` | 원본 메모리 트레이스 소비: 접근 패턴, 트레이스 펼치기, 메모리 사용량, 칩렛 간 접근 간격 | 8 |
| `data-structure/` | 버퍼(data_id) 단위 지표와 스트라이드 | 5 |
| `pte/` | 페이지테이블/TLB 지표, 축출 버킷 | 3 |
| `rdma/` | 원격(RDMA) 값 추출기 | 1 |
| `network/` | 웨이퍼 규모 메시의 선로별 부하, 칩렛별 수신 요청 그림, motivation 실행 시간 분해(`bottleneck_breakdown`)·hop CDF(`hop_distribution`)·거리 분포(`distance_histogram`) | 6 |
| `sketch/` | 공유 탐지용 하드웨어 스케치 구조 흉내(카운팅 블룸 필터, HyperLogLog) | 5 |
| `figure-generator/` | 임시 탐색용 주피터 노트북(`.ipynb`). **실행/임포트 경로에서 제외**(자체 변형본 `metrics_per_addr_plotter.py`를 들고 있어서) | 노트북 33 |
| `_out/` | 그림·표 출력물을 모아두는 곳. **git이 추적하지 않음**(`.gitignore`) | (출력) |
| `deprecated/` | 옛/대체된 스크립트 보관. 실행 경로에서 제외 | (보관) |

도메인 스크립트 합계: 약 92개.

## 2. 실행 / 임포트 규칙

스크립트가 폴더로 흩어져 있어도 서로(공유 모듈 `_paths`/`formatter`, 같은 도메인의 그림 코드, `../scripts/params.py`)를 **그냥 모듈 이름으로** 불러온다. 이게 되도록 `run.sh`가 **모든 도메인 폴더 + `../scripts`를 임포트 경로(`PYTHONPATH`)에 한꺼번에 올려준다.** 모듈 이름이 서로 안 겹치므로 이렇게 펼쳐도 충돌이 없다.

```bash
# 항상 저장소 최상단에서 run.sh로 실행. <도메인>/<스크립트>.py 와 실행 id 를 넘김.
analyze/run.sh pc/pc_rdma_metrics_fig.py mesh-6x6/latest
analyze/run.sh cacheline/cacheline_metrics.py mesh-6x6/latest
```

- `python3 analyze/pc/foo.py` 처럼 **직접 실행**하면, 다른 도메인을 임포트하지 않는 스크립트만 동작한다(공유 모듈·params 못 찾음). 그래서 `run.sh`를 쓴다.
- `deprecated/`, `_out/`, `__pycache__/`, `.claude/`, `figure-generator/` 는 임포트 경로에서 빠진다.
- 새 도메인 폴더를 추가해도 `run.sh`는 폴더를 통째로 훑으므로 수정할 필요 없다.

## 3. 출력 규칙 (지금은 제각각 — 주의)

결과 파일이 어디 생기는지는 스크립트마다 다르다. **지배적인 방식은 "실행한 위치(현재 디렉토리) 기준"** 이라, 어디서 `run.sh`를 부르냐가 출력 위치를 결정한다.

| 방식 | 실제 위치 | 예시 스크립트 |
|------|-----------|---------------|
| **현재 디렉토리 기준** `result_dir = f"./{date}"`, `"./cacheline-metrics"` 등 | `run.sh`를 저장소 최상단에서 돌리면 → `~/mgvm/{date}/...` (지저분) | `pc-stats/` 대부분, `top-n/` 대부분, `memory-trace/memory_footprint.py`, `cacheline/local_remote_ratio.py`, `pte/pte_eviction_bucket.py` |
| **결과 트리 기준** `run_root(date)/...` | `~/mgvm/results/<config>/<run>/...` (입력 트레이스 옆) | `cacheline/metrics_per_cacheline.py`, `access-count/access_count_per_time.py` |
| **경로 박아둠** | `~/mgvm/results/figs/...` 또는 `~/mgvm/scripts/pc-stats/` | `access-count/access_count_per_address.py`, `pc-stats/pc_stats_fig.py`, `pc-stats/pc_stats.py`, `pc-stats/pc_stats_per_home.py` |
| **입력 파일 옆** `os.path.dirname(csv)/plots_*` | 읽어들인 표가 있는 폴더 하위 | `pc-stats/plot_pc_sharing_chiplet.py`, `pc-stats/plot_pc_stats_l2miss_per_owner.py` |
| **`_out/` 로 쓰는 스크립트** | — | **없음.** `_out/`은 지금은 예전 출력 덤프를 옮겨둔 보관소일 뿐, 코드가 직접 쓰지는 않는다 |

주의할 점:
- **어떤 스크립트도 자기 파일 위치(`__file__`) 기준으로 출력하지 않는다.** 그래서 스크립트를 도메인 폴더로 옮긴 것 자체는 출력 위치를 바꾸지 않았다. 다만 "현재 디렉토리 기준" 출력은 **`run.sh`를 부른 위치**에 생기므로, 예전처럼 스크립트 폴더로 들어가서 돌리던 때와 달리 지금은 저장소 최상단에 흩어질 수 있다.
- 즉 결과가 한곳에 안 모이고 실행 위치에 따라 들쭉날쭉하다. (통일하려면 `common/`에 출력 경로 헬퍼를 하나 만들어 각 스크립트의 `result_dir = ...` 한 줄을 그걸로 바꾸면, 어디서 돌리든 `_out/` 한곳에 모이게 할 수 있다 — 아직 안 함.)

## 4. 새 스크립트 추가 규칙

- 알맞은 도메인 폴더에 넣는다. 새 주제면 폴더를 새로 만든다(`run.sh` 수정 불필요).
- 기존에 논문 그림을 뽑던 스크립트는 갈아엎지 말고 **새 파일로** 추가한다(연구용 원본 보존).
- `../scripts/params.py`(스윕 설정)는 여기로 복사하지 말 것 — 원본은 `../scripts/`에만 둔다.
- 결과 트리의 CSV/트레이스는 **읽기 전용**으로만 다룬다. 값이 이상하면 여기서 고치지 말고 `../util/tracing/`의 기록기를 고쳐 다시 돌린다.
