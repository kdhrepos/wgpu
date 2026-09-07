"""2x2 cluster sharing ratio analysis (adapted from top-n/top_cluster_2x2_shared_ratio.py).

For each remote address, counts how many distinct 2x2 clusters accessed it.
Produces per-benchmark figures showing:
    [left]  % of addresses by number of accessing clusters
    [right] % of accesses by number of accessing clusters

Motivation: if many addresses are shared across >=2 clusters, adjacent
chiplet clustering can reduce redundant remote traffic.

Output structure:
    _out/cacheline/[platform]-[date]/cluster_sharing_ratio/
        summary.csv
        [benchmark]/
            [benchmark].png

Usage:
    analyze/run.sh cacheline/cluster_sharing_ratio.py
    analyze/run.sh cacheline/cluster_sharing_ratio.py --date mesh-6x6/latest
    analyze/run.sh cacheline/cluster_sharing_ratio.py --group-size 4
    analyze/run.sh cacheline/cluster_sharing_ratio.py --read-only
"""

import argparse
import os
import sys
from concurrent.futures import ProcessPoolExecutor
from itertools import repeat
from pathlib import Path

import numpy as np
import pandas as pd
import matplotlib.pyplot as plt

parent_dir = os.path.abspath(os.path.join(os.path.dirname(__file__), "../../"))
if parent_dir not in sys.path:
    sys.path.append(parent_dir)
scripts_dir = os.path.abspath(os.path.join(os.path.dirname(__file__), "../../../scripts"))
if scripts_dir not in sys.path:
    sys.path.append(scripts_dir)

from _paths import memory_trace_dir, run_dir_tag, run_root
from plot_style import apply_paper_style, color_for, save_fig
from params import benchmarks

xDim = 6
yDim = 6
NUM_CHIPLETS = xDim * yDim
CLUSTER_W = 2  # 2x2 cluster width
cxDim = xDim // CLUSTER_W
cyDim = yDim // CLUSTER_W
MAX_CLUSTERS = cxDim * cyDim  # 9 for 6x6

username = os.path.expanduser("~")


def _build_cluster_map():
    m = {}
    for node in range(NUM_CHIPLETS):
        x, y = node % xDim, node // xDim
        cx, cy = x // CLUSTER_W, y // CLUSTER_W
        m[node] = f"c{cy * cxDim + cx}"
    return m

CHIPLET_TO_CLUSTER = _build_cluster_map()


def process_benchmark(benchmark, args, trace_dir):
    trace_file = f"{trace_dir}/{benchmark}_mesh_{xDim}x{yDim}_memory_trace.csv"
    if not os.path.exists(trace_file):
        return benchmark, None

    print(f"[{benchmark}] cluster sharing ratio...")
    try:
        df = pd.read_csv(trace_file, skipinitialspace=True)
        df = df.drop_duplicates()
        cols = ["addr", "chiplet", "owner", "rdma"]
        if args.read_only:
            cols.append("rw")
        df = df[cols]

        df["cluster"] = df["chiplet"].map(CHIPLET_TO_CLUSTER)
        df["owner_cluster"] = df["owner"].map(CHIPLET_TO_CLUSTER)

        if args.read_only:
            written = df.groupby("addr")["rw"].max().reset_index()
            written = written[written["rw"] != 0]["addr"].unique()
            rdma_df = df[(df["rdma"] == True) & (~df["addr"].isin(written))]
        else:
            rdma_df = df[df["rdma"] == True]

        remote_df = rdma_df[rdma_df["cluster"] != rdma_df["owner_cluster"]].copy()
        if remote_df.empty:
            return benchmark, None

        if args.group_size != 0:
            remote_df["addr"] = remote_df["addr"] // (64 * args.group_size)

        addr_metrics = remote_df.groupby("addr").agg(
            shared_clusters=("cluster", "nunique"),
            access_count=("addr", "count"),
        ).reset_index()

        total_addrs   = len(addr_metrics)
        total_accesses = int(addr_metrics["access_count"].sum())

        addr_dist   = addr_metrics["shared_clusters"].value_counts()
        access_dist = addr_metrics.groupby("shared_clusters")["access_count"].sum()

        row = {
            "benchmark": benchmark,
            "total_remote_addrs": total_addrs,
            "total_remote_accesses": total_accesses,
        }
        for i in range(1, MAX_CLUSTERS + 1):
            row[f"addr_pct_{i}clus"]   = round(addr_dist.get(i, 0)   / total_addrs    * 100, 2) if total_addrs    else 0.0
            row[f"access_pct_{i}clus"] = round(access_dist.get(i, 0) / total_accesses * 100, 2) if total_accesses else 0.0

        return benchmark, row

    except Exception as e:
        print(f"[{benchmark}] error: {e}")
        return benchmark, None


