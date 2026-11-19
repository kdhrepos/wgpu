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

This is a multi-module Go workspace (wired by `go.work`, no single root `go.mod`) hosting the **Akita** simulator framework and the **MGPUSim** AMD-GCN3 simulator, extended to model a **generic `xDim × yDim` mesh wafer-scale GPU**. Each top-level directory is its own Go module that depends on its siblings via `replace ... => ../<sibling>` directives — never bump versions to mainline; edit sources in-tree.

Scope: only the general mesh wafer-scale GPU (plus an `ideal` NoC reference). Research ideas and other platform topologies live in `~/mgvm` and must not be ported here.

- `akita/` — architecture-agnostic discrete-event simulator framework. Defines the core abstractions: `Component`, `Port`, `Engine` (serial + parallel), `Event`, `Connection`, `Hook`, `Ticker`. Everything else depends on it.
- `mem/` — memory hierarchy: `cache/writeback` (L2), `dram/`, `idealmemcontroller/`, `vm/tlb`, `vm/mmu`, `device/` (page table + memory allocators), `trace/`, `writebuffer.go`.
- `noc/` — network-on-chip: flits, traffic counter, `networking/` (mesh + ideal chiplet networks, multi-flit switches, XY routing, PCIe).
- `protocol/` — memory/translation message types (`protocol.go`, `meshprotocol.go`).
- `util/` — shared utilities: `akitaext` (extensions), `tracing`, `pipelining`, `ca` (cache abstractions), `global` (process-wide singletons), `buffer`.
- `lmf/` — low-module / home-node finders (address → chiplet/bank routing).
- `dnn/` — supporting DNN library used by some benchmarks (not in `go.work`).
- `mgpusim/` — the GPU simulator itself:
  - `samples/` — benchmark main packages, each built into a standalone executable, each with a `mesh` wrapper script as a usage example.
  - `benchmarks/` — benchmark logic libraries that the `samples/` mains wrap.
  - `platform/` — `Builder`s for the simulated platforms (see "Platform types" below).
  - `builders/` — GPU/chiplet/shader-array builders used by `platform/`.
  - `runner/` — central CLI flag parser (`runner.go`) shared by all sample mains; builds the platform, attaches tracers, runs, and emits CSV metrics.
  - `driver/`, `kernels/`, `insts/`, `emu/`, `timing/` — driver API, kernel/grid builders, ISA decode, functional emulator, timing model (CU, wavefront, L1V cache/ROB, command processor).
  - `rdma/`, `pagemigrationcontroller/`, `coreprotocol/` — inter-chiplet RDMA engine, PMC, CP/driver protocol.
- `scripts/compile.py` — builds every `mgpusim/samples/<bench>`.

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

Known pre-existing build failures (stale code, not platform-related): `benchmarks/parboil/histogram` (missing `kernels.hsaco`), `benchmarks/dnn/{layers,minerva,xor}`, `benchmarks/parboil/tpacf`, `benchmarks/rodinia/{mergesort,srad}`, `benchmarks/shoc/{md,md5sum}`, `samples/{addtoarray,latencybench,memcopy}`.

### Tests
Tests use **Ginkgo v1 + Gomega**. Mocks are generated with `mockgen` via `//go:generate` directives in the test files.
```bash
# Per-module (run from inside that module's directory):
go test ./...

# Single Ginkgo spec by description:
go test ./<pkg> -ginkgo.focus="<spec text>"

# Regenerate mocks for a package:
cd <pkg> && go generate ./...
```
Linting/formatting follows standard `go vet ./...` and `gofmt`.

### Run a benchmark directly
After building a sample, invoke it with `runner` flags (see `mgpusim/runner/runner.go` for the full set). Minimal timing run (default = interleaved allocation + chiplet-aware scheduling):
```bash
mgpusim/samples/spmv/spmv \
  -dim=2097152 -sparsity=0.00001 \
  -timing -report-all -verify -no-progress-bar \
  -platform-type=mesh -xDim=4 -yDim=4
```

## Platform types

`-platform-type` selects a builder in `mgpusim/platform/`. The dispatch is in `runner.buildTimingPlatform` (`mgpusim/runner/runner.go`). Options:
- `mesh` (default) — `xDim × yDim` chiplet mesh, multi-flit switches, XY routing.
- `ideal` — same chiplets on an ideal (contention-free) chiplet network; reference upper bound.

Adding a new platform = add a `MakeXxxGPUPlatformBuilder` in `platform/`, add a case to that switch, and add any new flags it needs. Only do this for general (non-research) topologies.

The default grid is a wafer-scale `xDim × yDim` mesh of chiplets with per-chiplet memory fixed at 4 GB (`b.WithTotalMemSize(uint64(xDim*yDim) * 4 * mem.GB)` in the same file). Physical addresses are striped across chiplets at `numBanksPerChiplet × pageSize` granularity and across banks at cache-line granularity.

## Scheduling & memory allocation

Defaults: `-scheduling=chiplet-aware`, `-mem-allocator-type=interleaved` (sequential page allocation over the striped physical address map → chiplet-interleaved data). Other schedulers: `greedy`, `round-robin`, `old-chiplet-aware`.

LASP (Locality-Aware Scheduling and Partitioning, prior work) is kept as an **opt-in** baseline:
- `-scheduling=lasp` + `-sched-partition` (`Xdiv` / `Ydiv`, or comma-separated per kernel).
- `-mem-allocator-type=lasp` + `-use-lasp-mem-alloc` — LASP memory allocator.
- `-data-partitioning` — `kernel-wide` (default), `row-based`, `col-based`, `stride-aware`, comma-separated per buffer.
- `-data-strides` — comma-separated strides for `stride-aware` buffers.
- `-col-size-for-lasp` — column-size hint for LASP.

## L2 cache

`mem/cache/writeback/` is the only L2 implementation (write-back, per-bank, local-first priority, PTE caching via the MMU→L2 path enabled by `-use-pte-caching`, default on). L1V lives in `mgpusim/timing/caches/l1v`.

## Tracing / output

The runner exposes many `-trace-*` and `-report-*` flags (cache hit rate, TLB stats, DRAM latency, RDMA transactions, hop counts, etc.) — see the flag declarations at the top of `mgpusim/runner/runner.go`. CSV metrics are written to `<-metric-file-name>.csv`. Memory/access-count traces produce binary `.trace` files that are gitignored.

## Conventions

- Each `Component` owns a set of `Port`s, sends `Msg`s through `Connection`s, and processes them on `akita.Event` ticks driven by an `Engine`. Builder pattern (`MakeXxxBuilder().WithFoo(...).Build()`) is used everywhere — follow it when adding new components.
- All cross-module references go through the `replace` directives. If you add a new module under any of the top-level dirs, add `replace gitlab.com/akita/<name> => ../<name>` to **every** module that imports it, and add it to `go.work`.
- `samples/<bench>/main.go` should stay tiny: parse benchmark-specific flags, call `runner.Runner{}.ParseFlag().Init()`, instantiate the benchmark from `mgpusim/benchmarks/...`, `runner.AddBenchmark(b)`, `runner.Run()`. Put real logic in `benchmarks/`, not in the sample main.
