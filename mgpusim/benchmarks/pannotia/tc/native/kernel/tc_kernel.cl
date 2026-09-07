/*
 * Triangle Counting kernel (vertex-centric merge-based set intersection).
 *
 * One workitem per vertex u.
 * For each neighbor v > u, count the size of N(u) ∩ N(v) using a merge
 * of the two sorted adjacency lists. Requires neighbor lists to be sorted
 * in ascending order (handled by the host before H2D copy).
 *
 * Output:
 *   tc_per_vertex[u] : local triangle contribution for vertex u
 *   total_count      : global triangle count (each triangle counted once)
 */
__kernel void tc_kernel(
    __global const int *row,          /* CSR row offsets (num_nodes+1) */
    __global const int *col,          /* CSR column indices (num_edges)  */
    __global       int *tc_per_vertex,/* per-vertex triangle count (out)  */
    __global       int *total_count,  /* global total (atomic, out)        */
    const int num_nodes,
    const int num_edges
)
{
    int u = get_global_id(0);
    if (u >= num_nodes) return;

    int count = 0;
    int u_start = row[u];
    int u_end   = row[u + 1];

    for (int ei = u_start; ei < u_end; ei++) {
        int v = col[ei];
        if (v <= u) continue; /* count each triangle exactly once */

        int v_start = row[v];
        int v_end   = row[v + 1];

        /* Merge-based intersection of sorted lists N(u) and N(v) */
        int pi = u_start;
        int pj = v_start;
        while (pi < u_end && pj < v_end) {
            int a = col[pi];
            int b = col[pj];
            if (a == b) {
                count++;
                pi++;
                pj++;
            } else if (a < b) {
                pi++;
            } else {
                pj++;
            }
        }
    }

    tc_per_vertex[u] = count;
    /* non-atomic stub: mgpusim lacks VI flat_atomic_* support */
    *total_count += count;
}
