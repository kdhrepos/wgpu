"""Quantitative ranking of mesh link load under X-Y routing.

Single-panel bar chart per benchmark: every link ranked descending by total
messages, top-N labelled with their endpoint chiplet IDs. A red CDF on the
twin axis tells you how concentrated the traffic is (e.g. "top 8 of 60 links
handle 80% of traffic"), so under-utilized links appear as the long flat
tail on the right.

Reads the same Chip-to-chip-traced rows as plot_mesh_link_load.py and shares
its X-Y path expansion (`_link_loads`).

Output:
    ./<config>-<timestamp>/routing-load/<bench>_<kind>_ranking.png
"""

import argparse
import os
import sys

import matplotlib.pyplot as plt
import numpy as np
from matplotlib.ticker import FuncFormatter, LogLocator, MultipleLocator

parent_dir = os.path.abspath(os.path.dirname(__file__))
if parent_dir not in sys.path:
    sys.path.append(parent_dir)

from _paths import report_dir, run_dir_tag, run_root
from plot_mesh_link_load import _link_loads, _parse_csv

DATA_KIND = "link-load-ranking"


def _link_label(link, xDim):
    (ax, ay), (bx, by) = link
    return f"{ay * xDim + ax}~{by * xDim + bx}"


def _all_mesh_edges(xDim, yDim):
    """Every undirected edge of an xDim*yDim mesh, keyed the same way as
    `_link_loads` (lower-coord first tuple)."""
    edges = []
    for y in range(yDim):
        for x in range(xDim - 1):
            edges.append(((x, y), (x + 1, y)))
    for x in range(xDim):
        for y in range(yDim - 1):
            edges.append(((x, y), (x, y + 1)))
    return edges


def _merge(counts, kind):
    if kind == "data":
        return dict(counts["data"])
    if kind == "ptw":
        return dict(counts["ptw"])
    merged = {}
    for k, v in counts["data"].items():
        merged[k] = merged.get(k, 0.0) + v
    for k, v in counts["ptw"].items():
        merged[k] = merged.get(k, 0.0) + v
    return merged


