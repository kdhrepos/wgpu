"""Heatmaps for the 36x36 chiplet-pair affinity matrices.

Consumes the per-benchmark CSVs that chiplet_affinity_by_owner.py and
chiplet_affinity_by_cacheline.py drop into:
    ./<config>-<timestamp>/matrices/affinity_<benchmark>_<kind>.csv

For each (benchmark, kind), produce a heatmap with:
    - Chiplet IDs on both axes (re-orderable: physical / 2x2 / 3x3 /
      hierarchical leaves order on 1 - affinity).
    - Cluster boundary lines overlaid (2x2, 3x3, or both) so it's
      visually obvious whether intra-cluster blocks really are hotter
      than inter-cluster cells.

Usage:
    # All benchmarks, all matrix kinds, 2x2 overlay on a 2x2-grouped order
    python3 plot_chiplet_affinity.py --order 2x2 --overlay 2x2

    # One benchmark, one kind, hierarchical reorder (requires scipy)
    python3 plot_chiplet_affinity.py --benchmark gemm --kind owner-cosine \
        --order hierarchical --overlay both

The script reads from `--input-dir` (default: ./<config>-<timestamp>/matrices
under whichever `--date` you resolve via _paths) and writes PNGs to
`<input-dir>/../heatmaps/`.
"""

import argparse
import glob
import os
username = os.path.expanduser('~')
import sys

import matplotlib.pyplot as plt
import numpy as np
import pandas as pd

parent_dir = os.path.abspath(os.path.join(os.path.dirname(__file__), "../../"))
if parent_dir not in sys.path:
    sys.path.append(parent_dir)

from _paths import run_dir_tag, run_root
from formatter import (
    chiplet_to_cluster_2x2,
    chiplet_to_cluster_3x3,
    clusters_2x2,
    clusters_3x3,
)

NUM_CHIPLETS = 36
KINDS = (
    "owner-cosine",
    "owner-jaccard",
    "cacheline-jaccard",
    "cacheline-cosine",
)


def _cluster_order(clusters_dict):
    """Return chiplet IDs grouped contiguously by cluster_name (sorted)."""
    order = []
    for cname in sorted(clusters_dict.keys()):
        order.extend(clusters_dict[cname])
    return order


def _hierarchical_order(A):
    """Leaves order from average-linkage hierarchical clustering on (1 - A).

    Returns None if scipy isn't installed — caller should fall back.
    """
    try:
        from scipy.cluster.hierarchy import leaves_list, linkage
        from scipy.spatial.distance import squareform
    except ImportError:
        return None

    D = 1.0 - A
    # Ensure perfect symmetry + zero diagonal that squareform expects.
    D = (D + D.T) / 2.0
    np.fill_diagonal(D, 0.0)
    D = np.clip(D, 0.0, None)
    condensed = squareform(D, checks=False)
    Z = linkage(condensed, method="average")
    return list(leaves_list(Z))


def _resolve_order(mode, A):
    if mode == "physical":
        return list(range(NUM_CHIPLETS))
    if mode == "2x2":
        return _cluster_order(clusters_2x2)
    if mode == "3x3":
        return _cluster_order(clusters_3x3)
    if mode == "hierarchical":
        order = _hierarchical_order(A)
        if order is None:
            print("  scipy not available; falling back to 'physical' order")
            return list(range(NUM_CHIPLETS))
        return order
    raise ValueError(f"unknown order mode: {mode}")


def _cluster_boundaries(reordered_ids, cluster_map):
    """Indices where the cluster label changes along `reordered_ids`.

    Returns a list of indices (positions to draw vertical/horizontal
    boundary lines at). A line at index k means "between column k-1 and k".
    """
    bounds = []
    prev = None
    for idx, chip in enumerate(reordered_ids):
        c = cluster_map.get(chip)
        if prev is not None and c != prev:
            bounds.append(idx)
        prev = c
    return bounds


def _draw_heatmap(A, reordered_ids, title, out_path, overlay):
    A_reordered = A[np.ix_(reordered_ids, reordered_ids)]

    fig, ax = plt.subplots(figsize=(10, 9))
    im = ax.imshow(A_reordered, cmap="viridis", aspect="equal", vmin=0.0, vmax=1.0)

    cbar = fig.colorbar(im, ax=ax, shrink=0.85)
    cbar.set_label("affinity", fontsize=11)

    labels = [str(c) for c in reordered_ids]
    ax.set_xticks(np.arange(NUM_CHIPLETS))
    ax.set_yticks(np.arange(NUM_CHIPLETS))
    ax.set_xticklabels(labels, fontsize=7, rotation=90)
    ax.set_yticklabels(labels, fontsize=7)
    ax.set_xlabel("chiplet", fontsize=11)
    ax.set_ylabel("chiplet", fontsize=11)
    ax.set_title(title, fontsize=12, pad=12)

    # Cluster boundary overlays. A vertical/horizontal line at position k
    # separates column/row k-1 and k, so we draw at k-0.5 to land on the
    # cell edges.
    overlays = []
    if overlay in ("2x2", "both"):
        overlays.append(("2x2", chiplet_to_cluster_2x2, "white", "-", 1.5))
    if overlay in ("3x3", "both"):
        overlays.append(("3x3", chiplet_to_cluster_3x3, "red", "--", 1.2))

    for tag, cmap_, color, ls, lw in overlays:
        for k in _cluster_boundaries(reordered_ids, cmap_):
            ax.axvline(k - 0.5, color=color, linestyle=ls, linewidth=lw, alpha=0.9)
            ax.axhline(k - 0.5, color=color, linestyle=ls, linewidth=lw, alpha=0.9)

    if overlays:
        from matplotlib.lines import Line2D
        legend = [Line2D([0], [0], color=c, linestyle=ls, linewidth=lw, label=tag)
                  for tag, _, c, ls, lw in overlays]
        ax.legend(handles=legend, loc="upper left", bbox_to_anchor=(1.15, 1.0),
                  frameon=False, fontsize=9, title="cluster boundary")

    fig.tight_layout()
    fig.savefig(out_path, dpi=400, bbox_inches="tight")
    plt.close(fig)


