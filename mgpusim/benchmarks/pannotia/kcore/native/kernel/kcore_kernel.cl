/*
 * K-Core Decomposition kernels (iterative vertex peeling).
 *
 * A k-core is the maximal subgraph in which every vertex has degree >= k.
 * The algorithm repeatedly peels vertices whose current degree < k until
 * the graph stabilises.
 *
 * Three-kernel design:
 *
 *   init_degree_kernel   — initialise degree[v] = row[v+1] - row[v] for all v.
 *
 *   peel_kernel          — for each active vertex v, if degree[v] < k set
 *                          to_remove[v]=1 and active[v]=0, signal changed=1.
 *
 *   update_degree_kernel — for each newly removed vertex u (to_remove[u]==1),
 *                          decrement degree[w] for every neighbor w that is
 *                          still active.  Clear to_remove[u] after processing.
 *
 * After convergence, active[v]==1 iff v belongs to the k-core.
 *
 * Sharing characteristics:
 *   update_degree_kernel has all WGs simultaneously writing (decrementing) to
 *   the shared degree[] array via atomic_sub — high inter-WG sharing.
 *   peel_kernel writes to the shared changed flag via atomic_or.
 */

/* ---- 1. Init degree ---- */
__kernel void init_degree_kernel(
    __global const int *row,     /* CSR row offsets */
    __global       int *active,  /* 1 = vertex is alive */
    __global       int *degree,  /* current degree */
    const int num_nodes
)
{
    int v = get_global_id(0);
    if (v >= num_nodes) return;
    active[v] = 1;
    degree[v] = row[v + 1] - row[v];
}

/* ---- 2. Peel: mark low-degree vertices for removal ---- */
__kernel void peel_kernel(
    __global const int *degree,
    __global       int *active,
    __global       int *to_remove,
    __global       int *changed,
    const int k,
    const int num_nodes
)
{
    int v = get_global_id(0);
    if (v >= num_nodes) return;
    if (!active[v]) return;

    if (degree[v] < k) {
        active[v]    = 0;
        to_remove[v] = 1;
        /* non-atomic stub: mgpusim lacks VI flat_atomic_* support */
        *changed |= 1;
    }
}

/* ---- 3. Update degrees of neighbors of removed vertices ---- */
__kernel void update_degree_kernel(
    __global const int *row,
    __global const int *col,
    __global const int *active,
    __global       int *degree,
    __global       int *to_remove,
    const int num_nodes
)
{
    int u = get_global_id(0);
    if (u >= num_nodes) return;
    if (!to_remove[u]) return;

    for (int e = row[u]; e < row[u + 1]; e++) {
        int w = col[e];
        if (active[w]) {
            /* non-atomic stub: mgpusim lacks VI flat_atomic_* support */
            degree[w] -= 1;
        }
    }
    to_remove[u] = 0;   /* clear after processing */
}
