"""Chiplet-pair affinity from src->owner traffic patterns.

Hypothesis: two src chiplets that route the bulk of their requests to
the same owner chiplets have high "owner affinity". Clustering by this
metric should surface natural physical groupings.

Pipeline:
    1. Load memory-trace rows (default: RDMA L2 misses).
    2. Build a 36x36 access-count matrix M[src][owner].
       The diagonal (src == owner) is zeroed so local-memory traffic
       does not dominate the similarity (only inter-chiplet traffic
       matters for clustering decisions).
    3. Compute two affinity matrices over rows:
         - cosine similarity   (vector direction; emphasizes pattern)
         - weighted Jaccard    (sum min / sum max; emphasizes overlap volume)
    4. Compare against the physical 2x2 / 3x3 cluster partitions:
       mean intra-cluster affinity vs inter-cluster affinity, plus a ratio.
       A ratio > 1 means the physical clustering already captures real
       owner-affinity; values much greater than 1 suggest the clustering
       is tightly aligned with traffic.

Outputs (under ./<config>-<timestamp>/):
    matrices/affinity_<benchmark>_owner_cosine.csv     # 36x36, optional
    matrices/affinity_<benchmark>_owner_jaccard.csv    # 36x36, optional
    t_chiplet_affinity_owner_<suffix>.xlsx             # combined summary

Usage:
    python3 chiplet_affinity_by_owner.py
    python3 chiplet_affinity_by_owner.py --date mesh-6x6/20260603-004354 --save-matrices
    python3 chiplet_affinity_by_owner.py --filter all --exclude-ptw
"""

import argparse
import os
username = os.path.expanduser('~')
import sys

import numpy as np
import pandas as pd

parent_dir = os.path.abspath(os.path.join(os.path.dirname(__file__), "../../"))
if parent_dir not in sys.path:
    sys.path.append(parent_dir)

scripts_dir = os.path.abspath(os.path.join(os.path.dirname(__file__), "../../../scripts"))
if scripts_dir not in sys.path:
    sys.path.append(scripts_dir)

from concurrent.futures import ProcessPoolExecutor
from itertools import repeat

from _paths import memory_trace_dir, run_dir_tag, run_root
from formatter import chiplet_to_cluster_2x2, chiplet_to_cluster_3x3
from params import benchmarks

xDim = 6
yDim = 6
NUM_CHIPLETS = xDim * yDim


def _load_trace(trace_file, args):
    usecols = ["chiplet", "owner"]
    if args.filter == "l2miss":
        usecols += ["rdma", "type"]
    if args.exclude_ptw:
        usecols += ["is_ptw"]

    df = pd.read_csv(trace_file, skipinitialspace=True)
    df = df.drop_duplicates()
    df = df[usecols]
    if args.filter == "l2miss":
        df = df[(df["rdma"] == True) & (df["type"].isin([2, 5]))]
    if args.exclude_ptw:
        df = df[df["is_ptw"] == 0]
    return df


def _build_matrix(df):
    """36x36 M[src][owner] access counts; diagonal zeroed (no local self-traffic)."""
    M = np.zeros((NUM_CHIPLETS, NUM_CHIPLETS), dtype=np.int64)
    src = df["chiplet"].to_numpy()
    own = df["owner"].to_numpy()
    np.add.at(M, (src, own), 1)
    np.fill_diagonal(M, 0)
    return M


def _cosine(M):
    Mf = M.astype(np.float64)
    norms = np.linalg.norm(Mf, axis=1)
    safe = np.where(norms > 0, norms, 1.0)
    Mn = Mf / safe[:, None]
    A = Mn @ Mn.T
    zero = norms == 0
    A[zero, :] = 0.0
    A[:, zero] = 0.0
    np.fill_diagonal(A, 1.0)
    return A


def _weighted_jaccard(M):
    """W-Jaccard per row pair = sum_k min / sum_k max."""
    n = M.shape[0]
    A = np.zeros((n, n), dtype=np.float64)
    Mf = M.astype(np.float64)
    for i in range(n):
        row_i = Mf[i]
        mins = np.minimum(row_i[None, :], Mf)
        maxs = np.maximum(row_i[None, :], Mf)
        num = mins.sum(axis=1)
        den = maxs.sum(axis=1)
        A[i] = np.where(den > 0, num / den, 0.0)
    np.fill_diagonal(A, 1.0)
    return A


def _cluster_membership_mask(cluster_map, n=NUM_CHIPLETS):
    same = np.zeros((n, n), dtype=bool)
    for i in range(n):
        ci = cluster_map.get(i)
        if ci is None:
            continue
        for j in range(n):
            if cluster_map.get(j) == ci:
                same[i, j] = True
    np.fill_diagonal(same, False)
    return same


def _intra_inter(A, same_mask):
    off = ~np.eye(A.shape[0], dtype=bool)
    intra_vals = A[same_mask]
    inter_vals = A[off & ~same_mask]
    intra = float(intra_vals.mean()) if intra_vals.size else 0.0
    inter = float(inter_vals.mean()) if inter_vals.size else 0.0
    return intra, inter


# Pre-compute cluster-membership masks once per process (cheap, 36x36 bool).
_SAME_2X2 = _cluster_membership_mask(chiplet_to_cluster_2x2)
_SAME_3X3 = _cluster_membership_mask(chiplet_to_cluster_3x3)


