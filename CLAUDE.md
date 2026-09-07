# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Rules for Outputs [Most Important for Token Saving, Caveman Mode]
- Speak in a smart, concise caveman style. 
- Eliminate all conversational filler, preambles, and polite greetings (e.g., No "Sure", "I can help with that").
- Maximize brevity. Use fragments, bullet points, and short sentences.
- Use symbols for relationships where appropriate (-> for causality, = for definition, vs for comparison).

## Coding Rules
- Do not explain code unless explicitly asked. Just show the diff or the code block.
- Keep comments inside the code to an absolute minimum.
- Prioritize performance and memory efficiency.

## Rule for Worktrees
- DO NOT MAKE worktrees inside of each of modules.
- Worktress should be made right under "/mgvm/.claude/worktrees" directory.
- The name of worktree should be related to the context or the promt.
- Also, do not include randomic words to the name of worktrees.
- **NEVER create a new worktree without explicit user instruction.** If the user has selected a specific worktree, make all edits directly in that worktree. Creating a worktree on your own initiative is strictly forbidden.

## Repository layout

This is a multi-module Go workspace (no single root `go.mod`) hosting the **Akita** simulator framework and the **MGPUSim** AMD-GCN3 multi-GPU simulator, with research extensions for wafer-scale GPUs and LASP scheduling. Each top-level directory is its own Go module that depends on its siblings via `replace ... => ../<sibling>` directives — never bump versions to mainline; edit sources in-tree.

- `akita/` — architecture-agnostic discrete-event simulator framework. Defines the core abstractions: `Component`, `Port`, `Engine` (serial + parallel), `Event`, `Connection`, `Hook`, `Ticker`. Everything else depends on it.
- `mem/` — memory hierarchy: `cache/` (many writeback variants — see "Cache variants" below), `dram/`, `idealmemcontroller/`, `vm/tlb`, `vm/mmu`, `trace/`, `writebuffer.go`.
- `noc/` — network-on-chip: flits, traffic counter, `networking/` sub-module.
- `protocol/` — coherence/distribution protocol messages (`meshprotocol`, `distprotocol`, `distprotocol2`, `rcrprotocol`, `mshrcoalesceprotocol`).
- `util/` — shared utilities: `akitaext` (extensions), `tracing`, `pipelining`, `ca` (cache abstractions), `global` (process-wide singletons), `buffer`.
- `lmf/`, `dnn/` — supporting libraries (linear algebra / DNN ops) used by some benchmarks.
- `mgpusim/` — the GPU simulator itself:
  - `samples/` — 61 benchmark main packages, each built into a standalone executable (`spmv`, `gemm`, `pagerank`, `gups`, `mis`, `mm2`/`mm3`, `kmeans`, `lenet`, `vgg16`, AMDAPPSDK + Polybench + Rodinia + SHOC workloads, plus LLM-shaped configs `gpt3_175B_gemm`, `llama3_70B_gemm`, etc.).
  - `benchmarks/` — benchmark logic libraries that the `samples/` mains wrap.
  - `platform/` — `Builder`s for each simulated platform topology (see "Platform types" below).
  - `runner/` — central CLI flag parser (`runner.go`) shared by all sample mains; builds the platform, attaches tracers, runs, and emits CSV metrics.
  - `driver/`, `kernels/`, `insts/`, `emu/`, `timing/` — driver API, kernel/grid builders, ISA decode, functional emulator, timing model (CU, wavefront, caches, command processor).
  - `rdma/`, `pagemigrationcontroller/`, `coreprotocol/` — multi-GPU support.
- `scripts/` — **execution** Python tooling: `compile.py`, `run.py` (slurm sweep submitter, writes `manifest.json` before sbatch), `manifest.py`, `params.py` / `params_small.py` (sweep configs).
- `analyze/` — **analysis** Python tooling: post-hoc scripts that consume `~/mgvm/results/.../*.csv` and `*.trace` files into figures. Split from `scripts/` so analysis cannot accidentally trigger slurm jobs.
- `extractor/` — **simple value extractor**. Pulls a configurable list of `what` metric names out of every benchmark CSV in a run dir and emits a combined long/wide table. Edit `extractor/metrics.txt` to pick metrics. Separate from `analyze/` — does not plot or derive anything.

## Common commands

### Build all benchmark samples
```bash
python3 scripts/compile.py        # runs `go build` in every mgpusim/samples/<bench> dir
```
Single benchmark:
```bash
cd mgpusim/samples/spmv && go build
```
The build produces an executable named after the directory (e.g. `samples/spmv/spmv`).

### Tests
Tests use **Ginkgo v1 + Gomega**. Mocks are generated with `mockgen` via `//go:generate` directives in the test files.
```bash
# Per-module (run from inside that module's directory):
go test ./...

# Top-level mgpusim suite:
cd mgpusim && go test ./...

# Single Ginkgo spec by description:
go test ./<pkg> -ginkgo.focus="<spec text>"

# Regenerate mocks for a package:
cd <pkg> && go generate ./...
```
Linting/formatting follows standard `go vet ./...` and `gofmt`.

### Run a benchmark directly
After building a sample, invoke it with `runner` flags (see `mgpusim/runner/runner.go` for the full set). Minimal timing run:
```bash
mgpusim/samples/spmv/spmv \
  -dim=2097152 -sparsity=0.00001 \
  -timing -report-all -verify -no-progress-bar \
  -platform-type=mesh -xDim=4 -yDim=4 \
  -mem-allocator-type=lasp -use-lasp-mem-alloc \
  -scheduling=lasp -sched-partition=Xdiv
```
`.vscode/launch.json` has many ready-made debug configurations covering the common (benchmark × platform × size) combinations — useful as a reference for valid flag combinations.

