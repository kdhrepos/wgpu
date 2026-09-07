/*
 * Friend Recommendation kernel (vertex-centric, 2-hop expansion).
 *
 * For each vertex u, enumerate 2-hop neighbors v (v ∉ N(u), v ≠ u).
 * Recommendation score(u→v) = |N(u) ∩ N(v)| via sorted-list merge.
 * Output: fr_best[u] = highest-score candidate vertex ID,
 *         fr_score[u] = that score (−1/0 if no candidate exists).
 *
 * Requires neighbor lists sorted ascending (host sorts before H2D).
 *
 * Complexity per vertex: O(deg(u) * deg(w) * log(deg(u)) + merge)
 *   where w ranges over N(u). Correct but may repeat score computation
 *   for v reachable via multiple shared friends; result is still exact.
 */
__kernel void fr_kernel(
    __global const int *row,       /* CSR row offsets (num_nodes+1) */
    __global const int *col,       /* CSR column indices (num_edges) */
    __global       int *fr_best,   /* per-vertex best recommendation ID (out) */
    __global       int *fr_score,  /* per-vertex best recommendation score (out) */
    const int num_nodes,
    const int num_edges
)
{
    int u = get_global_id(0);
    if (u >= num_nodes) return;

    int u_start = row[u];
    int u_end   = row[u + 1];

    int best_v     = -1;
    int best_score = 0;

    for (int wi = u_start; wi < u_end; wi++) {
        int w       = col[wi];
        int w_start = row[w];
        int w_end   = row[w + 1];

        for (int vi = w_start; vi < w_end; vi++) {
            int v = col[vi];
            if (v == u) continue;

            /* Skip if v is already a direct friend of u (binary search on sorted N(u)). */
            int lo = u_start, hi = u_end, found = 0;
            while (lo < hi) {
                int mid = (lo + hi) >> 1;
                int c   = col[mid];
                if (c == v)      { found = 1; break; }
                else if (c < v)  lo = mid + 1;
                else             hi = mid;
            }
            if (found) continue;

            /* |N(u) ∩ N(v)| via merge of sorted adjacency lists. */
            int score = 0;
            int pi = u_start, pj = row[v], pj_end = row[v + 1];
            while (pi < u_end && pj < pj_end) {
                int a = col[pi], b = col[pj];
                if (a == b)     { score++; pi++; pj++; }
                else if (a < b) pi++;
                else            pj++;
            }

            if (score > best_score || (score == best_score && (best_v < 0 || v < best_v))) {
                best_score = score;
                best_v     = v;
            }
        }
    }

    fr_best[u]  = best_v;
    fr_score[u] = best_score;
}
