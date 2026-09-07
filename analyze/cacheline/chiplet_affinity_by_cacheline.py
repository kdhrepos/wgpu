"""Chiplet-pair affinity from shared cacheline touches.

Hypothesis: two src chiplets that touch many of the same cachelines have
high "cacheline affinity" — clustering them keeps cooperative working
sets local. Closer to MSHR-coalescing benefit than the owner-based view.

Pipeline:
    1. Load memory-trace rows (default: RDMA L2 misses).
    2. Bucket addresses to cachelines (addr // 64).
    3. For each chiplet, build a sorted ndarray of unique cachelines it
       touched + parallel per-cacheline access counts.
    4. For every chiplet pair, intersect the two sorted arrays
       (np.intersect1d, C-implemented sorted-merge) and compute:
         - binary Jaccard  : |A ∩ B| / |A ∪ B|
         - count cosine    : <cnts_A, cnts_B> / (||cnts_A|| * ||cnts_B||),
                             dot-product taken only over the intersection
    5. Compare against physical 2x2 / 3x3 cluster partitions
       (mean intra vs inter affinity + ratio).

Outputs (under ./<config>-<timestamp>/):
    matrices/affinity_<benchmark>_cacheline_jaccard.csv    # 36x36, optional
    matrices/affinity_<benchmark>_cacheline_cosine.csv     # 36x36, optional
    t_chiplet_affinity_cacheline_<suffix>.xlsx             # summary table

Usage:
    python3 chiplet_affinity_by_cacheline.py
    python3 chiplet_affinity_by_cacheline.py --date mesh-6x6/20260603-004354 --save-matrices
    python3 chiplet_affinity_by_cacheline.py --filter all --exclude-ptw
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
CACHELINE_BYTES = 64


def _load_trace(trace_file, args):
    usecols = ["addr", "chiplet"]
    if args.filter == "l2miss":
        usecols += ["rdma", "type"]
    elif args.filter in ("rdma-only", "local-only"):
        usecols += ["owner"]
    elif args.filter == "rdma-miss-only":
        usecols += ["owner", "rdma"]
    if args.exclude_ptw:
        usecols += ["is_ptw"]

    df = pd.read_csv(trace_file, skipinitialspace=True)
    df = df.drop_duplicates()
    df = df[usecols]
    if args.filter == "l2miss":
        df = df[(df["rdma"] == True) & (df["type"].isin([2, 5]))]
    elif args.filter == "rdma-only":
        df = df[df["chiplet"] != df["owner"]]
    elif args.filter == "local-only":
        df = df[df["chiplet"] == df["owner"]]
    elif args.filter == "rdma-miss-only":
        df = df[(df["chiplet"] != df["owner"]) & (df["rdma"] == True)]
    if args.exclude_ptw:
        df = df[df["is_ptw"] == 0]
    return df


def _per_chiplet_cacheline_sets(df):
    """For every chiplet, return (sorted unique cacheline ids, per-line access counts)."""
    df = df.assign(cacheline=(df["addr"].astype(np.int64) // CACHELINE_BYTES))
    agg = df.groupby(["chiplet", "cacheline"], sort=False).size().reset_index(name="count")

    by_chip = {}
    for chip, sub in agg.groupby("chiplet"):
        cls = sub["cacheline"].to_numpy(dtype=np.int64)
        cnts = sub["count"].to_numpy(dtype=np.int64)
        order = np.argsort(cls, kind="stable")
        by_chip[int(chip)] = (cls[order], cnts[order])
    return by_chip


def _affinity_matrices(by_chip):
    """Compute 36x36 binary-Jaccard and count-cosine affinity over chiplet pairs."""
    n = NUM_CHIPLETS
    jaccard = np.zeros((n, n), dtype=np.float64)
    cosine = np.zeros((n, n), dtype=np.float64)
    sizes = np.zeros(n, dtype=np.int64)
    norms = np.zeros(n, dtype=np.float64)
    for i in range(n):
        if i not in by_chip:
            continue
        cls_i, cnts_i = by_chip[i]
        sizes[i] = cls_i.size
        norms[i] = float(np.sqrt((cnts_i.astype(np.float64) ** 2).sum()))

    for i in range(n):
        if i not in by_chip:
            continue
        cls_i, cnts_i = by_chip[i]
        for j in range(i + 1, n):
            if j not in by_chip:
                continue
            cls_j, cnts_j = by_chip[j]
            common, idx_i, idx_j = np.intersect1d(
                cls_i, cls_j, assume_unique=True, return_indices=True,
            )
            inter = common.size
            union = sizes[i] + sizes[j] - inter
            if union > 0:
                jaccard[i, j] = jaccard[j, i] = inter / union
            if inter > 0 and norms[i] > 0 and norms[j] > 0:
                dot = float((cnts_i[idx_i].astype(np.float64)
                             * cnts_j[idx_j].astype(np.float64)).sum())
                cosine[i, j] = cosine[j, i] = dot / (norms[i] * norms[j])

    np.fill_diagonal(jaccard, 1.0)
    np.fill_diagonal(cosine, 1.0)
    return jaccard, cosine, sizes


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


_SAME_2X2 = _cluster_membership_mask(chiplet_to_cluster_2x2)
_SAME_3X3 = _cluster_membership_mask(chiplet_to_cluster_3x3)


def process_benchmark(benchmark, args, trace_dir, out_dir):
    trace_file = f"{trace_dir}/{benchmark}_mesh_{xDim}x{yDim}_memory_trace.csv"
    if not os.path.exists(trace_file):
        return None

    print(f"[{benchmark}] cacheline-affinity (filter={args.filter}, no-ptw={args.exclude_ptw})...")
    try:
        df = _load_trace(trace_file, args)
        if df.empty:
            return None

        by_chip = _per_chiplet_cacheline_sets(df)
        if not by_chip:
            return None

        jaccard, cosine, sizes = _affinity_matrices(by_chip)

        if args.save_matrices:
            mat_dir = f"{out_dir}/matrices"
            os.makedirs(mat_dir, exist_ok=True)
            pd.DataFrame(jaccard).to_csv(
                f"{mat_dir}/affinity_{benchmark}_cacheline_jaccard.csv",
                index=False, header=False,
            )
            pd.DataFrame(cosine).to_csv(
                f"{mat_dir}/affinity_{benchmark}_cacheline_cosine.csv",
                index=False, header=False,
            )
            pd.Series(sizes, name="unique_cachelines").to_csv(
                f"{mat_dir}/unique_cachelines_{benchmark}.csv", index_label="chiplet",
            )

        j_in_2x2, j_out_2x2 = _intra_inter(jaccard, _SAME_2X2)
        j_in_3x3, j_out_3x3 = _intra_inter(jaccard, _SAME_3X3)
        c_in_2x2, c_out_2x2 = _intra_inter(cosine, _SAME_2X2)
        c_in_3x3, c_out_3x3 = _intra_inter(cosine, _SAME_3X3)

        def ratio(a, b):
            return round(a / b, 3) if b > 0 else float("inf")

        return {
            "benchmark": benchmark,
            "total_unique_cachelines_touched": int(sizes.sum()),
            "jaccard_intra_2x2": round(j_in_2x2, 4),
            "jaccard_inter_2x2": round(j_out_2x2, 4),
            "jaccard_ratio_2x2": ratio(j_in_2x2, j_out_2x2),
            "jaccard_intra_3x3": round(j_in_3x3, 4),
            "jaccard_inter_3x3": round(j_out_3x3, 4),
            "jaccard_ratio_3x3": ratio(j_in_3x3, j_out_3x3),
            "cosine_intra_2x2": round(c_in_2x2, 4),
            "cosine_inter_2x2": round(c_out_2x2, 4),
            "cosine_ratio_2x2": ratio(c_in_2x2, c_out_2x2),
            "cosine_intra_3x3": round(c_in_3x3, 4),
            "cosine_inter_3x3": round(c_out_3x3, 4),
            "cosine_ratio_3x3": ratio(c_in_3x3, c_out_3x3),
        }
    except Exception as e:  # noqa: BLE001 — research script, surface and continue
        print(f"[{benchmark}] Error: {e}")
        return None


def _resolve_out_dir(run_dir):
    return f"{username}/mgvm/analyze/_out/cacheline/{run_dir_tag(run_dir)}"


def main():
    parser = argparse.ArgumentParser(
        description="Chiplet-pair affinity from shared cacheline touches."
    )
    parser.add_argument("--date", type=str, default="mesh-6x6",
                        help="run id: 'latest', '<config>', '<config>/<timestamp>', or a timestamp.")
    parser.add_argument("--filter",
                        choices=["all", "l2miss", "rdma-only", "local-only", "rdma-miss-only"],
                        default="l2miss",
                        help="all: every row; l2miss: rdma==True & type in {2,5}; rdma-only: chiplet!=owner; local-only: chiplet==owner; rdma-miss-only: chiplet!=owner & rdma==True")
    parser.add_argument("--exclude-ptw", action="store_true",
                        help="drop is_ptw==1 rows (PTW traffic)")
    parser.add_argument("--save-matrices", action="store_true",
                        help="also save per-benchmark 36x36 affinity matrices as CSV")
    args = parser.parse_args()

    trace_dir = memory_trace_dir(args.date)
    run_dir = run_root(args.date)
    out_dir = _resolve_out_dir(run_dir)
    os.makedirs(out_dir, exist_ok=True)

    print(f"[cacheline-affinity] run:    {run_dir}")
    print(f"[cacheline-affinity] traces: {trace_dir}")
    print(f"[cacheline-affinity] out:    {out_dir}")

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
    output_path = f"{out_dir}/t_chiplet_affinity_cacheline{suffix}.xlsx"
    final_df.to_excel(output_path, index=False)

    print("\n" + "=" * 100)
    print(f"{'Chiplet Cacheline-Affinity Summary (intra vs inter cluster)':^100}")
    print("=" * 100)
    print(f"Saved: {output_path}")
    if args.save_matrices:
        print(f"36x36 matrices: {out_dir}/matrices/")
    print("=" * 100)
    print(final_df.to_string(index=False))


if __name__ == "__main__":
    main()
