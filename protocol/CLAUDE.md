# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Purpose

Wire-level message definitions for the cache-coherence / data-distribution protocols used across `mem/cache/*`. Every cache variant in `mem/cache/` pairs with one of these protocols — keep them in sync.

## Files

- `protocol.go` — common message base types and shared enums.
- `meshprotocol.go` — default protocol for `mesh`/`meshwt` platforms (paired with `mem/cache/writeback`, `writethrough`).
- `distprotocol.go`, `distprotocol2.go` — distribution protocols for `dynamic-dist*` platforms (paired with `mem/cache/writeback-dynamic-dist`).
- `common_distrsp_protocol.go` — response messages shared between the dist variants.
- `rcrprotocol.go` — Remote Cache Read protocol (paired with `mem/cache/writebackrcr*`).
- `mshrcoalesceprotocol.go` — MSHR-coalescing variant.

## When adding a new protocol

1. Add the message types here.
2. Add a matching cache implementation under `../mem/cache/<name>/` (its own package, possibly its own module).
3. Wire it into the relevant builder under `../mgpusim/builders/` and `../mgpusim/platform/`.
4. Update `replace` directives in any consuming `go.mod`.

## Conventions

- Message types embed `akita.MsgMeta` or wrap an existing protocol message — do not re-implement the metadata fields.
- Naming: `<Verb><Object>Req` / `<Verb><Object>Rsp` (e.g. `ReadReq`, `ReadRsp`).
- This module depends only on `akita` and `mem` — keep it that way; do not import `mgpusim`.