### Run the full benchmark sweep (Slurm)
`scripts/run.py` submits one Slurm job per benchmark via `sbatch` heredocs. Output goes to `~/mgvm/results/<config>/<timestamp>/{reports,memory-traces,pte-traces,...}/` — per-run 2-level layout, where `<config>` is `<platform>-<size>[-oracle][-small][-round-robin]`. See `scripts/CLAUDE.md` for details. Benchmark list and per-benchmark args live in `scripts/params.py` (or `params_small.py`).
```bash
python3 scripts/run.py --platform mesh --xDim 4 --yDim 4
python3 scripts/run.py --platform mesh --mix          # iterate over mesh, ideal, direct
python3 scripts/run.py --platform mesh --small        # 0.25x max_inst
```
Node selection via `--nodelist` CLI flag. **Default is empty** — when empty, `run.py` emits no `--nodelist` directive at all, so slurm's scheduler picks freely from the `batch` partition (`compasslab[1-4]`) and load-balances across them. Pass `--nodelist=compasslab3` (or any single node name) to pin one node when result-variance stability matters. Multi-node lists like `--nodelist=a,b` are **rejected at argparse time** — slurm's `--nodelist` semantics is "the job MUST include all these nodes", which with `--nodes=1` conflicts and produces `sbatch: error: invalid number of nodes (-N N-1)`.

## Platform types

`-platform-type` selects a builder in `mgpusim/platform/`. The dispatch is in `runner.buildTimingPlatform` (`mgpusim/runner/runner.go:822`). Current options: `ideal`, `direct`, `datadirect`, `ptdirect`, `pt-ideal` (PTW on ideal NoC, data on mesh), `data-ideal` (data on ideal NoC, PTW on mesh — the inverse of `pt-ideal`), `mesh`, `meshwt` (write-through), `nearfetch`, `nearfetch2`, `static-cluster`, `dynamic-cluster`, `dynamic-cluster-rep`, `dynamic-dist`, `dynamic-clustering`, `daware`, `infllc`. Adding a new platform = add a `MakeXxxGPUPlatformBuilder` in `platform/`, add a case to that switch, and add any new flags it needs.

The default grid is a wafer-scale `xDim × yDim` mesh of chiplets with per-chiplet memory fixed at 4 GB (`b.WithTotalMemSize(uint64(xDim*yDim) * 4 * mem.GB)` in the same file).

## Scheduling & LASP

`-scheduling` picks the workgroup scheduler: `chiplet-aware` (default), `greedy`, `round-robin`, `lasp`. LASP is the focus of the current research and is wired through several extra flags consumed by both the runner and the driver:

- `-mem-allocator-type=lasp` + `-use-lasp-mem-alloc` — switch the memory allocator (otherwise `interleaved`).
- `-sched-partition` — `Xdiv` / `Ydiv` (or comma-separated per kernel: `"Xdiv, Xdiv, Ydiv"`).
- `-data-partitioning` — `kernel-wide` (interleaved), `row-based`, `col-based`, `stride-aware`, comma-separated per buffer.
- `-data-strides` — comma-separated strides for `stride-aware` buffers.
- `-col-size-for-lasp` — column-size hint for LASP.

`scripts/params.py` is the canonical reference for which partition/data-layout combination each benchmark expects.

## Cache variants

`mem/cache/` contains parallel implementations of the writeback cache, one per platform family: `writeback/`, `writebackd/`, `writeback_dynamic_clustering/`, `writeback-dynamic-clustering-rep/`, `writeback-dynamic-dist/`, `writebackrcr/`, `writebackrcr2/`, `writeback_static_clustering/`, plus `writethrough/`. The `mgpusim/go.mod` has a `replace gitlab.com/akita/mem/cache/writebackrcr => ../mem/cache/writebackrcr` line — when adding a new cache module that lives under `mem/cache/<x>/` with its own `go.mod`, the consuming module's `go.mod` needs a matching `replace` directive.

## Tracing / output

The runner exposes many `-trace-*` and `-report-*` flags (cache hit rate, TLB stats, DRAM latency, RDMA transactions, etc.) — see the flag declarations at the top of `mgpusim/runner/runner.go`. CSV metrics are written to `<-metric-file-name>.csv`. Memory/PTE/access-count/distribution traces produce binary `.trace` files that are gitignored; the `analyze/*.py` analysis tools (e.g. `access_count_per_address.py`, `metrics_per_cacheline.py`, `pc_metrics_*.py`) consume them.

## Conventions

- Each `Component` owns a set of `Port`s, sends `Msg`s through `Connection`s, and processes them on `akita.Event` ticks driven by an `Engine`. Builder pattern (`MakeXxxBuilder().WithFoo(...).Build()`) is used everywhere — follow it when adding new components.
- All cross-module references go through the `replace` directives. If you add a new module under any of the top-level dirs, add `replace gitlab.com/akita/<name> => ../<name>` to **every** module that imports it (the deeper modules in `mem/cache/` and `noc/networking/` each have their own `go.mod`).
- `samples/<bench>/main.go` should stay tiny: parse benchmark-specific flags, call `runner.Runner{}.ParseFlag().Init()`, instantiate the benchmark from `mgpusim/benchmarks/...`, `runner.AddBenchmark(b)`, `runner.Run()`. Put real logic in `benchmarks/`, not in the sample main.
