# bptree — work-in-progress (resumed 2026-06-02 10:15)

## Status snapshot

- **kernel restructure DONE** — `findK_kernel.cl` rewritten to use two
  flat int32 arrays with power-of-2 per-knode stride.  `kernels.disasm`
  shows 0 occurrences of `v_mad_i64_i32`.  Kernel binary rebuilt on a
  machine with clang-ocl available.
- **kernarg ABI DONE** — `FindKKernelArgs` now has 7 Hidden* fields
  (3 GlobalOffset + 4 implicit: Printf, DefaultQueue, CompletionAction,
  MultigridSyncArg) to match the kernel binary's
  `KernargSegmentByteSize=120`.  Verified at runtime via the
  diagnostic `log.Printf` at the top of `exec()`.
- **streamcluster ✅** — sibling benchmark works after a similar ABI
  fix (different cause: missing explicit `PaddingDimX` field).  See
  `../streamcluster/CLAUDE.md`.
- **Priority 1 APPLIED (2026-06-02 10:15)** — dummy 4 KB page-0 guard
  restored at the head of `initMem`; `scripts/params.py` 'bptree'
  `data-partitioning` extended to 8 `kernel-wide` tokens (1 guard +
  7 real).  Sample binary rebuilt (`samples/bptree/bptree`).  Pending
  verification run.  Most recent panic before the guard was
  `external = 0xfc0` (a different in-page-0 offset than the previously
  recorded `0xff8` — same page-0 bug family, different access).

## Confirmed clean (do not re-investigate)

- `v_mad_i64_i32` compiler emission — eliminated by the kernel
  restructure.  `grep -c v_mad_i64_i32 kernels.disasm` returns 0.
- Kernarg struct ABI mismatch — fixed at 120 B with the 7 Hidden*
  fields.
- Knodes-struct alignment / stride — replaced by flat arrays.
- Streamcluster's symptom — fixed (different root cause: explicit
  padding field).

## The remaining bug (for tomorrow)

`external = 0xff8 = 4088` is a **process page 0 (vAddr 0..0xfff)
in-page byte offset**.  Compared with the diag alloc map, byte 4088
sits inside the first page of `knodesKeysD` (which is at vAddr 0
since LASP starts at 0 with no guard).  The DRAM controller receives
the request *without* the page-base part — i.e. the translation
chain on the request path is dropping the page number bits and
forwarding only the in-page offset.

This is the same symptom family as the original 0xfc0 panic, but
**after** all the kernel-side issues have been fixed.  The kernel no
longer has `v_mad_i64_i32`, the kernarg ABI is matched, the
disassembly looks healthy — yet some access still produces a
sub-Offset pAddr.

Two hypotheses:

(A) **Cache-line prefetch into page 0.**  The kernel reads
    `knodes_keys[(k << 9) + thid]` for k in {0, 1} and thid in [0,255],
    which together cover bytes 0..3071 of `knodesKeysD` (page 0).
    A cache prefetcher might pull in the rest of page 0 (bytes
    3072..4095) including byte 4088, and the simulator mis-translates
    that prefetch request.  This is **simulator-side**, not benchmark-
    side.

(B) **Some other buffer's byte-4088 access** is the real culprit.
    `recordsD[i]` at the leaf-probe step reads at i = record id.  If
    the kernel returns recordIdx = 1022 for any query, it accesses
    byte 4088 of recordsD — and that buffer's page 0 starts at vAddr
    `0xb88c000` (per the diag map).  Whether the same translation bug
    triggers there depends on whether it's a per-LASP-first-page bug
    or a generic any-page-0 bug.

If (A): the only viable benchmark-side workaround is the **dummy
page-0 guard** at the head of `initMem` (the one we removed during
the kernel restructure).  With the guard, `knodesKeysD` shifts to
vAddr 0x1000, page 0 of LASP becomes a never-read buffer, and the
prefetch can't trigger the bug.

