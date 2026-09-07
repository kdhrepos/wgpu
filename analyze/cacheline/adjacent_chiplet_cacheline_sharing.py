"""Adjacent chiplet cacheline sharing analysis.

Question: Do physically adjacent chiplets access the same cachelines more often?

Pipeline:
    1. Load memory trace (L2 miss rows)
    2. Drop duplicates, select needed columns
    3. Bucket addresses to cachelines (addr // 64)
    4. Compute pairwise Jaccard similarity per benchmark
    5. Group chiplet pairs by Manhattan distance
    6. Visualize per-benchmark sharing vs distance

Figure layout:
    - One row per benchmark
    - [left]  boxplot: Jaccard distribution by Manhattan distance
    - [right] bar: mean Jaccard for adjacent (dist=1) vs far (dist>=5)

Usage:
    analyze/run.sh cacheline/adjacent_chiplet_cacheline_sharing.py
    analyze/run.sh cacheline/adjacent_chiplet_cacheline_sharing.py --date mesh-6x6/latest
    analyze/run.sh cacheline/adjacent_chiplet_cacheline_sharing.py --save-heatmap
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
import matplotlib.gridspec as gridspec

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
CACHELINE_BYTES = 64

username = os.path.expanduser("~")

COORDS = {i: (i // xDim, i % xDim) for i in range(NUM_CHIPLETS)}

def _build_dist_matrix():
    n = NUM_CHIPLETS
    D = np.zeros((n, n), dtype=np.int32)
    for i in range(n):
        ri, ci = COORDS[i]
        for j in range(n):
            rj, cj = COORDS[j]
            D[i, j] = abs(ri - rj) + abs(ci - cj)
    return D

DIST_MATRIX = _build_dist_matrix()

DIST_GROUPS  = [1, 2, 3, 4, 5]
DIST_LABELS  = ["1\n(adj)", "2", "3", "4", "5+"]
FAR_DIST     = 5  # dist >= FAR_DIST -> "far"


def _load_trace(trace_file):
    df = pd.read_csv(trace_file, skipinitialspace=True)
    df = df.drop_duplicates()
    df = df[["addr", "chiplet", "rdma", "type"]]
    df = df[(df["rdma"] == True) & (df["type"].isin([2, 5]))]
    return df


def _per_chiplet_cacheline_sets(df):
    df = df.assign(cacheline=(df["addr"].astype(np.int64) // CACHELINE_BYTES))
    agg = df.groupby(["chiplet", "cacheline"], sort=False).size().reset_index(name="cnt")
    by_chip = {}
    for chip, sub in agg.groupby("chiplet"):
        cls = sub["cacheline"].to_numpy(dtype=np.int64)
        order = np.argsort(cls, kind="stable")
        by_chip[int(chip)] = cls[order]
    return by_chip


def _jaccard_matrix(by_chip):
    n = NUM_CHIPLETS
    J = np.zeros((n, n), dtype=np.float32)
    sizes = np.array([by_chip[i].size if i in by_chip else 0 for i in range(n)], dtype=np.int64)
    for i in range(n):
        if i not in by_chip:
            continue
        for j in range(i + 1, n):
            if j not in by_chip:
                continue
            inter = np.intersect1d(by_chip[i], by_chip[j], assume_unique=True).size
            union = sizes[i] + sizes[j] - inter
            if union > 0:
                J[i, j] = J[j, i] = inter / union
    np.fill_diagonal(J, 1.0)
    return J


def _jaccard_by_dist(J):
    result = {}
    for g in DIST_GROUPS:
        mask = (DIST_MATRIX == g) if g < FAR_DIST else (DIST_MATRIX >= g)
        vals = J[mask]
        result[g] = vals[vals > 0]
    return result


def process_benchmark(benchmark, trace_dir):
    trace_file = f"{trace_dir}/{benchmark}_mesh_{xDim}x{yDim}_memory_trace.csv"
    if not os.path.exists(trace_file):
        return benchmark, None, None
    print(f"[{benchmark}] computing cacheline Jaccard...")
    try:
        df = _load_trace(trace_file)
        if df.empty:
            return benchmark, None, None
        by_chip = _per_chiplet_cacheline_sets(df)
        if len(by_chip) < 2:
            return benchmark, None, None
        J = _jaccard_matrix(by_chip)
        return benchmark, J, _jaccard_by_dist(J)
    except Exception as e:
        print(f"[{benchmark}] error: {e}")
        return benchmark, None, None


def _plot_one_benchmark(bench, J, dist_data, bench_dir, save_heatmap):
    apply_paper_style()

    ADJ_COL = color_for(0)
    FAR_COL = color_for(3)

    fig, (ax_box, ax_bar) = plt.subplots(1, 2, figsize=(8, 3.0))
    fig.subplots_adjust(left=0.09, right=0.97, top=0.88, bottom=0.16, wspace=0.38)
    fig.suptitle(
        f"{bench}  —  Adjacent chiplet cacheline sharing  "
        f"(L2 miss, Jaccard = |A∩B|/|A∪B|)",
        fontsize=8.5, fontweight="bold",
    )

    # ── boxplot: Jaccard by distance ─────────────────────────────────────
    box_data = [dist_data[g].tolist() for g in DIST_GROUPS]
    bp = ax_box.boxplot(
        box_data,
        positions=range(len(DIST_GROUPS)),
        widths=0.55,
        patch_artist=True,
        medianprops=dict(color="white", linewidth=1.2),
        flierprops=dict(marker=".", markersize=1.5, alpha=0.25),
        showfliers=True,
    )
    palette = [color_for(i) for i in range(len(DIST_GROUPS))]
    for i, patch in enumerate(bp["boxes"]):
        patch.set_facecolor(palette[i])
        patch.set_alpha(0.85)
    for w in bp["whiskers"] + bp["caps"]:
        w.set(color="gray", linewidth=0.7)

    ax_box.set_xticks(range(len(DIST_GROUPS)))
    ax_box.set_xticklabels(DIST_LABELS, fontsize=8)
    ax_box.set_xlabel("Manhattan distance", fontsize=8)
    ax_box.set_ylabel("Jaccard similarity", fontsize=8)
    ax_box.set_title("Jaccard by distance", fontsize=8.5)
    ax_box.grid(axis="y", alpha=0.2, linewidth=0.5)
    ax_box.set_ylim(bottom=0)

    # ── bar: adjacent vs far mean Jaccard ────────────────────────────────
    adj_vals = dist_data.get(1, np.array([]))
    far_vals = dist_data.get(FAR_DIST, np.array([]))
    adj_mean = float(adj_vals.mean()) if adj_vals.size else 0.0
    far_mean = float(far_vals.mean()) if far_vals.size else 0.0

    bars = ax_bar.bar([0, 1], [adj_mean, far_mean],
                      color=[ADJ_COL, FAR_COL], width=0.5, alpha=0.9)
    bars[1].set_hatch("///")

    if far_mean > 1e-6:
        ratio = adj_mean / far_mean
        ax_bar.text(0.5, max(adj_mean, far_mean) * 1.08,
                    f"x{ratio:.2f}", ha="center", va="bottom",
                    fontsize=8.5, fontweight="bold")

    ax_bar.set_xticks([0, 1])
    ax_bar.set_xticklabels([f"Adjacent\n(dist=1)", f"Far\n(dist>={FAR_DIST})"], fontsize=8)
    ax_bar.set_ylabel("Mean Jaccard", fontsize=8)
    ax_bar.set_title("Adjacent vs Far", fontsize=8.5)
    ax_bar.grid(axis="y", alpha=0.2, linewidth=0.5)
    ax_bar.set_ylim(bottom=0, top=max(adj_mean, far_mean) * 1.3 + 1e-6)

    for bar, val in zip(bars, [adj_mean, far_mean]):
        ax_bar.text(bar.get_x() + bar.get_width() / 2,
                    val + max(adj_mean, far_mean) * 0.02,
                    f"{val:.4f}", ha="center", va="bottom", fontsize=7)

    out_paths = save_fig(fig, bench_dir, bench)
    print(f"  [{bench}] saved: {out_paths}")
    plt.close(fig)

    # ── optional heatmap: 6x6 neighbor Jaccard grid ──────────────────────
    if save_heatmap:
        nm = np.zeros(NUM_CHIPLETS)
        for i in range(NUM_CHIPLETS):
            nbr = J[i, DIST_MATRIX[i] == 1]
            nm[i] = float(nbr.mean()) if nbr.size else 0.0
        grid = nm.reshape(yDim, xDim)

        fig2, ax2 = plt.subplots(figsize=(4.5, 4.0))
        fig2.subplots_adjust(left=0.08, right=0.88, top=0.88, bottom=0.08)
        im = ax2.imshow(grid, cmap="YlOrRd", vmin=0)
        fig2.colorbar(im, ax=ax2, label="Mean Jaccard w/ neighbors")
        ax2.set_title(f"{bench}  —  neighbor Jaccard heatmap", fontsize=8.5)
        ax2.set_xlabel("Col", fontsize=8)
        ax2.set_ylabel("Row", fontsize=8)
        ax2.set_xticks(range(xDim))
        ax2.set_yticks(range(yDim))
        for ri in range(yDim):
            for ci in range(xDim):
                chip_id = ri * xDim + ci
                val = grid[ri, ci]
                tc = "white" if val > grid.max() * 0.65 else "black"
                ax2.text(ci, ri, f"{chip_id}\n{val:.3f}",
                         ha="center", va="center", fontsize=6, color=tc)
        hm_paths = save_fig(fig2, bench_dir, f"{bench}_heatmap")
        print(f"  [{bench}] heatmap: {hm_paths}")
        plt.close(fig2)


def _plot_per_benchmark(valid_results, base_out_dir, save_heatmap):
    for bench, J, dist_data in valid_results:
        bench_dir = base_out_dir / bench
        bench_dir.mkdir(parents=True, exist_ok=True)
        _plot_one_benchmark(bench, J, dist_data, bench_dir, save_heatmap)


def _resolve_out_dir(run_dir):
    tag = run_dir_tag(run_dir)
    return Path(f"{username}/mgvm/analyze/_out/cacheline/{tag}/adjacent_chiplet_cacheline_sharing")


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("--date", type=str, default="mesh-6x6")
    parser.add_argument("--save-heatmap", action="store_true")
    args = parser.parse_args()

    trace_dir = memory_trace_dir(args.date)
    run_dir   = run_root(args.date)
    out_dir   = _resolve_out_dir(run_dir)
    out_dir.mkdir(parents=True, exist_ok=True)

    print(f"[adjacent-sharing] traces: {trace_dir}")
    print(f"[adjacent-sharing] out:    {out_dir}")

    with ProcessPoolExecutor() as executor:
        results = list(executor.map(
            process_benchmark, benchmarks, repeat(str(trace_dir))
        ))

    valid = [(b, J, d) for b, J, d in results if J is not None]
    if not valid:
        print("No valid results.")
        return

    # summary CSV
    rows = []
    for bench, J, dist_data in valid:
        row = {"benchmark": bench}
        for g, lbl in zip(DIST_GROUPS, DIST_LABELS):
            vals = dist_data.get(g, np.array([]))
            key = lbl.replace("\n", "")
            row[f"mean_jaccard_dist{key}"] = round(float(vals.mean()), 5) if vals.size else 0.0
            row[f"n_pairs_dist{key}"] = int(vals.size)
        a = row.get("mean_jaccard_dist1(adj)", 0.0)
        f = row.get(f"mean_jaccard_dist5+", 0.0)
        row["adj_far_ratio"] = round(a / f, 3) if f > 1e-6 else float("inf")
        rows.append(row)

    summary_df = pd.DataFrame(rows)
    csv_path = out_dir / "adjacent_cacheline_sharing_summary.csv"
    summary_df.to_csv(csv_path, index=False)
    print(f"Summary CSV: {csv_path}")

    _plot_per_benchmark(valid, out_dir, args.save_heatmap)


if __name__ == "__main__":
    main()
