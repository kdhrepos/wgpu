# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Purpose

Network-on-chip primitives that sit between Akita `Component`s. Two modules live here:

- `noc/` (this directory) — flits, request buffers, transfer events, traffic counter. The shared substrate.
- `noc/networking/` — its **own Go module** (separate `go.mod`). Higher-level topologies and policies that consume `noc` and `mem`. Subpackages:
  - `arbitration/` — switch arbiters.
  - `routing/` — XY/dimension-order/etc. routing tables.
  - `switching/` — switch implementations.
  - `pcie/` — PCIe interconnect model used by some multi-GPU platforms.
  - `chipnetwork/` — on-chip mesh wiring used by wafer-scale topologies.
  - `networking/` — top-level network builder used by `mgpusim/platform/`.

## Core types (this module)

- `flit.go` — `Flit` struct (the message unit on the wire).
- `requestbuffer.go` — small bounded buffer used by switches/endpoints.
- `transferevent.go` — `akita.Event` representing a flit traversing a link.
- `trafficcounter.go` — opt-in counter for bandwidth/utilization reporting.

## Module boundary

Because `networking/` is its own module, public APIs on the outer `noc` package are de-facto frozen — changes break the inner module's compile. When changing a signature in this directory, update `networking/` in the same commit and bump both `go.mod` replace targets if needed (`mgpusim/go.mod` already has `replace gitlab.com/akita/noc/networking => ../noc/networking`).

## Tests / lint

```bash
go test ./...
cd networking && go test ./...
```
Both modules follow the akita coding guideline (80-col, no comment paragraphs, ≤10-line functions). See `../akita/doc/coding_guideline.md`.
