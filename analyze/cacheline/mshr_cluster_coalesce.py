"""MSHR-Reservation coalescing opportunity per cluster.

For each benchmark, count how many L2 misses (RDMA accesses) issued by
chiplets in the SAME cluster target the SAME cacheline within a small
cycle window — these are the misses that a cluster-level MSHR could
coalesce into a single outgoing request.

Cluster definitions live in analyze/formatter.py:
    - 2x2 clustering of a 6x6 wafer-scale GPU -> 9 clusters of 4 chiplets
    - 3x3 clustering of a 6x6 wafer-scale GPU -> 4 clusters of 9 chiplets

For each (benchmark, cluster_scheme, window) we report:
    - total_remote_accesses    : RDMA L2 misses where home is in a *different* cluster
    - coalesce_groups          : leader requests (one per coalesced bundle)
    - coalesced_accesses       : total_remote_accesses - coalesce_groups
    - coalesce_ratio_%         : 100 * coalesced_accesses / total_remote_accesses
    - avg_group_size           : mean coalesce-group size
    - max_group_size           : max coalesce-group size
    - groups_size_{1,2,3,4+}_% : group-size histogram by group count
    - access_in_groups>=2_%    : share of accesses that land in groups >= 2

Grouping uses a leader-anchored window: the first access opens an MSHR
that stays busy for W cycles; subsequent same-cluster, same-cacheline
accesses within W cycles of that leader coalesce. Once the window
expires the next access opens a new MSHR.

Usage:
    python3 mshr_cluster_coalesce.py --date mesh-6x6/20260603-004354
    python3 mshr_cluster_coalesce.py --date latest --windows 100,1000,10000
    python3 mshr_cluster_coalesce.py --cluster 2x2 --filter all
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
FREQ_HZ = 1_000_000_000  # 1 GHz: time(s) * 1e9 -> cycles


def _fmt_cycles(n):
    if n >= 1_000_000 and n % 1_000_000 == 0:
        return f"{n // 1_000_000}M"
    if n >= 1_000 and n % 1_000 == 0:
        return f"{n // 1_000}K"
    return str(int(n))


def _leader_anchored_groups(cycles, window):
    """Greedy leader-anchored grouping over an *already sorted* cycle array.

    Returns (group_sizes, leader_count). Each group has size >= 1 and
    every cycle in the group is within `window` cycles of the leader
    (the first cycle in that group).
    """
    n = cycles.size
    if n == 0:
        return np.empty(0, dtype=np.int64), 0
    sizes = []
    i = 0
    while i < n:
        leader = cycles[i]
        # rightmost index whose cycle <= leader + window
        j = np.searchsorted(cycles, leader + window, side="right")
        if j <= i:
            j = i + 1
        sizes.append(j - i)
        i = j
    return np.asarray(sizes, dtype=np.int64), len(sizes)


def _bucketize_group_sizes(sizes):
    """Return per-size counts in buckets {1, 2, 3, 4+}."""
    if sizes.size == 0:
        return {1: 0, 2: 0, 3: 0, "4+": 0}
    return {
        1: int(np.sum(sizes == 1)),
        2: int(np.sum(sizes == 2)),
        3: int(np.sum(sizes == 3)),
        "4+": int(np.sum(sizes >= 4)),
    }


def _scheme_stats(df, cluster_map, scheme_label, window):
    """Compute coalesce stats for a single clustering + window.

    `df` must already be filtered (L2 miss / PTW) and contain
    [time, addr, chiplet, owner]. It is not mutated.
    """
    work = df[["addr", "chiplet", "owner", "time"]].copy()
    work["cluster"] = work["chiplet"].map(cluster_map)
    work["home_cluster"] = work["owner"].map(cluster_map)

    # Keep only requests whose home is in a different cluster — those are
    # the ones that would actually pay an inter-cluster RDMA traversal and
    # therefore benefit from cluster-level MSHR coalescing.
    work = work[work["cluster"] != work["home_cluster"]]
    if work.empty:
        return None

    work["cacheline"] = work["addr"].astype(np.int64) // CACHELINE_BYTES
    work["cycle"] = (work["time"].astype(np.float64) * FREQ_HZ).astype(np.int64)
    work = work.sort_values(["cluster", "cacheline", "cycle"])

    total_accesses = len(work)
    group_sizes_all = []

    # Apply the leader-anchored scanner per (cluster, cacheline).
    grouped = work.groupby(["cluster", "cacheline"], sort=False)["cycle"]
    for _, cycles in grouped:
        sizes, _ = _leader_anchored_groups(cycles.to_numpy(), window)
        if sizes.size:
            group_sizes_all.append(sizes)

    if not group_sizes_all:
        return None

    sizes = np.concatenate(group_sizes_all)
    n_groups = int(sizes.size)
    coalesced = int(total_accesses - n_groups)
    bucket = _bucketize_group_sizes(sizes)
    accesses_in_multi = int(sizes[sizes >= 2].sum()) if (sizes >= 2).any() else 0

    return {
        "cluster_scheme": scheme_label,
        "window_cycles": int(window),
        "total_remote_accesses": int(total_accesses),
        "coalesce_groups": n_groups,
        "coalesced_accesses": coalesced,
        "coalesce_ratio_%": round(100.0 * coalesced / total_accesses, 2)
        if total_accesses
        else 0.0,
        "avg_group_size": round(float(sizes.mean()), 3),
        "max_group_size": int(sizes.max()),
        "groups_size_1_%": round(100.0 * bucket[1] / n_groups, 2) if n_groups else 0.0,
        "groups_size_2_%": round(100.0 * bucket[2] / n_groups, 2) if n_groups else 0.0,
        "groups_size_3_%": round(100.0 * bucket[3] / n_groups, 2) if n_groups else 0.0,
        "groups_size_4+_%": round(100.0 * bucket["4+"] / n_groups, 2)
        if n_groups
        else 0.0,
        "access_in_groups>=2_%": round(100.0 * accesses_in_multi / total_accesses, 2)
        if total_accesses
        else 0.0,
    }


def _load_trace(trace_file, args):
    usecols = ["time", "addr", "chiplet", "owner"]
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


def process_benchmark(benchmark, args, trace_dir):
    trace_file = f"{trace_dir}/{benchmark}_mesh_{xDim}x{yDim}_memory_trace.csv"
    if not os.path.exists(trace_file):
        return None

    print(
        f"[{benchmark}] MSHR-coalesce (filter={args.filter}, "
        f"exclude_ptw={args.exclude_ptw}, windows={args.windows}, "
        f"clusters={args.cluster})..."
    )

    try:
        df = _load_trace(trace_file, args)
        if df.empty:
            return None

        schemes = []
        if args.cluster in ("2x2", "both"):
            schemes.append(("2x2", chiplet_to_cluster_2x2))
        if args.cluster in ("3x3", "both"):
            schemes.append(("3x3", chiplet_to_cluster_3x3))

        rows = []
        for scheme_label, cluster_map in schemes:
            for window in args.windows:
                stats = _scheme_stats(df, cluster_map, scheme_label, window)
                if stats is None:
                    continue
                stats = {"benchmark": benchmark, **stats}
                rows.append(stats)

        if not rows:
            return None
        return pd.DataFrame(rows)

    except Exception as e:  # noqa: BLE001 — research script, surface and continue
        print(f"[{benchmark}] Error: {e}")
        return None


def _parse_windows(s):
    out = [int(x) for x in s.split(",") if x.strip()]
    if not out:
        raise argparse.ArgumentTypeError("--windows must list >=1 positive int(s)")
    if any(w <= 0 for w in out):
        raise argparse.ArgumentTypeError("--windows values must be positive")
    return out


def main():
    parser = argparse.ArgumentParser(
        description="Per-cluster MSHR coalescing opportunity from memory traces."
    )
    parser.add_argument(
        "--date",
        type=str,
        default="mesh-6x6",
        help="run id: 'latest', '<config>', '<config>/<timestamp>', or a timestamp.",
    )
    parser.add_argument(
        "--cluster",
        choices=["2x2", "3x3", "both"],
        default="both",
        help="cluster scheme to analyze",
    )
    parser.add_argument(
        "--windows",
        type=_parse_windows,
        default=[100, 1000, 10000],
        help="comma-separated coalesce windows in cycles (default: 100,1000,10000)",
    )
    parser.add_argument(
        "--filter",
        choices=["all", "l2miss"],
        default="l2miss",
        help="all: every memory-trace row; l2miss: rdma==True & type in {2,5}",
    )
    parser.add_argument(
        "--exclude-ptw",
        action="store_true",
        help="drop is_ptw==1 rows (page-table walk traffic)",
    )
    args = parser.parse_args()

    trace_dir = memory_trace_dir(args.date)
    run_dir = run_root(args.date)
    print(f"[mshr-coalesce] run: {run_dir}")
    print(f"[mshr-coalesce] traces: {trace_dir}")

    with ProcessPoolExecutor() as executor:
        results = list(
            executor.map(process_benchmark, benchmarks, repeat(args), repeat(str(trace_dir)))
        )
    results = [r for r in results if r is not None]
    if not results:
        print("No results generated.")
        return

    final_df = pd.concat(results, ignore_index=True)

    # Per-benchmark order matches the benchmarks list; cluster + window orderings
    # follow the input args naturally because we iterate in that order above.
    bench_index = {b: i for i, b in enumerate(benchmarks)}
    final_df = final_df.sort_values(
        by=["benchmark", "cluster_scheme", "window_cycles"],
        key=lambda c: c.map(bench_index) if c.name == "benchmark" else c,
        kind="stable",
    )

    # Mirror the per-run output convention used by sibling scripts.
    out_dir = f"{username}/mgvm/analyze/_out/cacheline/{run_dir_tag(run_dir)}"
    os.makedirs(out_dir, exist_ok=True)

    suffix_parts = [args.filter, args.cluster]
    if args.exclude_ptw:
        suffix_parts.append("no-ptw")
    suffix_parts.append("w" + "-".join(_fmt_cycles(w) for w in args.windows))
    suffix = "_" + "_".join(suffix_parts)
    output_path = f"{out_dir}/t_mshr_cluster_coalesce{suffix}.xlsx"

    final_df.to_excel(output_path, index=False)

    print("\n" + "=" * 100)
    print(f"{'MSHR-Reservation Coalesce Opportunity (per cluster, per window)':^100}")
    print("=" * 100)
    print(f"Total rows: {len(final_df)}")
    print(f"Saved to:   {output_path}")
    print("=" * 100)

    # Compact terminal summary: one row per (benchmark, cluster_scheme) per window.
    summary_cols = [
        "benchmark",
        "cluster_scheme",
        "window_cycles",
        "total_remote_accesses",
        "coalesce_groups",
        "coalesce_ratio_%",
        "access_in_groups>=2_%",
        "avg_group_size",
        "max_group_size",
    ]
    have_cols = [c for c in summary_cols if c in final_df.columns]
    print(final_df[have_cols].to_string(index=False))


if __name__ == "__main__":
    main()
