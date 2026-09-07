# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Purpose

**L**ocal-home / **M**odule **F**inder. A tiny module that maps addresses to their "home" node (which chiplet/GPU owns a given address) for distributed memory platforms. Pulled into `mgpusim` via `replace gitlab.com/akita/lmf => ../lmf`.

## Files

- `homenodefinder.go` — interface every finder satisfies.
- `lowmodulefinder.go` — base helpers; the parallel of `mem/cache/lowmodulefinder.go` but for inter-chiplet routing.
- `static_localhomefinder.go` — fixed mapping (used by `static-cluster` platforms).
- `dynamic_localhomefinder.go` — mapping that updates as data migrates (used by `dynamic-cluster`, `dynamic-dist*` platforms).

## When to touch this module

- Adding a new clustering / distribution platform that needs a different address→home mapping.
- Changing how page-level locality is tracked across chiplets.

Both involve a paired change in `mgpusim/builders/` (constructor wiring) and `mgpusim/platform/` (which finder gets handed to the address translator and caches).

## Convention

Finders are stateful but cheap to allocate. They live for the lifetime of a simulation run. They are typically shared between the L2 cache, the address translator, and the RDMA engine — do not assume single ownership.
