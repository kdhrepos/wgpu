# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Purpose

Memory system components for the Akita framework: caches, DRAM controllers, address translation (TLB/MMU), an ideal memory controller for non-timing simulation, and the byte-addressable `Storage` backing store. Consumed primarily by `mgpusim`.

## Layout

- `storage.go` — flat byte-addressable backing store used by all DRAM/ideal-controller components.
- `writebuffer.go` — small generic write-coalescing buffer.
- `idealmemcontroller/` — zero-latency memory used by the `ideal` platform and as a reference oracle.
- `dram/` — timing DRAM controller (`memcontroller.go`, builder, internal scheduler/bank state).
- `cache/` — see "Cache variants" below.
- `vm/` — virtual memory: `tlb/` (L1/L2 TLBs), `idealtlb/`, `mmu/` (page walker), `addresstranslator/` (caching translator in front of components), `tlbtracer.go`.
- `device/` — minimal "address-space owning device" abstraction shared by drivers.
- `trace/` — memory-trace replay/format helpers.
- `acceptancetests/` — Ginkgo acceptance tests that wire up real components end-to-end.

## Cache variants

`cache/` contains *parallel implementations* of the writeback cache, one per platform family. They share interfaces from `cache.go` / `protocol.go` / `directory.go` / `mshr.go` / `victimfinder.go` but each subdirectory is its own Go package (and `writebackrcr/` is its own *module* with its own `go.mod`):

| Directory | Used by platform |
| --- | --- |
| `writeback/` | the standard `mesh`, `direct`, `datadirect`, `ptdirect`, `nearfetch`, `nearfetch2`, `daware`, `infllc` |
| `writebackd/` | distribution-aware variant |
| `writeback_dynamic_clustering/` | `dynamic-cluster` |
| `writeback-dynamic-clustering-rep/` | `dynamic-cluster-rep` |
| `writeback-dynamic-dist/` | `dynamic-dist`, `dynamic-clustering` |
| `writebackrcr/`, `writebackrcr2/` | RCR protocol experiments (own module) |
| `writeback_static_clustering/` | `static-cluster` |
| `writethrough/` | `meshwt` |

Adding a new variant: copy the closest existing directory, rename the package, and add a `replace gitlab.com/akita/mem/cache/<name> => ../mem/cache/<name>` directive to **every** consuming module's `go.mod` (at minimum `mgpusim/go.mod`).

## Low-module finder

`cache/lowmodulefinder.go`, `hashlowmodulefinder.go`, `twolevellowmodulefinder.go` map physical addresses to the next-level component (e.g. L1→L2, L2→DRAM bank). Platforms in `mgpusim/platform/` pick a finder based on their address-interleaving scheme. When adding a new platform topology, you'll almost certainly need a custom finder here rather than modifying an existing one — the same finder is shared across all caches at a given level.

## Tests

Ginkgo specs with mockgen-generated mocks. Per-package suite tests follow the `*_suite_test.go` pattern.
```bash
go test ./...
go test ./cache/writeback -ginkgo.focus="<spec text>"
cd cache/writeback && go generate ./...   # regenerate mocks after interface changes
```
`acceptancetests/` is heavier — it builds real component graphs and runs them through scenarios; run before merging any cache or TLB change.

## Cross-module note

`replace` lines in `go.mod` point to `../akita`, `../util`, `../mgpusim`, `../lmf`, `../protocol`. Anything you add that crosses module boundaries needs matching replaces on both sides.
