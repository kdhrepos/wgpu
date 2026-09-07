# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Purpose

The **analysis** half of the Python tooling. Consumes the CSV / `.trace` artifacts that `scripts/run.py` writes under `~/mgvm/results/` and produces tables and figures. Execution (compile + slurm submission) lives in the sibling `../scripts/` directory; this directory is intentionally separated so analysis scripts cannot accidentally trigger slurm jobs.

## Layout (domain folders)

Code is split into one folder per analysis **domain**. Shared library code lives in `common/`; every other folder holds runnable entry scripts (each with its own `if __name__ == '__main__'`). Invoke scripts through the `run.sh` wrapper (see Running) so cross-domain imports resolve.

- `common/` — shared library, imported everywhere: `_paths.py` (run-id → result-tree path resolution + `motivation_out_dir`/`address_stats_dir`/`local_remote_ratio_xlsx` helpers), `formatter.py` (chiplet/cluster maps + DataFrame helpers `get_rdma_df`, `get_aggregated_df`, `num_chiplets`, `ns`, `chip_cols`), `plot_style.py` (paper-grade matplotlib style — Okabe-Ito palette, `apply_paper_style`, `color_for`, `save_fig`, `SIZE_*` constants), `motivation_common.py` (long-form report loader, `traffic_matrix`, `manhattan_matrix`, `trace_chunks`, `file_suffix_for_config`), `pc_hotness.py` (reproduces `rdma/pctable.go` tier classification from a memory trace — chunked `PCAccum`, `classify_signals`, `hot_pcs`; shared by the two PC-hotness motivation figures so their "hot" definition stays identical).
- `access-count/` — address-level access histograms + read/write & sharing ratios (`access_count_per_*`, `access_count_ratio*`, `access_rw_*`, `access_skew_cdf` 로렌츠 곡선).
- `address/` — per-address metrics (+ RDMA) and their plotters (`addr_metrics_*`, `addr_rdma_metrics*`, `metrics_per_addr*`, `address_access_*`, `sharing_degree_hist` motivation 공유도 분포).
- `pc/` — core per-PC metrics (`pc_metrics_*`, `pc_rdma_metrics*`, `pc_shared_ratio`, `duplicated_pc_count`, `same_set_ratio_per_pc`).
- `pc-stats/` — per-PC statistical family: per-owner / L2-miss / PTE / RDMA stats + PC sharing-distribution (`pc_stats_*`, `pc_*_shared_ratio`, `plot_pc_*`, `hot_pc_metrics`, `pc_to_buffer`, `pc_predicts_characteristics` η² 분산 분해, `pc_temporal_stability` 에폭 자기상관, `pc_hotness_signature` motivation-1: PC별 정렬 hotness 곡선 + reuse-sharing 산포 격자, `pc_line_scatter` motivation-1 라인해상도: 점=캐시라인·색=PC·공분산 타원으로 PC별 영역 분리(논문용 png+pdf)).
- `cacheline/` — cacheline-level views: reuse interval, rw-sharing, set-conflict (3C), chiplet/owner affinity, MSHR cluster coalescing (`cacheline_metrics*`, `metrics_per_cacheline`, `remote_cachelines`, `chiplet_affinity_*`, `mshr_cluster_coalesce`, `set_conflict_miss_3c`, `local_remote_ratio`, `remote_ratio_plot` motivation 막대, `intra_cluster_cacheline_sharing`, `pc_cacheline_reuse_interval`, `oracle_placement_headroom` 완벽 배치 한계선, `premature_hot_eviction` motivation-2: hot 라인(OR 규칙)이 evict 후 access-count window 안에 재요청되는 비율 = 조기축출 낭비, `eviction_churn_by_hotness` per-line churn=(evict+overwrite)/access 를 hotness tier별+eviction lift 로 "hotness 무관 축출" 검정, `hot_pc_line_hit_ratio` hot-PC 라인의 hit/miss 비율(벤치축·라인평균)).
- `top-n/` — top-X hottest / most-shared selection + eviction share (`top_*`, `hot_data_eviction_share`).
- `memory-trace/` — raw memory-trace consumers: access pattern, trace expansion, footprint, inter-chiplet interval, data-id annotation (`memory_*`, `inter_chiplet_access_interval`, `annotate_trace_with_data_id`).
- `data-structure/` — per-buffer (data-id) metrics & strides (`data_structure*`, `data_structures_*`, `data_id_owner_distribution`, `local_remote_strides`).
- `pte/` — PTE/TLB metrics + eviction bucket (`pte_metrics_*`, `pte_eviction_bucket`).
- `rdma/` — standalone RDMA value extractor (`extract_rdma`).
- `network/` — wafer-scale mesh link-load + per-chiplet received-request views (`plot_mesh_link_load*`, `plot_chiplet_received`, motivation: `bottleneck_breakdown` 실행 시간 분해, `hop_distribution` hop CDF, `distance_histogram` 거리 분포).
- `sketch/` — hardware sketch-structure emulation for sharing detection: counting Bloom filter + HyperLogLog (`cbf_*`, `hll_*`, `cbf_params`).
- `figure-generator/*.ipynb` — ad-hoc Jupyter plot exploration. Kept **off** the `run.sh` import path (holds a divergent notebook-local `metrics_per_addr_plotter.py`).
- `_out/` — gitignored dump for figures/tables/CSV outputs, kept out of the code tree. `deprecated/` — old/superseded script copies, off the import path.

