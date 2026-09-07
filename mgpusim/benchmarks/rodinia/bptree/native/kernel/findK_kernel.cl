/*
 * B+ Tree findK kernel — flat-array variant.
 *
 * Layout: knodes[] split into two flat int32 arrays with a power-of-2
 * per-knode stride so addressing lowers to shift+add (no integer
 * multiply, no v_mad_i64_i32 emulation hazard).
 *
 *   slots 0..ORDER       — active keys / indices
 *   slots ORDER+1..STRIDE-1 — padding (keys = MAX_INT, indices = 0)
 *
 * Order 32 (was 64/256) increases tree depth (~log_order(size)) so each
 * query touches more internal nodes; the upper-level nodes are
 * fan-in-shared across all chiplets, which is what we want to measure.
 * Lower fan-out => more internal (index) knodes => a bigger hot+shared
 * band (the 1/order^2 index footprint grows ~4x from order 64 to 32).
 *
 * Two correctness fixes vs the previous (order=256, stride=512) build:
 *
 *  1. 64-bit shift for `currKnodeD[bid] << KNODE_STRIDE_LOG2`. The old
 *     `int << 9` compiled to `s_lshl_b32` and overflowed when the knode
 *     index reached 2^(32-SHIFT) nodes. The `long` cast below makes the
 *     shift 64-bit so it can never wrap regardless of order/stride; a
 *     smaller SHIFT (6 for order 32) only raises the safe threshold.
 *     Cast to `long` before the shift.
 *
 *  2. `currKnodeD` / `offsetD` marked `volatile`. The compiler otherwise
 *     emits `s_load_dword` (scalar K$ path) for these reads while the
 *     stores use `flat_store_dword` (vector path). The two caches are
 *     not coherent and the kernel does not emit `s_dcache_inv` after the
 *     barrier, so scalar reads can see stale values. Marking the pointer
 *     volatile forces the load down the vector path (flat_load), which
 *     is coherent with flat_store across the barrier.
 */

#define DEFAULT_ORDER       32
#define KNODE_STRIDE_LOG2   6         /* 2^6 = 64 ints per knode per array (fits order+1=33 slots) */

__kernel void findK(
    __global const int *knodes_keys,      /* flat: knode k -> keys[i]    at (k << SHIFT) + i */
    __global const int *knodes_indices,   /* flat: knode k -> indices[i] at (k << SHIFT) + i */
    __global const int *recordsD,
    __global volatile int *currKnodeD,
    __global volatile int *offsetD,
    __global const int *keysD,
    __global       int *ansD,
    const int height,
    const int knodes_elem)
{
    int thid = get_local_id(0);
    int bid  = get_group_id(0);

    for (int i = 0; i < height; i++) {
        long curr_base = ((long)currKnodeD[bid]) << KNODE_STRIDE_LOG2;
        long off_base  = ((long)offsetD[bid])    << KNODE_STRIDE_LOG2;

        if (knodes_keys[curr_base + thid]     <= keysD[bid] &&
            knodes_keys[curr_base + thid + 1]  > keysD[bid]) {
            int next = knodes_indices[off_base + thid];
            if (next < knodes_elem) {
                offsetD[bid] = next;
            }
        }
        barrier(CLK_GLOBAL_MEM_FENCE);

        if (thid == 0) {
            currKnodeD[bid] = offsetD[bid];
        }
        barrier(CLK_GLOBAL_MEM_FENCE);
    }

    /* Leaf-level equality probe. */
    long leaf_base = ((long)currKnodeD[bid]) << KNODE_STRIDE_LOG2;
    if (knodes_keys[leaf_base + thid] == keysD[bid]) {
        ansD[bid] = recordsD[knodes_indices[leaf_base + thid]];
    }
}
