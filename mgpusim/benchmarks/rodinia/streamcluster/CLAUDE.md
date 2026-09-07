# streamcluster — RESOLVED (2026-06-02)

Status: works correctly at `-num=2097152 -dim=128 -k=64 -iter=8` with
LASP enabled.  No further action needed unless the kernel binary is
recompiled with different clang-ocl flags.

## TL;DR

This benchmark hit a panic that looked like a memory/translation bug
but was actually a **kernel-argument ABI mismatch** in the Go wrapper:
`encoding/binary.Write` does not emit Go's implicit struct padding,
so a missing explicit `Padding int32` between `Dim` and `X` in
`PgainKernelArgs` made `X` land 4 bytes earlier than the kernel ABI
expected.  The kernel then read a garbage value for `X`, used it as
an index, addressed an unmapped vAddr, and the DRAM controller
panicked with `external = 0x0`.

## The fix (already applied)

In `main.go::PgainKernelArgs`:

```go
Dim         int32
PaddingDimX int32 // 4B explicit pad: 8-aligns X         <-- ADDED
X           int64
K           int32
PaddingK    int32 // 4B explicit pad: 8-aligns Hidden*   <-- renamed from Padding
HiddenGlobalOffsetX int64
...
```

Verified offsets against `kernels.disasm` `s_load_*` probes:
- X at 0x38 (= 56) — `s_load_dwordx2 s[24:25], s[6:7], 0x38`
- K at 0x40 (= 64) — `s_load_dword s2, s[6:7], 0x40`
- HiddenGlobalOffsetX at 0x48 (= 72) — `s_load_dwordx2 s[20:21], s[6:7], 0x48`

Total struct size = 96 B (matches `co.KernargSegmentByteSize`).

No kernel (.cl) change was needed.  No simulator change was made.

## If streamcluster fails again

See `../bptree/CLAUDE.md` "Fallback options if the current fix fails"
section.  Priority 2 (binary.Size diagnostic) is the streamcluster-
specific check; Priorities 3–5 (instrument alloc map, isolate which
dimension, kernel restructure) apply to this benchmark too.

The streamcluster-specific Priority 5 entry there describes splitting
`Point_Struct *p` into three flat arrays as a last-resort restructure.

## Rejected hypotheses (do not re-try)

- ~~LASP `nextVAddr = 0` causes vAddr-0 reads to fail~~ — was applied
  to simulator and reverted.  Didn't help.
- ~~`v_mad_i64_i32` ABS-modifier missing in emu~~ — applied and
  reverted.  Didn't help.
- ~~Prepend a 4 KB dummy LASP guard to consume vAddr 0~~ — applied and
  removed.  Didn't help (because the issue was decode-side, not
  vAddr-side).

Owner directive (2026-06-01): **benchmark/script-only modifications**.
No simulator code changes.

## Context-preservation summary

If you are a future Claude continuing this work:

1. The current fix is benchmark-side: explicit padding in the Go
   kernel-args struct so `encoding/binary.Write` emits the exact
   layout the kernel binary expects.
2. `scripts/params.py` 'streamcluster' has 5 buffers, all
   `kernel-wide`: p, coord, work_mem, center_table, switch_membership.
3. Footprint at default size ≈ 1.6 GB (coord dominates).
4. The companion benchmark `bptree` had a different root cause
   (compiler-emitted `v_mad_i64_i32` on a non-power-of-2 struct
   stride); its fix is a kernel restructure documented in
   `../bptree/CLAUDE.md`.