If (B): the guard does not help.  This bug would be in the simulator's
generic in-page-0 translation handling, which violates the
benchmark/script-only rule and would need owner approval to fix.

## Things to try tomorrow (priority order)

1. **Restore the dummy guard.**  In `benchmark.go::initMem`, add
   `_ = alloc(uint64(1 << 12))` at the head of the allocation block.
   Update `scripts/params.py` 'bptree' `data-partitioning` to 8 tokens
   (1 guard + 7 real buffers: knodes_keys, knodes_indices, records,
   currKnode, offset, keys, ans).  Re-run.

   Outcome:
   - **panic gone** → hypothesis (A) confirmed.  Keep the guard.
     Document in this file that the kernel restructure was necessary
     for the explicit byte-4088 access but the guard is still needed
     to prevent the prefetch-into-page-0 bug.
   - **panic still 0xff8** → hypothesis (B).  Move to step 2.

2. **Confirm bug is generic per-buffer page 0 by reordering allocs.**
   In `initMem`, allocate `recordsD` FIRST (instead of `knodesKeysD`).
   If panic moves to a different external value or stops, the bug is
   buffer-position-specific.  If panic stays at 0xff8 from the buffer
   that's now first, it's generic-LASP-first-buffer.  Either way,
   diagnostic data for the owner-level discussion.

3. **Isolate per-knob (per CLAUDE.md Fallback Priority 4).**  Try
   `-platform-type=ideal`, `-xDim=2 -yDim=2`, `-mem-allocator-type=
   interleaved`, smaller `-size`/`-count`.  Each rules out one
   dimension.

4. **If 1–3 don't lead anywhere**, the bug is in the simulator's
   address translation / DRAM-routing path that the owner directive
   forbids us from touching.  Escalate.

## Files & state

- `native/kernel/findK_kernel.cl` — flat-array variant (active).
- `kernels.hsaco` — rebuilt from the above on a machine with clang-ocl.
  KernargSegmentByteSize = 120 (verified at runtime).
