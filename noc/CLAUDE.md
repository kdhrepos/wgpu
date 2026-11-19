# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Purpose

Network-on-chip primitives that sit between Akita `Component`s.

- `noc/` (this directory) — flits, request buffers, transfer events, traffic counter. The shared substrate.
- `noc/networking/` — higher-level topologies and policies (same `noc` module). Subpackages:
  - `arbitration/` — switch arbiters.
  - `routing/` — mesh (XY) and ideal routing tables.
  - `switching/` — multi-flit chiplet switch, endpoints, switch connections.
  - `pcie/` — PCIe interconnect between driver/CPU and the GPU.
  - `chipnetwork/` — inter-chiplet network builders: `meshchipnetwork.go` (mesh), `idealchipnetwork.go` (ideal).
  - `networking/` — top-level network builder used by `mgpusim/platform/`.

## Core types (this module)

- `flit.go` — `Flit` struct (the message unit on the wire).
- `requestbuffer.go` — small bounded buffer used by switches/endpoints.
- `transferevent.go` — `akita.Event` representing a flit traversing a link.
- `trafficcounter.go` — opt-in counter for bandwidth/utilization reporting.

## Tests / lint

```bash
go test ./...
```
Follows the akita coding guideline (80-col, no comment paragraphs, ≤10-line functions). See `../akita/doc/coding_guideline.md`.
