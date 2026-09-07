/*
 * fr-simple: a compute-thinned memory-access proxy for the Friend
 * Recommendation (fr) benchmark.
 *
 * WHY THIS EXISTS.
 *   fr's cross-chiplet sharing comes entirely from reading the adjacency
 *   list N(w) of each 1-hop neighbor w of the source vertex u: a high
 *   in-degree hub w is a neighbor of many u's, so N(w) is read by many
 *   source vertices spread across chiplets. fr's O(deg^3) cost, by
 *   contrast, is dominated by the 2-hop set-intersection (binary search +
 *   sorted-list merge over N(u), N(v)) -- cold, private traffic that is
 *   IRRELEVANT to a memory-system comparison (dynamic-clustering vs
 *   nearfetch) yet consumes the whole instruction budget, so the shared
 *   working set never materialises within a feasible simulation window.
 *
 * WHAT CHANGED.
 *   fr-simple KEEPS the hub-sharing access pattern (the 2-hop traversal
 *   that reads N(w) for every neighbor w of u) and DROPS the intersection.
 *   Per-vertex cost falls from O(deg^3) to O(deg^2), so the same shared
 *   working set is realised ~deg x sooner.
 *
 * OUTPUT (CPU-verifiable, but NOT the exact |N(u) intersect N(v)| score):
 *   fr_score[u] = number of 2-hop paths from u  (common-neighbor count
 *                 with multiplicity, a valid recommendation weight)
 *   fr_best[u]  = largest 2-hop vertex id seen
 *
 * The memory footprint and cross-chiplet sharing distribution match fr's;
 * only the redundant intersection arithmetic is removed. Neighbor lists
 * are still sorted ascending on the host (parity with fr; not required
 * here).
 */
__kernel void frsimple_kernel(
    __global const int *row,       /* CSR row offsets   (num_nodes + 1) */
    __global const int *col,       /* CSR column indices (num_edges)    */
    __global       int *fr_best,   /* per-vertex max 2-hop vertex id (out) */
    __global       int *fr_score,  /* per-vertex 2-hop path count    (out) */
    const int num_nodes,
    const int num_edges
)
{
    int u = get_global_id(0);
    if (u >= num_nodes) return;

    int u_start = row[u];
    int u_end   = row[u + 1];

    int best  = -1;
    int score = 0;

    for (int wi = u_start; wi < u_end; wi++) {
        int w       = col[wi];        /* 1-hop neighbor of u */
        int w_start = row[w];
        int w_end   = row[w + 1];

        /* Read N(w). For a hub w this line is read by every u that lists
         * w as a neighbor -> the exact cross-chiplet-shared access fr has. */
        for (int vi = w_start; vi < w_end; vi++) {
            int v = col[vi];
            if (v == u) continue;
            score++;
            if (v > best) best = v;
        }
    }

    fr_best[u]  = best;
    fr_score[u] = score;
}
