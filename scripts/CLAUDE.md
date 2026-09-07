# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Purpose

The **execution** half of the Python tooling: compile all benchmarks and submit sweeps to Slurm. Post-hoc analysis lives in the sibling `../analyze/` directory.

## Files

- `compile.py` — runs `go build` in every `mgpusim/samples/<bench>/` directory, then every `mgpusim/cmd/<name>/` tool binary (currently just `footprint`, no PGO). Add new benchmarks/tools here.
- `run.py` — submits one Slurm job per benchmark in `params.benchmarks`. Generates an `sbatch` heredoc per job. **`--footprint`** submits `mgpusim/cmd/footprint` replay jobs instead of the timing-sim binary — a host-side address-stream replay (`mgpusim/footprint/`) that reproduces a benchmark's inter-chiplet shared-footprint without a timing simulation, so it isn't truncated by `-max-inst` the way a timing-run memory trace is. Only benchmarks with a registered `footprint.Replay` succeed (see `mgpusim/cmd/footprint -list`); all the timing/tracer flags are ignored in this mode. Output lands in `<result_dir>/footprint/` (see layout below), consumed by `analyze/cacheline/shared_footprint_capacity_cdf.py --footprint-dir=`. **Node selection via `--nodelist`**:
  - **Empty (default)** — no `#SBATCH --nodelist` directive. In practice slurm's `cons_tres` packs all jobs onto one node because each job only asks for `--cpus-per-task=1` (so honestly: don't rely on this to spread work).
  - **Single name** (`--nodelist=compasslab3`) — pins every submitted job to that node.
  - **Comma-separated list** (`--nodelist=compasslab1,compasslab2,compasslab3`) — Python-side round-robin: `run_sweep` cycles through the list and emits `#SBATCH --nodelist=<one node>` per sbatch. Each sbatch still uses `--nodes=1`, so there's no slurm-level conflict (the old "multi-node rejected at argparse" rule is gone; the conflict was specifically a *single* sbatch listing multiple nodes with `--nodes=1`).
  Two-pass: builds all `BenchmarkEntry`s (creates output dirs, composes the simulator command), writes a `manifest.json`, **then** submits. Each submission is done via `subprocess.run(..., capture_output=True)`: sbatch failures are surfaced with the job name + sbatch's stderr (not silently swallowed like the old `os.system` path — that swallowing is what made an earlier sweep look "submitted" while every sbatch had rejected the malformed multi-node `--nodelist`). Pass `--dry-run` to do everything except the `sbatch` calls — useful for verifying the manifest, command lines, and per-job node assignments before committing cluster time.
- `manifest.py` — defines `Manifest` and `BenchmarkEntry` dataclasses. `run.py` writes one manifest per (platform, run-id) into the run-root dir (`<config>/<timestamp>/manifest.json`, schema v2) and a backlink copy into the slurm config dir's `manifests/` subdir (`<slurm-dir>/manifests/manifest_<run-id>.json`). The `manifests/` subdir is created automatically if missing — it keeps the slurm config dir's top level clean (just `run.sh`, `outputs/`, `errors/`). The manifest captures `submitted_at`, `run_id`, all `run.py` args, full per-benchmark simulator command, and expected output paths — written **before** any `sbatch`, so the intent record exists even if submission fails. Subdir name is `SLURM_MANIFEST_SUBDIR = "manifests"` in `manifest.py`. Manifest `result_dir` field semantics (v2): points to the run-root `<config>/<timestamp>`, with subdirs (`reports/`, `memory-traces/`, ...) living under it.
- `params.py` — canonical per-benchmark config: `sched-partition`, `data-partitioning`, `data-strides`, `params` (CLI args), `max_inst`. The `benchmarks` list at the top controls what `run.py` actually submits.
- `params_small.py` — smaller-workload variant.

## Result directory layout

`run.py` writes to `~/mgvm/results/` using a `<config>/<timestamp>` 2-level layout — one folder per sweep config (platform + size + modifiers), and one timestamp dir per run under it, so re-runs of the same config accumulate side by side:
```
~/mgvm/results/
  <config>/                                    # e.g. mesh-4x4 / mesh-8x8-small / dynamic-clustering-4x4-oracle
    <timestamp>/                               # e.g. 20260519-120000
      manifest.json                            # written by run.py before sbatch
      reports/
        <bench>_<platform>_<wxh>.csv           # metrics
      memory-traces/                           # if --memory-trace
        <bench>_<platform>_<wxh>_memory_trace
      pte-traces/                              # if --pte-trace
      access-counts/                           # if --access-count
      pc-state-traces/                         # if --pc-state-trace
      dist-traces/                             # dynamic-cluster / dynamic-clustering only
      data-allocation-logs/                    # if --log-data-allocation
      footprint/                               # if --footprint (replaces reports/ etc. for that run)
        <bench>_<wxh>/shared_lines.csv         # line,reuse,nchip — shared_footprint_capacity_cdf.py input
        <bench>_<wxh>/summary.json
    <timestamp>/                               # another run of same config
      ...
<worktree-root>/slurm/<slurm-dir>/
  manifests/manifest_<run-id>.json             # backlink to the same manifest (run-id = <config>-<timestamp>)
  outputs-<suffix>/<bench>.out                 # suffix = xDim×yDim or cluster_size
  errors-<suffix>/<bench>.err
  footprint-outputs-<suffix>/<bench>.out       # --footprint mode's own out/err dirs
  footprint-errors-<suffix>/<bench>.err
  cpu-prof-<suffix>/<bench>_<platform>_<WxH>.prof   # if --profile
  mem-prof-<suffix>/<bench>_<platform>_<WxH>.mprof  # if --profile
```
`<config>` is built by `build_config_id` in `run.py`: `<platform>-<size>[-oracle][-small][-round-robin]`. Size is `xDim×yDim` for mesh-family platforms, `cluster_size` for `staticcluster`.

Analysis scripts in `../analyze/` use `_paths.py` helpers (`memory_trace_dir(run_id)`, `report_dir(run_id)`, etc.) where `run_id` is either `<config>/<timestamp>` or just `<timestamp>` (auto-resolved if unique). `extractor/extract.py` walks the same layout via `RESULTS_BASE.glob("*/*")`. If you add a new trace channel, both `run.py` (path generation) and the matching analysis/extractor consumer need to be updated together.

## Adding a benchmark to the sweep

1. Append the benchmark name to the `benchmarks` list in `params.py`.
2. Add a config dict in `benchmark_configs` with `sched-partition`, `data-partitioning`, `params`, `max_inst`. The `name` / `alias` fields let one benchmark binary run under multiple workload labels (see `gpt3_175B_gemm`, `llama3_70B_gemm`).
3. Add the benchmark to `compile.py` so it gets built.

## Conventions

- Python 3, no virtualenv expected — uses `os.system` + `subprocess` directly.
- Paths assume `~/mgvm` as the workspace root (`username = os.path.expanduser('~')`). Slurm scripts use `${username}/mgvm/...`.
- These scripts are research-grade — favor a working copy over a refactor unless the user asks.
- **Slurm submission needs explicit user approval** — `run.py` without `--dry-run`, or any direct `sbatch` call, must be confirmed before being executed by Claude.
