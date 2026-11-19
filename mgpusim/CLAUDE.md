# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Purpose

MGPUSim — an AMD GCN3 ISA multi-GPU simulator built on `akita`. Models functional emulation, cycle-level timing, and (in this fork) a generic `xDim × yDim` mesh wafer-scale chiplet GPU. LASP scheduling/allocation is available as an opt-in baseline.

## Top-level layout

- `runner/` — **central CLI** (`runner.go`). All `samples/` mains share its flag set, platform dispatch, tracer wiring, and CSV output. New global flags / tracers / platforms are registered here.
- `platform/` — `mesh` (default) and `ideal` builders, plus `emu.go`. Selected via `-platform-type`. Switch in `runner.buildTimingPlatform` (`runner/runner.go`). Builders compose pieces from `builders/`.
- `builders/` — finer-grained builders (`common.go`, `mesh.go`, `ideal.go`, `shaderarray.go`, `die.go`) used by `platform/`.
- `samples/` — benchmark `main` packages, one per directory, each with a `mesh` wrapper script. Each builds to `samples/<name>/<name>`. Compiled in bulk by `../scripts/compile.py`.
- `benchmarks/` — benchmark *logic* (kernel setup, host code) organized by source: `amdappsdk/`, `polybench/`, `rodinia/`, `shoc/`, `heteromark/`, `mafiaports/`, `pannotia/`, `parboil/`, `dnn/`, `matrix/`, `microbenchmarks/`, `mccl/`. `samples/<bench>/main.go` should be a thin shell that imports from here.
- `driver/` — host-side GPU driver: command queue, kernel launch, `Distributor` (multi-GPU work splitting), memory copy via the `memcopy.hsaco` kernel.
- `kernels/` — grid/workgroup abstractions; `gridbuilder.go` builds dispatch grids.
- `insts/` — GCN3 instruction encoding/decoding; `gcn3disassembler/` is a separate disassembler binary.
- `emu/` — pure functional emulator (no timing). Mirrors GCN3 ALU ops: `alu*.go`, `aluds.go`, `alu_flat.go`. Used for `-verify`.
- `timing/` — cycle-level model:
  - `cu/` — compute unit pipeline: fetch/decode/issue, SIMD, scalar, LDS, vector/scalar memory units, scheduler, wavefront pool.
  - `caches/` — L1V cache (`l1v/`) and ROB.
  - `cp/` — command processor + DMA.
  - `wavefront/` — wavefront state.
- `rdma/` — per-chiplet RDMA engine bridging L2 ↔ mesh network. `coreprotocol/`, `pagemigrationcontroller/` — CP/driver messages, PMC.
- `bitops/` — small bitfield helpers used across decoders.
- `docker/dev/` — dev container definition.

## Build / test

```bash
# Build all sample binaries:
python3 ../scripts/compile.py

# Build one sample:
cd samples/<bench> && go build

# Top-level Ginkgo suite (mocks are generated from go:generate directives in gcn_test.go):
go test ./...
go generate ./...     # regen mocks after interface changes
```

## Running

Every sample exposes the runner flag set on top of any benchmark-specific flags. Typical invocation (defaults: interleaved allocation, chiplet-aware scheduling):
```bash
samples/spmv/spmv \
  -dim=2097152 -sparsity=0.00001 \
  -timing -report-all -verify -no-progress-bar \
  -platform-type=mesh -xDim=4 -yDim=4
```
Opt-in LASP: add `-mem-allocator-type=lasp -use-lasp-mem-alloc -scheduling=lasp -sched-partition=Xdiv`. Each `samples/<bench>/mesh` script is a ready-made example.

## Key flag groups (in `runner/runner.go`)

- **Platform**: `-platform-type` (`mesh` default, `ideal`), `-xDim`, `-yDim`, `-num-req-per-cycle`, `-switch-latency`.
- **Scheduling**: `-scheduling` (chiplet-aware default, greedy, round-robin, old-chiplet-aware, lasp), `-sched-partition` (Xdiv/Ydiv, comma-sep per kernel), `-data-partitioning` (kernel-wide, row-based, col-based, stride-aware), `-data-strides`, `-col-size-for-lasp`.
- **Memory**: `-mem-allocator-type` (interleaved default, lasp), `-use-lasp-mem-alloc`, `-log2-page-size`, `-use-pte-caching`.
- **Reports**: `-report-all`, `-report-essential`, and many `-report-<thing>` flags emitting CSV columns. Output to `<-metric-file-name>.csv`.
- **Tracers**: `-trace-mem`, `-trace-tlb`, `-trace-vis`, `-trace-access-count-per-addrs`, `-trace-memory-access`, `-trace-pte-access`, `-trace-l2sql`. Output is `*.trace` (gitignored) or MySQL.
- **Sim control**: `-timing` (else functional), `-parallel` (parallel engine), `-max-inst`, `-time-limit` (hours), `-verify`, `-debug-isa`, `-refresh`.

## Adding a platform

Only general (non-research) topologies belong in this repo.
1. New file under `platform/` defining `Make<Name>GPUPlatformBuilder()` and `Builder.Build()` returning `(akita.Engine, *driver.Driver)`. Embed `CommonPlatformBuilder` from `platform/common.go`.
2. Add a `case "<name>":` in `runner.buildTimingPlatform` (`runner/runner.go`).

## Adding a benchmark sample

`samples/<bench>/main.go` should be ~20 lines: parse benchmark-specific flags, `runner := new(runner.Runner).ParseFlag().Init()`, instantiate the benchmark from `benchmarks/...`, `runner.AddBenchmark(b)`, `runner.Run()`. Put real logic in `benchmarks/`. `../scripts/compile.py` auto-discovers every `package main` under `samples/`.
