# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Purpose

Wire-level message definitions shared by L1/L2 caches, RDMA engines, MMU, and DRAM.

## Files

- `protocol.go` — `ReadReq`/`WriteReq`/`DataReadyRsp`/`WriteDoneRsp`, translation messages, and the `AccessReq` interface.
- `meshprotocol.go` — small helpers for the mesh platform.

## Conventions

- Message types embed `akita.MsgMeta` or wrap an existing protocol message — do not re-implement the metadata fields.
- Naming: `<Verb><Object>Req` / `<Verb><Object>Rsp` (e.g. `ReadReq`, `ReadRsp`).
- This module depends only on `akita` and `mem` — keep it that way; do not import `mgpusim`.