- `benchmark.go` — current state has:
  - 7-Hidden-field `FindKKernelArgs` (sized to 120 B).
  - flat-array `knodesKeysD` / `knodesIndicesD` allocation.
  - **NO dummy guard** at the moment.
  - Diagnostic `log.Printf` at the top of `exec()` (delete after
    tomorrow's debugging settles).
- `scripts/params.py` 'bptree' — 7 `kernel-wide` tokens, no guard.

## Historical / earlier diagnosis (kept for reference)

The sections below were written during the first round of debugging
(when v_mad_i64_i32 + kernarg ABI were the suspects).  Both of those
issues are now confirmed fixed — but the kernel-side fixes did not
end the panics.  Keep this material for context, not as the current
plan.  The "Things to try tomorrow" list at the top of this file is
the live work plan.

## What was changed and why (historical)

The bptree `findK` kernel was restructured to avoid a compiler-emitted
`v_mad_i64_i32` instruction that misbehaved under the MGPUSim functional
emulator.  No simulator code was modified.

## What was changed and why

**Symptom.** With the upstream-style kernel (single `Knode` struct array
of 2068-byte stride), bptree at `-size=12000000 -count=1000000` panicked
during kernel execution:

```
mem/idealmemcontroller/addressconverter.go:32: address is smaller than offset
panic: address is smaller than offset
ConvertExternalToInternal(..., 0xfc0)
```

`0xfc0 = 4032` is exactly the byte offset of `knodesD[1].keys[233]` from
the buffer base — i.e. the page-base portion of the address was being
dropped, leaving only the in-buffer offset.

**Root cause.** Clang-ocl for gfx803 emits

```
v_mad_i64_i32 v[3:4], s[0:1], s20, v4, v[2:3]   // base + k * stride
```

for `knodesD[k].keys[i]` addressing because the struct stride (2068 B)
is not a power of two.  The MGPUSim GCN3 functional emulator does not
correctly produce the 64-bit result of this instruction across the
particular operand shape clang-ocl emits here; the resulting wrong
address propagates to the DRAM controller as a sub-Offset pAddr and
hits the controller's "address belongs to this bank?" invariant.

**Fix.** Replace the device-side `__global knode *knodesD` argument with
two separate flat int32 arrays whose per-knode stride is a power of
two (512 ints = 2048 B):

| Array            | Size                          | Slot i in [0..256] | Slot i in [257..511] |
|------------------|-------------------------------|--------------------|----------------------|
| `knodes_keys`    | `num_knodes * 512 * 4 B`      | `knode[k].keys[i]` | sentinel `MaxInt32`  |
| `knodes_indices` | `num_knodes * 512 * 4 B`      | `knode[k].indices[i]` | 0                 |

Kernel addressing becomes `knodes_keys[(k << 9) + i]` — a pure shift +
add, no integer multiply, no `v_mad_i64_i32`.

The unused `location`, `is_leaf`, `num_keys` fields of the upstream
`knode` struct are dropped on the device side (they were never read).

**Memory impact.**  Per knode: 2068 B → 2 × 2048 B = 4096 B.  For
size = 12 M (≈ 47 k knodes) the on-GPU footprint grows from ≈ 97 MB to
≈ 192 MB.  Still well under the 4 GB-per-chiplet budget at any
chiplet count we sweep.

## Files in the changed set

- `native/kernel/findK_kernel.cl` — rewritten kernel (flat arrays).
- `benchmark.go` — `FindKKernelArgs` now carries `KnodesKeys` +
  `KnodesIndices` GPUPtrs instead of a single `Knodes`; `initMem`
  allocates two flat buffers and `exec` flattens `hKnodes []Knode` into
  them before H2D.  `Knode` struct on the host side is unchanged.
- `scripts/params.py` — `'bptree'` entry's `data-partitioning` lists
  7 `kernel-wide` tokens matching the new allocation order:
  `knodes_keys, knodes_indices, records, currKnode, offset, keys, ans`.

The dummy "page-0 guard" allocation that previously sat at the head of
`initMem` is **gone**; it was based on a wrong hypothesis (we believed
the LASP page-0 vAddr was special-cased somewhere — it isn't).  With
the flat-array kernel, the original panic does not reproduce, so the
guard is not needed.

## What did NOT fix this (rejected hypotheses, do not re-try)

1. **Implementing integer ABS in `mgpusim/emu/aluvop3a.go::vop3aPreProcessAbs`** —
   per GCN3 ISA, ABS modifiers on integer ops are hardware-ignored.
   The emulator's "log + skip" matches hardware.  The bptree binary
   does not even set ABS on its `v_mad_i64_i32` instructions (decoded
   bits 10..8 = 0).  Patch was applied and rolled back.

2. **Bumping the LASP allocator's `nextVAddr` from 0 to `1 << log2PageSize`**
   in `mem/device/memoryallocator.go::allocatePagesLASP` —
   did not change the panic.  Patch was applied and rolled back.

3. **Prepending a 4 KB dummy LASP allocation to consume vAddr 0..0xfff**
   in `initMem` — did not change the panic.  Removed.

The owner's directive is **benchmark/script-only modifications**.
Do not patch simulator code (`mgpusim/emu/*`, `mem/device/*`,
`mem/idealmemcontroller/*`, etc.) to fix benchmark behaviour.  If a
new bug surfaces here, prefer kernel/benchmark workarounds.

## Fallback options if the current fix fails

Run in priority order. Stop as soon as one resolves the panic or
narrows the cause enough to diagnose.

### Priority 1 — verify the build actually picked up the change (5 sec)

```bash
cd /home/ehdgus7279/mgvm/mgpusim/benchmarks/rodinia/bptree
ls -la kernels.hsaco                       # mtime should be recent
grep -c v_mad_i64_i32 kernels.disasm       # expect 0
grep "knodes_keys\|knodes_indices" kernels.disasm | head
```

If `v_mad_i64_i32` count is still > 0, `make clean && make` did not
regenerate from the new `findK_kernel.cl`.  The .hsaco is stale.

### Priority 2 — confirm the streamcluster ABI fix is encoded right (30 sec)

For streamcluster, add a one-shot diagnostic print in `main.go::exec`:

```go
import "encoding/binary"
// inside exec(), once:
var probe PgainKernelArgs
log.Printf("PgainKernelArgs binary.Size=%d  KernargSeg=%d",
    binary.Size(probe), b.kernel.KernargSegmentByteSize)
```

Expect both to print `96`.  If they differ, more ABI padding is needed.

### Priority 3 — instrument allocation map (2 min)

Add to `bptree/benchmark.go::initMem` (and analogously for
streamcluster's `initMem`):

```go
log.Printf("bptree alloc map:")
log.Printf("  knodesKeysD    = 0x%x (size %d B)", b.knodesKeysD, flatBytes)
log.Printf("  knodesIndicesD = 0x%x (size %d B)", b.knodesIndicesD, flatBytes)
log.Printf("  recordsD       = 0x%x (size %d B)", b.recordsD, recordsBytes)
log.Printf("  currKnodeD     = 0x%x (size %d B)", b.currKnodeD, queryBytes)
log.Printf("  offsetD        = 0x%x (size %d B)", b.offsetD, queryBytes)
log.Printf("  keysD          = 0x%x (size %d B)", b.keysD, queryBytes)
log.Printf("  ansD           = 0x%x (size %d B)", b.ansD, queryBytes)
```

Then panic's `external = 0xXXX` can be matched against an allocation
range to identify which buffer / offset misbehaves.

### Priority 4 — isolate which dimension is broken (5 min)

Re-run with ONE knob changed at a time:

| Variant | What it rules out |
|---|---|
| `-platform-type=ideal` | mesh-NoC routing bug |
| `-xDim=2 -yDim=2` (4 chiplets) | scale-related per-chiplet bug |
| `-mem-allocator-type=interleaved` (LASP off) | LASP allocator bug |
| `-max-inst=10000000` (10 M cap) | see how far sim got before panic |
| small workload (`-size=100000 -count=1000`) | scale-triggered bug |

If exactly one knob makes the panic go away, that dimension is the
trigger and the next investigation step is in that subsystem.

### Priority 5 — bigger fallback patches (if Priorities 1–4 don't resolve)

**bptree-specific:**

- Pad stride larger: change `KNODE_STRIDE_LOG2` to `10`
  (1024 ints = 4096 B per knode per array, = 1 page).  Make the
  matching update to `benchmark.go::KnodeStrideLog2`.  Memory grows
  4× but the addressing becomes a page-aligned shift.

- Try different clang-ocl flags in `makefile`:
  - `clang-ocl -mcpu=gfx803 -g -O0 ...` (disable optimization)
  - `clang-ocl -mcpu=fiji -g ...`           (alternate ISA target)

- Explicit byte-level addressing in the kernel:
  ```c
  size_t base_byte = ((size_t)curr_knode) << (KNODE_STRIDE_LOG2 + 2);
  size_t off_byte  = ((size_t)thid) << 2;
  int key = *(__global const int*)
      ((__global const char*)knodes_keys + base_byte + off_byte);
  ```
  Forces the compiler to compose byte offsets explicitly; can avoid a
  64-bit MAD even when the high-level expression looks innocent.

**streamcluster-specific:**

- If the ABI fix wasn't enough, restructure the kernel similarly —
  split `Point_Struct *p` into three flat arrays `p_weight`,
  `p_assign`, `p_cost` with native pow-of-2 strides (4 B / 8 B / 4 B).
  Update `main.go` arg struct (P → P_Weight, P_Assign, P_Cost) and
  `scripts/params.py` `data-partitioning` (5 → 7 tokens).  Eliminates
  the only `v_mad_i64_i32` instructions in this kernel.

**Both, last resort:**

- Re-introduce the 4 KB dummy LASP guard at the head of `initMem`
  (the one removed in this fix).  Harmless safety net; if there is a
  genuine vAddr-0 sensitivity somewhere we haven't characterised, the
  guard will mask it.  Remember to add a leading `kernel-wide` token
  to `scripts/params.py` for the benchmark.

### Priority 6 — give up and revert these benchmarks (if all else fails)

Comment out `"bptree"` and/or `"streamcluster"` in
`scripts/params.py::benchmarks`.  Trade-off: lose those data points
from the 16/36/64-chiplet sweep.  Document in the sweep results that
those rows are missing.

### Out-of-scope (do NOT pursue without owner approval)

Per owner directive (2026-06-01): no simulator-side modifications.
That means:

- Do NOT patch `mgpusim/emu/aluvop3a.go` to implement integer ABS
  handling — we tried, it didn't help, and it's owner-disallowed
  regardless.
- Do NOT patch `mem/device/memoryallocator.go::allocatePagesLASP`
  to start `nextVAddr` away from zero — we tried, it didn't help.
- Do NOT patch `mem/idealmemcontroller/addressconverter.go` to
  silently accept low addresses — that just hides the bug.

If genuinely no benchmark-side fix works, the right next step is to
escalate to the owner with the complete diagnostic record from
Priorities 1–4, not to silently flip the simulator boundary.

---

## If bptree fails again after this restructure

Decision tree:

1. **`external = 0xfc0` style panic returns** — verify `kernels.hsaco`
   was rebuilt from the new `findK_kernel.cl`.  Run
   `grep -c v_mad_i64_i32 kernels.disasm`; the new kernel should emit
   **zero** occurrences.  If non-zero, the .cl→.hsaco compile didn't
   pick up the change; rebuild with `cd benchmarks/rodinia/bptree &&
   make clean && make`.

2. **`external = 0x0` panic (literal zero)** — likely a kernel-argument
   ABI mismatch.  Check `FindKKernelArgs` field offsets against the
   actual `s_load_*` probes in `kernels.disasm` (look for the entries
   immediately after `findK:`).  encoding/binary.Write does NOT emit
   Go's implicit struct padding, so int64 fields after an odd number
   of int32 fields need an explicit `Padding int32` placeholder.

3. **Panic stack from elsewhere (not `addressconverter.go`)** — read
   the panic message carefully; this restructure family of bugs is
   confined to the address-conversion path.

## Useful pointers

- Kernel source: `native/kernel/findK_kernel.cl`
- Disassembly: `kernels.disasm` (rebuild with `make`)
- Go-side benchmark: `benchmark.go`
- Sample entry: `../../../samples/bptree/main.go`
- Sweep config: `../../../../scripts/params.py` (key: `'bptree'`)

## Context-preservation summary (for resuming next session)

If you are a future Claude continuing this work and the chat context
was lost, the key facts to know:

1. The owner's hard rule is **benchmark/script-only modifications**.
   No simulator-side patches.

2. The active fix for bptree is a **kernel restructure** to flat
   arrays with power-of-2 stride, eliminating the `v_mad_i64_i32`
   path that the GCN3 emulator mis-executes.

3. `scripts/params.py` 'bptree' uses `size=12000000, count=1000000`
   (~190 MB footprint with the flat-array layout). 7 buffers, all
   `kernel-wide` partitioning.

4. The companion benchmark `streamcluster` (sibling directory) had a
   different bug: a missing explicit `Padding int32` field between
   `Dim` and `X` in `PgainKernelArgs`, which made `encoding/binary`
   serialise the struct 4 bytes shorter than the kernel ABI expected.
   That benchmark's fix is the explicit Padding fields; no kernel
   restructure was needed there.