def _draw_stacked_fig(bench_names, mat, ylabel, title, out_dir, filename):
    apply_paper_style()

    n_bench = len(bench_names)
    n_clus  = MAX_CLUSTERS
    cmap    = plt.get_cmap("tab10")
    colors  = [cmap(i) for i in range(n_clus)]

    fig, ax = plt.subplots(figsize=(max(8, n_bench * 0.55), 3.8))
    fig.subplots_adjust(left=0.07, right=0.97, top=0.88, bottom=0.28)
    fig.suptitle(
        f"{title}  ({xDim}x{yDim} mesh, remote RDMA accesses)",
        fontsize=9, fontweight="bold",
    )

    x       = np.arange(n_bench)
    bar_w   = 0.65
    bottoms = np.zeros(n_bench)
    handles = []
    for j in range(n_clus):
        vals = mat[:, j]
        bars = ax.bar(x, vals, bar_w, bottom=bottoms, color=colors[j], alpha=0.92)
        handles.append(bars[0])
        for xi, (v, bot) in enumerate(zip(vals, bottoms)):
            if v >= 5.0:
                ax.text(xi, bot + v / 2, f"{v:.0f}%",
                        ha="center", va="center", fontsize=5.5,
                        color="white" if v > 12 else "black")
        bottoms += vals

    ax.set_xticks(x)
    ax.set_xticklabels(bench_names, rotation=45, ha="right", fontsize=7)
    ax.set_ylabel(ylabel, fontsize=8)
    ax.set_ylim(0, 100)
    ax.yaxis.set_major_formatter(plt.FuncFormatter(lambda v, _: f"{v:.0f}%"))
    ax.grid(axis="y", alpha=0.2, linewidth=0.5)

    ax.legend(handles,
              [str(i + 1) for i in range(n_clus)],
              title="# sharing clusters",
              ncol=n_clus, fontsize=7, title_fontsize=7.5,
              frameon=False,
              loc="upper center", bbox_to_anchor=(0.5, -0.28))

    out_dir.mkdir(parents=True, exist_ok=True)
    out = save_fig(fig, out_dir, filename)
    print(f"Saved: {out}")
    plt.close(fig)


def _plot_all_benchmarks(valid, base_out_dir):
    bench_names = [b for b, _ in valid]
    n_clus      = MAX_CLUSTERS

    addr_mat   = np.array([[r[f"addr_pct_{j}clus"]   for j in range(1, n_clus + 1)] for _, r in valid])
    access_mat = np.array([[r[f"access_pct_{j}clus"] for j in range(1, n_clus + 1)] for _, r in valid])

    _draw_stacked_fig(bench_names, addr_mat,
                      "% of remote addresses", "2x2 Cluster Sharing — Address Distribution",
                      base_out_dir / "address", "cluster_sharing_ratio_address")

    _draw_stacked_fig(bench_names, access_mat,
                      "% of remote accesses", "2x2 Cluster Sharing — Access Distribution",
                      base_out_dir / "access", "cluster_sharing_ratio_access")


def _resolve_out_dir(run_dir):
    tag = run_dir_tag(run_dir)
    return Path(f"{username}/mgvm/analyze/_out/cacheline/{tag}/cluster_sharing_ratio")


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("--date", type=str, default="mesh-6x6")
    parser.add_argument("--group-size", type=int, default=0,
                        help="group addresses by N cachelines (0 = no grouping)")
    parser.add_argument("--read-only", action="store_true",
                        help="exclude addresses that are ever written")
    args = parser.parse_args()

    trace_dir = memory_trace_dir(args.date)
    run_dir   = run_root(args.date)
    out_dir   = _resolve_out_dir(run_dir)
    out_dir.mkdir(parents=True, exist_ok=True)

    print(f"[cluster-sharing] traces: {trace_dir}")
    print(f"[cluster-sharing] out:    {out_dir}")

    with ProcessPoolExecutor() as executor:
        results = list(executor.map(
            process_benchmark, benchmarks,
            repeat(args), repeat(str(trace_dir)),
        ))

    valid = [(b, r) for b, r in results if r is not None]
    if not valid:
        print("No valid results.")
        return

    summary_df = pd.DataFrame([r for _, r in valid])

    addr_cols   = ["benchmark", "total_remote_addrs"]   + [f"addr_pct_{i}clus"   for i in range(1, MAX_CLUSTERS + 1)]
    access_cols = ["benchmark", "total_remote_accesses"] + [f"access_pct_{i}clus" for i in range(1, MAX_CLUSTERS + 1)]

    addr_dir   = out_dir / "address"
    access_dir = out_dir / "access"
    addr_dir.mkdir(parents=True, exist_ok=True)
    access_dir.mkdir(parents=True, exist_ok=True)

    summary_df[addr_cols].to_csv(addr_dir / "summary_address.csv", index=False)
    summary_df[access_cols].to_csv(access_dir / "summary_access.csv", index=False)
    print(f"Address CSV: {addr_dir / 'summary_address.csv'}")
    print(f"Access  CSV: {access_dir / 'summary_access.csv'}")

    _plot_all_benchmarks(valid, out_dir)

    # print summary table
    display_cols = (["benchmark", "total_remote_addrs", "total_remote_accesses"]
                    + [f"addr_pct_{i}clus" for i in range(1, MAX_CLUSTERS + 1)])
    print("\n" + "=" * 100)
    print(f"{'Cluster Sharing Ratio (2x2, 6x6 mesh)':^100}")
    print("=" * 100)
    print(summary_df[display_cols].to_string(index=False))


if __name__ == "__main__":
    main()
