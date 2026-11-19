# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Purpose

**L**ow-**M**odule **F**inder. Maps addresses to the next-level port (chiplet RDMA engine, L2 bank, DRAM bank) and to the home chiplet for the mesh wafer-scale GPU. Pulled into `mgpusim` via `replace gitlab.com/akita/lmf => ../lmf`.

## Files

- `lowmodulefinder.go` — interleaved / striped / banked finders (chiplet stripe = `numBanksPerChiplet × pageSize`, bank stripe = cache line).
- `homenodefinder.go` — address → home chiplet ID.

## Convention

Finders are stateful but cheap to allocate. They live for the lifetime of a simulation run. They are typically shared between the L2 cache, the address translator, and the RDMA engine — do not assume single ownership.