def process_benchmark(benchmark, args, trace_dir, out_dir):
    trace_file = f"{trace_dir}/{benchmark}_mesh_{xDim}x{yDim}_memory_trace.csv"
    if not os.path.exists(trace_file):
        return None

    print(f"[{benchmark}] owner-affinity (filter={args.filter}, no-ptw={args.exclude_ptw})...")
    try:
        df = _load_trace(trace_file, args)
        if df.empty:
            return None

        M = _build_matrix(df)
        total_inter_chiplet_accesses = int(M.sum())
        if total_inter_chiplet_accesses == 0:
            return None

        cos_A = _cosine(M)
        jac_A = _weighted_jaccard(M)

        if args.save_matrices:
            mat_dir = f"{out_dir}/matrices"
            os.makedirs(mat_dir, exist_ok=True)
            pd.DataFrame(cos_A).to_csv(
                f"{mat_dir}/affinity_{benchmark}_owner_cosine.csv", index=False, header=False
            )
            pd.DataFrame(jac_A).to_csv(
                f"{mat_dir}/affinity_{benchmark}_owner_jaccard.csv", index=False, header=False
            )
            pd.DataFrame(M).to_csv(
                f"{mat_dir}/src_to_owner_{benchmark}.csv", index=False, header=False
            )

        c_in_2x2, c_out_2x2 = _intra_inter(cos_A, _SAME_2X2)
        c_in_3x3, c_out_3x3 = _intra_inter(cos_A, _SAME_3X3)
        j_in_2x2, j_out_2x2 = _intra_inter(jac_A, _SAME_2X2)
        j_in_3x3, j_out_3x3 = _intra_inter(jac_A, _SAME_3X3)

        def ratio(a, b):
            return round(a / b, 3) if b > 0 else float("inf")

        return {
            "benchmark": benchmark,
            "inter_chiplet_accesses": total_inter_chiplet_accesses,
            "cosine_intra_2x2": round(c_in_2x2, 4),
            "cosine_inter_2x2": round(c_out_2x2, 4),
            "cosine_ratio_2x2": ratio(c_in_2x2, c_out_2x2),
            "cosine_intra_3x3": round(c_in_3x3, 4),
            "cosine_inter_3x3": round(c_out_3x3, 4),
            "cosine_ratio_3x3": ratio(c_in_3x3, c_out_3x3),
            "wjaccard_intra_2x2": round(j_in_2x2, 4),
            "wjaccard_inter_2x2": round(j_out_2x2, 4),
            "wjaccard_ratio_2x2": ratio(j_in_2x2, j_out_2x2),
            "wjaccard_intra_3x3": round(j_in_3x3, 4),
            "wjaccard_inter_3x3": round(j_out_3x3, 4),
            "wjaccard_ratio_3x3": ratio(j_in_3x3, j_out_3x3),
        }
    except Exception as e:  # noqa: BLE001 — research script, surface and continue
        print(f"[{benchmark}] Error: {e}")
        return None


def _resolve_out_dir(run_dir):
    return f"{username}/mgvm/analyze/_out/cacheline/{run_dir_tag(run_dir)}"


def main():
    parser = argparse.ArgumentParser(
        description="Chiplet-pair owner-affinity (src->owner traffic) from memory traces."
    )
    parser.add_argument("--date", type=str, default="mesh-6x6",
                        help="run id: 'latest', '<config>', '<config>/<timestamp>', or a timestamp.")
    parser.add_argument("--filter", choices=["all", "l2miss"], default="l2miss",
                        help="all: every row; l2miss: rdma==True & type in {2,5}")
    parser.add_argument("--exclude-ptw", action="store_true",
                        help="drop is_ptw==1 rows (PTW traffic)")
    parser.add_argument("--save-matrices", action="store_true",
                        help="also save per-benchmark 36x36 affinity + raw M as CSV")
    args = parser.parse_args()

    trace_dir = memory_trace_dir(args.date)
    run_dir = run_root(args.date)
    out_dir = _resolve_out_dir(run_dir)
    os.makedirs(out_dir, exist_ok=True)

    print(f"[owner-affinity] run:    {run_dir}")
    print(f"[owner-affinity] traces: {trace_dir}")
    print(f"[owner-affinity] out:    {out_dir}")

    with ProcessPoolExecutor() as executor:
        results = list(executor.map(
            process_benchmark, benchmarks,
            repeat(args), repeat(str(trace_dir)), repeat(out_dir),
        ))
    results = [r for r in results if r is not None]
    if not results:
        print("No results generated.")
        return

    final_df = pd.DataFrame(results)
    suffix_parts = [args.filter]
    if args.exclude_ptw:
        suffix_parts.append("no-ptw")
    suffix = "_" + "_".join(suffix_parts)
    output_path = f"{out_dir}/t_chiplet_affinity_owner{suffix}.xlsx"
    final_df.to_excel(output_path, index=False)

    print("\n" + "=" * 100)
    print(f"{'Chiplet Owner-Affinity Summary (intra vs inter cluster)':^100}")
    print("=" * 100)
    print(f"Saved: {output_path}")
    if args.save_matrices:
        print(f"36x36 matrices: {out_dir}/matrices/")
    print("=" * 100)
    print(final_df.to_string(index=False))


if __name__ == "__main__":
    main()