def _discover_files(input_dir, benchmark_filter, kind_filter):
    """Yield (benchmark, kind, path). On-disk names use '_' between bench and kind tokens."""
    pattern = os.path.join(input_dir, "affinity_*.csv")
    for path in sorted(glob.glob(pattern)):
        base = os.path.basename(path)[len("affinity_"):-len(".csv")]
        # base is "<benchmark>_<kind>" where kind ∈ {owner_cosine, owner_jaccard,
        # cacheline_jaccard, cacheline_cosine}. Split on the *last* two tokens.
        parts = base.rsplit("_", 2)
        if len(parts) < 3:
            continue
        bench = parts[0]
        kind = f"{parts[1]}-{parts[2]}"
        if kind not in KINDS:
            continue
        if benchmark_filter and bench != benchmark_filter:
            continue
        if kind_filter and kind != kind_filter:
            continue
        yield bench, kind, path


def main():
    parser = argparse.ArgumentParser(
        description="Heatmaps for chiplet-pair affinity matrices."
    )
    parser.add_argument("--date", type=str, default="mesh-6x6",
                        help="run id used to locate the matrices/ subdir "
                             "(same format as the affinity scripts).")
    parser.add_argument("--input-dir", type=str, default=None,
                        help="explicit path to the matrices/ directory; "
                             "overrides --date when set.")
    parser.add_argument("--benchmark", type=str, default=None,
                        help="only plot this benchmark (default: every CSV in dir)")
    parser.add_argument("--kind", choices=list(KINDS) + ["all"], default="all",
                        help="which affinity matrix kind(s) to plot")
    parser.add_argument("--order", choices=["physical", "2x2", "3x3", "hierarchical"],
                        default="2x2",
                        help="row/column ordering: physical 0..35, "
                             "2x2/3x3 (chiplets within a cluster contiguous), "
                             "hierarchical (scipy leaves-order on 1 - affinity)")
    parser.add_argument("--overlay", choices=["none", "2x2", "3x3", "both"],
                        default="both",
                        help="cluster boundary lines on the heatmap")
    args = parser.parse_args()

    if args.input_dir:
        input_dir = args.input_dir
        out_root = os.path.dirname(input_dir.rstrip("/"))
    else:
        run_dir = run_root(args.date)
        out_root = f"{username}/mgvm/analyze/_out/cacheline/{run_dir_tag(run_dir)}"
        input_dir = os.path.join(out_root, "matrices")

    if not os.path.isdir(input_dir):
        print(f"matrices directory not found: {input_dir}")
        print("Run the affinity scripts with --save-matrices first.")
        sys.exit(1)

    out_dir = os.path.join(out_root, "heatmaps")
    os.makedirs(out_dir, exist_ok=True)

    kind_filter = None if args.kind == "all" else args.kind
    overlay = None if args.overlay == "none" else args.overlay

    files = list(_discover_files(input_dir, args.benchmark, kind_filter))
    if not files:
        print(f"no matrix CSVs matched in {input_dir} "
              f"(benchmark={args.benchmark}, kind={args.kind})")
        sys.exit(1)

    print(f"[heatmap] input:  {input_dir}")
    print(f"[heatmap] output: {out_dir}")
    print(f"[heatmap] order={args.order}  overlay={overlay}  files={len(files)}")

    for bench, kind, path in files:
        try:
            A = pd.read_csv(path, header=None).to_numpy(dtype=np.float64)
        except Exception as e:  # noqa: BLE001
            print(f"[{bench}/{kind}] read failed: {e}")
            continue
        if A.shape != (NUM_CHIPLETS, NUM_CHIPLETS):
            print(f"[{bench}/{kind}] unexpected shape {A.shape}, skip")
            continue

        order = _resolve_order(args.order, A)
        title = f"{bench} — {kind} (order={args.order})"
        out_png = os.path.join(out_dir, f"{bench}_{kind}_{args.order}.png")
        _draw_heatmap(A, order, title, out_png, overlay)
        print(f"  saved {out_png}")


if __name__ == "__main__":
    main()