def _draw(bench, kind, csv_path, out_path, top_n):
    xDim, yDim, counts = _parse_csv(csv_path)
    if xDim is None or yDim is None:
        print(f"  {bench}: xDim/yDim missing, skip")
        return

    pair_counts = _merge(counts, kind)
    loads = _link_loads(pair_counts, xDim)
    for edge in _all_mesh_edges(xDim, yDim):
        loads.setdefault(edge, 0.0)
    if not any(v > 0 for v in loads.values()):
        print(f"  {bench}: no traffic at all, skip")
        return

    sorted_links = sorted(loads.items(), key=lambda kv: kv[1], reverse=True)
    values = np.array([v for _, v in sorted_links], dtype=float)
    labels = [_link_label(k, xDim) for k, _ in sorted_links]
    ranks = np.arange(len(values))

    nonzero_mask = values > 0

    fig, ax = plt.subplots(figsize=(11, 6))
    ax.bar(ranks[nonzero_mask], values[nonzero_mask],
           color="steelblue", edgecolor="none")
    ax.set_xlabel("link rank (most → least loaded)")
    ax.set_ylabel(f"messages on link ({kind})")
    ax.set_title(f"{bench} — link load ranking "
                 f"({len(values)} links, {xDim}x{yDim} mesh)", fontsize=11)
    ax.set_yscale("log")
    ax.set_xlim(-0.8, len(values) - 0.2)
    ax.grid(True, axis="y", alpha=0.3, which="both")

    def _fmt_int(v, _):
        if v <= 0:
            return ""
        return f"{int(v):,}" if v >= 1 else f"{v:g}"

    def _fmt_minor(v, _):
        if v <= 0:
            return ""
        mant = v / (10 ** np.floor(np.log10(v)))
        if abs(mant - 2) < 0.1 or abs(mant - 5) < 0.1:
            return f"{int(v):,}" if v >= 1 else f"{v:g}"
        return ""

    ax.yaxis.set_major_locator(LogLocator(base=10, numticks=20))
    ax.yaxis.set_minor_locator(
        LogLocator(base=10, subs=[2, 3, 4, 5, 6, 7, 8, 9], numticks=200))
    ax.yaxis.set_major_formatter(FuncFormatter(_fmt_int))
    ax.yaxis.set_minor_formatter(FuncFormatter(_fmt_minor))
    ax.tick_params(axis="y", which="major", labelsize=9)
    ax.tick_params(axis="y", which="minor", labelsize=6.5, colors="dimgray")

    total = values.sum()
    cdf = np.cumsum(values) / total * 100.0
    ax2 = ax.twinx()
    ax2.plot(ranks, cdf, color="firebrick", lw=1.5, marker=".", ms=3, zorder=3)
    ax2.set_ylabel("cumulative share (%)", color="firebrick")
    ax2.tick_params(axis="y", colors="firebrick")
    ax2.set_ylim(0, 105)
    ax2.yaxis.set_major_locator(MultipleLocator(20))
    ax2.yaxis.set_minor_locator(MultipleLocator(5))
    ax2.tick_params(axis="y", which="minor", colors="firebrick", labelsize=6.5)
    ax2.axhline(80, color="firebrick", ls=":", lw=0.8, alpha=0.6)
    p80_rank = int(np.searchsorted(cdf, 80)) + 1
    ax2.text(p80_rank, 80, f"  top {p80_rank}/{len(values)} = 80%",
             fontsize=9, color="firebrick", va="center")

    ymin, ymax = ax.get_ylim()
    ax.set_ylim(ymin, ymax * 12)
    for i in range(min(top_n, len(values))):
        if values[i] <= 0:
            continue
        ax.text(i, values[i] * 1.4, labels[i],
                ha="center", va="bottom", fontsize=8, rotation=90)

    zero_idx = ranks[~nonzero_mask]
    n_zero = len(zero_idx)
    if n_zero > 0:
        ymin_new, _ = ax.get_ylim()
        y_mark = ymin_new * 1.6
        ax.scatter(zero_idx, [y_mark] * n_zero,
                   marker="x", color="firebrick", s=45, lw=1.6,
                   zorder=5, clip_on=False)
        ax.scatter([], [], marker="x", color="firebrick", s=45, lw=1.6,
                   label=f"zero traffic ({n_zero} links)")
        ax.legend(loc="upper right", fontsize=9, frameon=False)

    nonzero_sorted = [(k, v) for k, v in sorted_links if v > 0]
    if nonzero_sorted:
        min_link, min_v = nonzero_sorted[-1]
        min_label = _link_label(min_link, xDim)
    else:
        min_v, min_label = 0.0, "-"
    ax.text(0.99, -0.13,
            f"total={total:.0f}  "
            f"max={values[0]:.0f} ({labels[0]})  "
            f"min nonzero={min_v:.0f} ({min_label})  "
            f"zero={n_zero}/{len(values)}  "
            f"max/min={values[0] / max(min_v, 1):.1f}x",
            transform=ax.transAxes, ha="right", fontsize=8, color="dimgray")

    fig.tight_layout()
    fig.savefig(out_path, dpi=400, bbox_inches="tight")
    plt.close(fig)


def main():
    parser = argparse.ArgumentParser(
        description="Mesh link load ranking under X-Y routing.")
    parser.add_argument("--date", type=str, default="mesh-6x6",
                        help="run id (see analyze/_paths.py).")
    parser.add_argument("--benchmark", type=str, default=None,
                        help="only this benchmark (default: every CSV).")
    parser.add_argument("--kind", choices=["data", "ptw", "both"], default="both",
                        help="data=Chip-to-chip-traced, ptw=Chip-to-chip-ptw-traced, "
                             "both=sum (default).")
    parser.add_argument("--top-n", type=int, default=10,
                        help="links labelled by endpoint IDs (default: 10).")
    args = parser.parse_args()

    run_dir = run_root(args.date)
    date_dir = run_dir_tag(run_dir)
    out_dir = os.path.join(".", DATA_KIND, date_dir)
    os.makedirs(out_dir, exist_ok=True)

    reports = report_dir(args.date)
    csvs = sorted(reports.glob("*.csv"))
    if args.benchmark:
        csvs = [p for p in csvs
                if p.stem == args.benchmark
                or p.stem.startswith(args.benchmark + "_")]
    if not csvs:
        print(f"no CSVs in {reports}")
        sys.exit(1)

    print(f"[mesh-link-rank] reports: {reports}")
    print(f"[mesh-link-rank] output:  {out_dir}")
    print(f"[mesh-link-rank] kind={args.kind}  top-n={args.top_n}  "
          f"files={len(csvs)}")

    for path in csvs:
        bench = path.stem
        out_png = os.path.join(out_dir, f"{bench}_{args.kind}_ranking.png")
        _draw(bench, args.kind, path, out_png, args.top_n)
        print(f"  saved {out_png}")


if __name__ == "__main__":
    main()