## Imports & the `run.sh` wrapper

Scripts live in domain folders but import each other **flat by module name** — shared helpers (`_paths`, `formatter`), domain plotters (`*_plotter`), and the sweep config `params` (canonical in `../scripts/params.py`). `analyze/run.sh` makes this work with **zero per-file path edits**: it puts every domain folder (+ `../scripts`) on `PYTHONPATH`, so any module resolves regardless of which folder the entry script sits in. Module names are globally unique, so this runtime flattening is collision-free. `deprecated/`, `_out/`, `__pycache__/`, `.claude/`, and `figure-generator/` are excluded from the path. Adding a new domain folder needs no wrapper edit — `run.sh` globs all folders.

Do **not** copy `params.py` into this directory — the sweep config is canonical in `../scripts/params.py` and must not diverge.

## Manifest

Each sweep's `~/mgvm/results/reports/<run-id>/manifest.json` is the authoritative record of what produced the CSVs in that directory: `submitted_at`, `run_id`, every benchmark's full simulator command, expected output paths. Prefer reading the manifest to inferring options from filenames — when filename parsing and manifest disagree, the manifest is correct. `scripts/manifest.py` defines the schema; loading is `Manifest.load("<path>/manifest.json")`.

## Running

Use the wrapper from the repo root, passing `<domain>/<script>.py` and the result-tree run id:
```bash
analyze/run.sh pc/pc_rdma_metrics_fig.py <run-id-or-path>
analyze/run.sh cacheline/cacheline_metrics.py mesh-6x6/latest
```
The wrapper sets `PYTHONPATH` (all domain folders + `../scripts`) then execs `python3`. Running a script directly (`python3 analyze/pc/foo.py`) only works if it imports nothing cross-domain. Most scripts hard-code `~/mgvm/results/...` paths via `os.path.expanduser('~')`. No virtualenv expected; depends on `pandas`, `numpy`, `matplotlib`, `seaborn` (+ `mmh3` for `sketch/`).

### Run-id resolution (`_paths.run_root`)

Every helper in `common/_paths.py` (`memory_trace_dir`, `access_count_dir`, …) routes the
`--date`/`--trace-id` value through `run_root()`, so you almost never have to copy a
timestamp by hand. Accepted forms:

| input | resolves to |
|-------|-------------|
| `latest` (or empty) | newest run across **all** configs (by mtime) |
| `mesh-6x6` (a config name) | newest run under that config |
| `mesh-6x6/latest`, `latest:mesh-6x6` | same |
| `mesh-6x6/20260601-235841` | that exact run |
| `20260601-235841` | auto-resolved under any config when unambiguous |
| `20260601` (substring) | fuzzy match; ambiguous → error lists candidates |

List what's available with `python3 analyze/common/_paths.py` (all configs) or
`python3 analyze/common/_paths.py mesh-6x6` (one config), newest first, `LATEST` flagged.

## Conventions

- Research-grade code: favor adding a new analysis script over heavily refactoring an existing one; the older scripts are often the published-figure source. New scripts go in the matching domain folder (add a new folder for a new domain — no wrapper change needed).
- Output location is **per-script and inconsistent** — most write to the *current working directory* (`./{date}/...`), a few to the result tree (`run_root(date)/...`), a few hard-coded (`~/mgvm/results/figs`, `~/mgvm/scripts/pc-stats`), and the plotters next to their input CSV. **No script writes to `_out/`** (it currently just holds relocated old dumps). So results land wherever you launch `run.sh` from. Outputs are gitignored (`*.png` repo-wide; `_out/`/`__pycache__/` via `analyze/.gitignore`) — never commit them. Full catalog + folder format: see `STRUCTURE.md`.
- These scripts are **read-only against the result tree**. Never edit, regenerate, or "fix" CSVs from here — if a column looks wrong, fix the tracer in `../util/tracing/` and re-run the sweep.
