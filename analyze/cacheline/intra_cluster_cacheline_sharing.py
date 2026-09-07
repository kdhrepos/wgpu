"""Intra-cluster cacheline sharing ratio.

Question:
    "Of the cachelines touched by chiplets in the same cluster, what
     fraction is touched by >= 2 chiplets within that cluster?"

For each (benchmark, cluster_scheme):
    1. Tag each access with its chiplet's cluster.
    2. For every (cluster, cacheline), compute the number of *distinct*
       chiplets *within that cluster* that touched it = the per-cluster
       sharing degree. Cap is 4 for 2x2, 9 for 3x3.
    3. Aggregate across all clusters in the scheme:
         - shared_lines_%     : lines with degree >= 2 / total touched lines
         - shared_accesses_%  : accesses landing on degree>=2 lines /
                                total accesses
         - per-degree histograms (lines + accesses): 1, 2, 3, 4+

By default counts every L2-miss access. With --remote-only the analysis
is restricted to accesses whose home chiplet is *outside* the cluster —
that ties the metric to MSHR-reservation savings, since only those lines
would benefit from cluster-level coalescing of outbound requests.

Outputs:
    ./<config>-<timestamp>/t_intra_cluster_cacheline_sharing_<suffix>.xlsx
    one row per (benchmark, cluster_scheme).

With --per-cluster, an extra sheet/file shows the breakdown per
individual cluster_name (cluster_0_2x2, cluster_1_2x2, ...).

Usage:
    python3 intra_cluster_cacheline_sharing.py
    python3 intra_cluster_cacheline_sharing.py --remote-only --filter l2miss
    python3 intra_cluster_cacheline_sharing.py --cluster 3x3 --per-cluster
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
CACHELINE_BYTES = 64
DEGREE_BUCKETS = ("1", "2", "3", "4+")  # cluster-internal sharing degree

CLUSTER_MAPS = {
    "2x2": chiplet_to_cluster_2x2,
    "3x3": chiplet_to_cluster_3x3,
}


def _load_trace(trace_file, args):
    usecols = ["addr", "chiplet"]
    needs_owner = args.remote_only or args.filter in ("rdma-only", "local-only", "rdma-miss-only")
    if needs_owner:
        usecols += ["owner"]
    if args.filter == "l2miss":
        usecols += ["rdma", "type"]
    elif args.filter == "rdma-miss-only":
        usecols += ["rdma"]
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


def _bucket_degree(d):
    """Map a per-cluster sharing degree to one of the histogram buckets."""
    if d <= 1:
        return "1"
    if d == 2:
        return "2"
    if d == 3:
        return "3"
    return "4+"


def _analyze_scheme(df, scheme_label, cluster_map, remote_only, per_cluster):
    """Return (scheme_summary_dict, per_cluster_df_or_None)."""
    work = df[["addr", "chiplet"] + (["owner"] if remote_only else [])].copy()
    work["cluster"] = work["chiplet"].map(cluster_map)
    work = work[work["cluster"].notna()]
    if work.empty:
        return None, None

    if remote_only:
        work["home_cluster"] = work["owner"].map(cluster_map)
        work = work[work["cluster"] != work["home_cluster"]]
        if work.empty:
            return None, None

    work["cacheline"] = work["addr"].astype(np.int64) // CACHELINE_BYTES

    # (cluster, cacheline) -> total accesses + distinct chiplets within the cluster.
    grp = work.groupby(["cluster", "cacheline"], sort=False)
    per_line = grp.agg(
        accesses=("chiplet", "count"),
        degree=("chiplet", "nunique"),
    ).reset_index()
    per_line["bucket"] = per_line["degree"].map(_bucket_degree)

    # ---- scheme-wide totals (sum over all clusters) ----------------------
    total_lines = int(len(per_line))
    total_accesses = int(per_line["accesses"].sum())
    shared_mask = per_line["degree"] >= 2
    shared_lines = int(shared_mask.sum())
    shared_accesses = int(per_line.loc[shared_mask, "accesses"].sum())

    line_hist = per_line["bucket"].value_counts().reindex(DEGREE_BUCKETS, fill_value=0)
    acc_hist = (per_line.groupby("bucket", observed=False)["accesses"]
                         .sum()
                         .reindex(DEGREE_BUCKETS, fill_value=0))

    avg_degree = float(per_line["degree"].mean()) if total_lines else 0.0
    max_degree = int(per_line["degree"].max()) if total_lines else 0

    summary = {
        "cluster_scheme": scheme_label,
        "total_lines_touched": total_lines,
        "shared_lines_count": shared_lines,
        "shared_lines_%": round(100.0 * shared_lines / total_lines, 2) if total_lines else 0.0,
        "total_accesses": total_accesses,
        "shared_accesses_count": shared_accesses,
        "shared_accesses_%": round(100.0 * shared_accesses / total_accesses, 2)
        if total_accesses else 0.0,
        "avg_degree": round(avg_degree, 3),
        "max_degree": max_degree,
    }
    for b in DEGREE_BUCKETS:
        summary[f"lines_deg_{b}_%"] = round(
            100.0 * int(line_hist[b]) / total_lines, 2) if total_lines else 0.0
    for b in DEGREE_BUCKETS:
        summary[f"accesses_deg_{b}_%"] = round(
            100.0 * int(acc_hist[b]) / total_accesses, 2) if total_accesses else 0.0

    # ---- per-cluster breakdown (optional) --------------------------------
    pc_df = None
    if per_cluster:
        rows = []
        for cluster_name, sub in per_line.groupby("cluster", sort=True):
            n_lines = len(sub)
            n_acc = int(sub["accesses"].sum())
            sh_mask = sub["degree"] >= 2
            sh_lines = int(sh_mask.sum())
            sh_acc = int(sub.loc[sh_mask, "accesses"].sum())
            lh = sub["bucket"].value_counts().reindex(DEGREE_BUCKETS, fill_value=0)
            ah = (sub.groupby("bucket", observed=False)["accesses"]
                      .sum()
                      .reindex(DEGREE_BUCKETS, fill_value=0))
            row = {
                "cluster_scheme": scheme_label,
                "cluster": cluster_name,
                "total_lines_touched": n_lines,
                "shared_lines_count": sh_lines,
                "shared_lines_%": round(100.0 * sh_lines / n_lines, 2) if n_lines else 0.0,
                "total_accesses": n_acc,
                "shared_accesses_count": sh_acc,
                "shared_accesses_%": round(100.0 * sh_acc / n_acc, 2) if n_acc else 0.0,
                "avg_degree": round(float(sub["degree"].mean()), 3),
                "max_degree": int(sub["degree"].max()),
            }
            for b in DEGREE_BUCKETS:
                row[f"lines_deg_{b}_%"] = round(
                    100.0 * int(lh[b]) / n_lines, 2) if n_lines else 0.0
            for b in DEGREE_BUCKETS:
                row[f"accesses_deg_{b}_%"] = round(
                    100.0 * int(ah[b]) / n_acc, 2) if n_acc else 0.0
            rows.append(row)
        pc_df = pd.DataFrame(rows)

    return summary, pc_df


def process_benchmark(benchmark, args, trace_dir):
    trace_file = f"{trace_dir}/{benchmark}_mesh_{xDim}x{yDim}_memory_trace.csv"
    if not os.path.exists(trace_file):
        return None, None

    print(
        f"[{benchmark}] intra-cluster sharing (filter={args.filter}, "
        f"no-ptw={args.exclude_ptw}, remote-only={args.remote_only})..."
    )
    try:
        df = _load_trace(trace_file, args)
        if df.empty:
            return None, None

        schemes = (
            [(args.cluster, CLUSTER_MAPS[args.cluster])]
            if args.cluster in CLUSTER_MAPS
            else [("2x2", CLUSTER_MAPS["2x2"]), ("3x3", CLUSTER_MAPS["3x3"])]
        )

        summary_rows = []
        per_cluster_rows = []
        for label, cmap in schemes:
            summary, pc_df = _analyze_scheme(
                df, label, cmap, args.remote_only, args.per_cluster
            )
            if summary is None:
                continue
            summary = {"benchmark": benchmark, **summary}
            summary_rows.append(summary)
            if pc_df is not None and not pc_df.empty:
                pc_df.insert(0, "benchmark", benchmark)
                per_cluster_rows.append(pc_df)

        if not summary_rows:
            return None, None
        summary_df = pd.DataFrame(summary_rows)
        pc_concat = pd.concat(per_cluster_rows, ignore_index=True) if per_cluster_rows else None
        return summary_df, pc_concat

    except Exception as e:  # noqa: BLE001
        print(f"[{benchmark}] Error: {e}")
        return None, None


def _resolve_out_dir(run_dir):
    return f"{username}/mgvm/analyze/_out/cacheline/{run_dir_tag(run_dir)}"


def main():
    parser = argparse.ArgumentParser(
        description="Intra-cluster cacheline sharing ratio from memory traces."
    )
    parser.add_argument("--date", type=str, default="mesh-6x6",
                        help="run id: 'latest', '<config>', '<config>/<timestamp>', or a timestamp.")
    parser.add_argument("--cluster", choices=["2x2", "3x3", "both"], default="both")
    parser.add_argument("--filter",
                        choices=["all", "l2miss", "rdma-only", "local-only", "rdma-miss-only"],
                        default="l2miss",
                        help="all: every row; l2miss: rdma==True & type in {2,5}; rdma-only: chiplet!=owner; local-only: chiplet==owner; rdma-miss-only: chiplet!=owner & rdma==True")
    parser.add_argument("--exclude-ptw", action="store_true",
                        help="drop is_ptw==1 rows (PTW traffic)")
    parser.add_argument("--remote-only", action="store_true",
                        help="keep only lines whose home chiplet is in a different cluster "
                             "(i.e., the lines a cluster-level MSHR would actually coalesce).")
    parser.add_argument("--per-cluster", action="store_true",
                        help="also emit a per-cluster breakdown file")
    args = parser.parse_args()

    trace_dir = memory_trace_dir(args.date)
    run_dir = run_root(args.date)
    out_dir = _resolve_out_dir(run_dir)
    os.makedirs(out_dir, exist_ok=True)

    print(f"[intra-cluster-sharing] run:    {run_dir}")
    print(f"[intra-cluster-sharing] traces: {trace_dir}")

    with ProcessPoolExecutor() as executor:
        results = list(executor.map(
            process_benchmark, benchmarks, repeat(args), repeat(str(trace_dir)),
        ))

    summaries = [s for s, _ in results if s is not None]
    per_clusters = [p for _, p in results if p is not None]

    if not summaries:
        print("No results generated.")
        return

    final_df = pd.concat(summaries, ignore_index=True)

    # Preserve benchmark-list order, then cluster_scheme alphabetic.
    bench_index = {b: i for i, b in enumerate(benchmarks)}
    final_df = final_df.sort_values(
        by=["benchmark", "cluster_scheme"],
        key=lambda c: c.map(bench_index) if c.name == "benchmark" else c,
        kind="stable",
    )

    suffix_parts = [args.filter, args.cluster]
    if args.exclude_ptw:
        suffix_parts.append("no-ptw")
    if args.remote_only:
        suffix_parts.append("remote-only")
    suffix = "_" + "_".join(suffix_parts)
    output_path = f"{out_dir}/t_intra_cluster_cacheline_sharing{suffix}.xlsx"
    final_df.to_excel(output_path, index=False)

    if args.per_cluster and per_clusters:
        pc_df = pd.concat(per_clusters, ignore_index=True)
        pc_df = pc_df.sort_values(
            by=["benchmark", "cluster_scheme", "cluster"],
            key=lambda c: c.map(bench_index) if c.name == "benchmark" else c,
            kind="stable",
        )
        pc_path = f"{out_dir}/t_intra_cluster_cacheline_sharing{suffix}_per_cluster.xlsx"
        pc_df.to_excel(pc_path, index=False)

    print("\n" + "=" * 100)
    print(f"{'Intra-Cluster Cacheline Sharing (scheme-wide aggregate)':^100}")
    print("=" * 100)
    print(f"Saved: {output_path}")
    if args.per_cluster and per_clusters:
        print(f"Per-cluster detail: {pc_path}")
    print("=" * 100)
    summary_cols = [
        "benchmark", "cluster_scheme",
        "total_lines_touched", "shared_lines_%",
        "total_accesses", "shared_accesses_%",
        "avg_degree", "max_degree",
        "lines_deg_2_%", "lines_deg_3_%", "lines_deg_4+_%",
    ]
    have_cols = [c for c in summary_cols if c in final_df.columns]
    print(final_df[have_cols].to_string(index=False))


if __name__ == "__main__":
    main()
