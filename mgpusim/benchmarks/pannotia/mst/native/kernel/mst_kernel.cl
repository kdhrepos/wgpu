/*
 * MST kernels — Borůvka's algorithm, Shiloach-Vishkin style.
 *
 * Iteration: reset → find_min → union → compress, until changed==0.
 *
 * NOTE: min_wt / min_edge writes are non-atomic stubs (same convention as CC).
 * Race conditions may select a non-minimum edge, but the access pattern and
 * convergence behaviour are representative.
 *
 * Sharing characteristics:
 *   find_min: every thread in component C writes to min_wt[C] and min_edge[C]
 *     → heavy contention on per-component slots; Zipf graphs make a few large
 *     components dominate → extreme hot spots on those slots (unlike CC which
 *     has no such reduction stage).
 *   union: comp[] random read+write (same as CC hook).
 *   compress: comp[] sequential path compression (same as CC compress).
 */

#define INT_MAX_VAL 2147483647

/* 1. Reset per-component min state before each Boruvka round. */
__kernel void mst_reset(
    __global int *min_wt,
    __global int *min_edge,
    __global int *changed,
    const int num_nodes
)
{
    int u = get_global_id(0);
    if (u >= num_nodes) return;
    min_wt[u]   = INT_MAX_VAL;
    min_edge[u] = -1;
    if (u == 0) *changed = 0;
}

/* 2. For each vertex, find the minimum-weight cross-component edge.
 *    Write result into min_wt[srccomp] / min_edge[srccomp] (non-atomic). */
__kernel void mst_find_min(
    __global const int *row,
    __global const int *col,
    __global const int *weight,
    __global const int *comp,
    __global int *min_wt,
    __global int *min_edge,
    __global int *changed,
    const int num_nodes,
    const int num_edges
)
{
    int u = get_global_id(0);
    if (u >= num_nodes) return;

    int src_comp = comp[u];
    int u_start  = row[u];
    int u_end    = row[u + 1];

    for (int e = u_start; e < u_end; e++) {
        int dst_comp = comp[col[e]];
        if (dst_comp == src_comp) continue;

        int w = weight[e];
        /* non-atomic conditional write — races accepted */
        if (w < min_wt[src_comp]) {
            min_wt[src_comp]   = w;
            min_edge[src_comp] = e;
            *changed = 1;
        }
    }
}

/* 3. Component roots merge with their chosen min-edge destination.
 *    Only vertices where comp[u]==u (roots) act. */
__kernel void mst_union(
    __global const int *col,
    __global int *comp,
    __global const int *min_wt,
    __global const int *min_edge,
    __global int *changed,
    const int num_nodes
)
{
    int u = get_global_id(0);
    if (u >= num_nodes) return;

    int src = comp[u];
    if (src != u) return;           /* only roots act */
    if (min_wt[src] == INT_MAX_VAL) return; /* no outgoing edge */

    int dst = comp[col[min_edge[src]]];
    if (src == dst) return;

    int hi = src > dst ? src : dst;
    int lo = src < dst ? src : dst;
    comp[hi] = lo;   /* non-atomic stub */
    *changed = 1;
}

/* 4. Path compression: one pointer-jumping step per vertex (same as CC). */
__kernel void mst_compress(
    __global int *comp,
    __global int *changed,
    const int num_nodes
)
{
    int v = get_global_id(0);
    if (v >= num_nodes) return;

    int c  = comp[v];
    int c2 = comp[c];
    if (c != c2) {
        comp[v]  = c2;
        *changed = 1;
    }
}
