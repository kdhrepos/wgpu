/*
 * Connected Components kernels (Shiloach-Vishkin parallel union-find).
 *
 * Pannotia ships no upstream OpenCL source for connected_comp (see
 * SOURCES.md in this directory); this file is written by hand to match the
 * Go-side argument layouts in benchmarks/pannotia/cc/benchmark.go.
 *
 * Two-kernel design:
 *
 *   hook_kernel     - one thread per edge. Recover source vertex u from the
 *                     edge id by binary search over CSR row offsets, take
 *                     v = col[eid], then merge the components of u and v by
 *                     atomic_min onto the higher label. Signal progress
 *                     through the shared 'changed' flag.
 *
 *   compress_kernel - one thread per vertex. Pointer-jumping step
 *                     comp[v] = comp[comp[v]]. The host re-runs both kernels
 *                     until 'changed' stays 0, at which point all chains are
 *                     flattened.
 *
 * Sharing characteristics:
 *   hook_kernel has all work-groups simultaneously updating the shared comp[]
 *   array via atomic_min — high inter-WG sharing. compress_kernel reads/writes
 *   comp[] without atomics on the target slot (only the owning thread writes
 *   comp[v]) but reads any comp[c] concurrently with other threads' hooks
 *   from the prior kernel.
 */

/* ---- 1. Hook: merge component labels along each edge ---- */
__kernel void hook_kernel(
    __global const int *row,      /* CSR row offsets, length num_nodes + 1 */
    __global const int *col,      /* CSR column indices, length num_edges */
    __global       int *comp,     /* component labels, length num_nodes */
    __global       int *changed,  /* int32 flag, set to 1 on any merge */
    const int num_nodes,
    const int num_edges
)
{
    int eid = get_global_id(0);
    if (eid >= num_edges) return;

    /* Binary search for the largest u with row[u] <= eid. */
    int lo = 0;
    int hi = num_nodes;
    while (lo < hi) {
        int mid = (lo + hi) >> 1;
        if (row[mid + 1] <= eid) {
            lo = mid + 1;
        } else if (row[mid] > eid) {
            hi = mid;
        } else {
            lo = mid;
            break;
        }
    }
    int u = lo;
    int v = col[eid];

    int cu = comp[u];
    int cv = comp[v];
    if (cu == cv) return;

    int hi_lbl = cu > cv ? cu : cv;
    int lo_lbl = cu < cv ? cu : cv;

    /* non-atomic stubs: mgpusim lacks VI flat_atomic_* support */
    if (comp[hi_lbl] > lo_lbl) comp[hi_lbl] = lo_lbl;
    *changed |= 1;
}

/* ---- 2. Compress: one pointer-jumping step per vertex ---- */
__kernel void compress_kernel(
    __global       int *comp,
    __global       int *changed,
    const int num_nodes
)
{
    int v = get_global_id(0);
    if (v >= num_nodes) return;

    int c  = comp[v];
    int c2 = comp[c];
    if (c != c2) {
        comp[v] = c2;
        /* non-atomic stub: mgpusim lacks VI flat_atomic_* support */
        *changed |= 1;
    }
}
