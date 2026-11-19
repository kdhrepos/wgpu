# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Purpose

Shared utilities used across the workspace. Two things stand out:

- `tracing/` — the **tracer registry** that the runner wires up based on `-trace-*` / `-report-*` flags. Adding a new metric or trace channel almost always means adding a file here.
- `global/` — **process-wide singletons** (the one place mutable global state is allowed). The rest of the workspace passes state through builders/constructors; `global` exists for cross-cutting concerns that would otherwise thread through every layer.

## Layout

- `tracing/` — one file per tracer (see "Adding a tracer" below). `tracer.go` defines the interface; `task.go` defines the `Task` (start/end time, parent/child). `api.go` provides `StartTask`/`EndTask`/`AddMilestone` used by components. Backends include `jsontracer.go`, `mysqltracer.go`, `mongodbtracer.go`.
- `global/global.go` — `Global` singleton (verbose flag, LASP partitioning state, data-allocation log, time limit, etc.). Accessed via `global.GetInstance()` (sync.Once-guarded).
- `akitaext/` — extensions on top of `akita`: `BufferedSender` (most components use this rather than raw `Port.Send`).
- `pipelining/` — generic pipeline stage helper.
- `ca/` — cache abstractions shared between cache implementations.
- `buffer.go`, `gvar.go` — small primitives.

## Adding a tracer

1. New file under `tracing/` implementing the tracer interface — see existing examples like `accesscounttracer.go`, `memoryaccesstracer.go`, `pteaccesstracer.go`.
2. Tracers register via hooks on `Component`s/`Port`s. They are attached in `mgpusim/runner/runner.go` based on flag values — add the flag and attachment logic there.
3. CSV columns and binary trace files go through `tracer.go` / `task.go` plumbing; do not hand-roll file writers.

## Singletons in `global`

Only add to `global` if the value truly cannot be threaded through normal constructors (e.g. LASP partitioning is consulted deep inside cache and memory-allocator code paths). Each new field needs a setter + getter on `*Global`. Initialize defaults in the `sync.Once` block of `GetInstance()`.

## Tests / lint

```bash
go test ./...
go generate ./...    # regenerate mocks in akitaext/
```
Follows the akita coding guideline (80-col, no comment paragraphs, ≤10-line functions).
