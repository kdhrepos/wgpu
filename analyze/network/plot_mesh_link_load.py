"""Per-link traffic load on a wafer-scale mesh under X-Y routing.

The runner only records chip-to-chip pair counts (src,dst,count) in each
per-benchmark CSV — no per-link information is logged. Under X-Y dimension-
order routing, the route of every message is fully determined by its (src,dst)
pair, so we can compute each link's load post-hoc by summing the counts of
every pair whose path traverses it.

X-Y order matches what the simulator actually does (see
noc/networking/routing/meshroutingtable.go:74): a message moves along X first
until it reaches the destination column, then along Y. Direction is collapsed
(undirected link), so the two segments (a→b) and (b→a) share a single line.

Chiplet ID → (x,y): the mesh builder in mgpusim/builders/mesh.go places
chiplets row-major as `id = row * xDim + col`, so x = id % xDim, y = id // xDim.

Inputs:
    --date <run_id>: resolves via analyze/_paths.run_root (default "mesh-6x6").
    --kind {data,ptw,both}: data = Chip-to-chip-traced,
                            ptw  = Chip-to-chip-ptw-traced,
                            both = sum of the two.

Output:
    ./<config>-<timestamp>/routing-load/<bench>_<kind>.png
"""

import argparse
import csv
import os
import re
import sys
from collections import defaultdict

import matplotlib.pyplot as plt
import numpy as np
from matplotlib.collections import LineCollection
from matplotlib.colors import LogNorm, Normalize

parent_dir = os.path.abspath(os.path.dirname(__file__))
if parent_dir not in sys.path:
    sys.path.append(parent_dir)

from _paths import report_dir, run_dir_tag, run_root

DATA_KIND = "link-load-heatmap"
DATA_KEY = "Chip-to-chip-traced"
PTW_KEY = "Chip-to-chip-ptw-traced"
PAIR_RE = re.compile(r"^\s*(\d+)\s*-\s*(\d+)\s*$")
PARAM_RE = re.compile(r"^([A-Za-z][\w-]*):\s*(.+?)\s*$")


def _parse_csv(path):
    """Return (xDim, yDim, {'data': {(src,dst): count}, 'ptw': ...})."""
    xDim = yDim = None
    counts = {"data": defaultdict(float), "ptw": defaultdict(float)}
    with open(path, newline="") as f:
        for row in csv.reader(f):
            if not row:
                continue
            where = row[0].strip()
            m = PARAM_RE.match(where)
            if m and m.group(1) == "xDim":
                xDim = int(m.group(2))
                continue
            if m and m.group(1) == "yDim":
                yDim = int(m.group(2))
                continue
            if where not in (DATA_KEY, PTW_KEY) or len(row) < 3:
                continue
            pair = PAIR_RE.match(row[1])
            if not pair:
                continue
            src, dst = int(pair.group(1)), int(pair.group(2))
            try:
                val = float(row[2])
            except ValueError:
                continue
            kind = "data" if where == DATA_KEY else "ptw"
            counts[kind][(src, dst)] += val
    return xDim, yDim, counts


def _xy_path_links(src, dst, xDim):
    """Yield undirected (a,b) coordinate-pair links on the X-then-Y path."""
    sx, sy = src % xDim, src // xDim
    dx, dy = dst % xDim, dst // xDim
    cx, cy = sx, sy
    while cx != dx:
        nx = cx + (1 if dx > cx else -1)
        a, b = (cx, cy), (nx, cy)
        yield (a, b) if a <= b else (b, a)
        cx = nx
    while cy != dy:
        ny = cy + (1 if dy > cy else -1)
        a, b = (cx, cy), (cx, ny)
        yield (a, b) if a <= b else (b, a)
        cy = ny


def _link_loads(pair_counts, xDim):
    loads = defaultdict(float)
    for (src, dst), count in pair_counts.items():
        if src == dst or count <= 0:
            continue
        for link in _xy_path_links(src, dst, xDim):
            loads[link] += count
    return loads


def _node_transit(pair_counts, xDim):
    """Total messages passing through each node (including own src/dst)."""
    transit = defaultdict(float)
    for (src, dst), count in pair_counts.items():
        if src == dst or count <= 0:
            continue
        sx, sy = src % xDim, src // xDim
        dx, dy = dst % xDim, dst // xDim
        cx, cy = sx, sy
        transit[(cx, cy)] += count
        while cx != dx:
            cx += 1 if dx > cx else -1
            transit[(cx, cy)] += count
        while cy != dy:
            cy += 1 if dy > cy else -1
            transit[(cx, cy)] += count
    return transit


def _draw(loads, transit, xDim, yDim, title, out_path, log_scale):
    if not loads:
        print(f"  {out_path}: no traffic, skip")
        return

    fig, ax = plt.subplots(figsize=(8.5, 7.8))

    link_keys = list(loads.keys())
    link_vals = np.array([loads[k] for k in link_keys], dtype=float)
    segments = [list(k) for k in link_keys]

    cmap = plt.cm.viridis
    if log_scale and link_vals.min() > 0:
        norm = LogNorm(vmin=link_vals.min(), vmax=link_vals.max())
    else:
        norm = Normalize(vmin=link_vals.min(), vmax=link_vals.max())

    vmax = link_vals.max() or 1.0
    widths = 0.6 + 7.4 * np.sqrt(link_vals / vmax)
    lc = LineCollection(segments, colors=cmap(norm(link_vals)),
                        linewidths=widths, capstyle="round", zorder=2)
    ax.add_collection(lc)

    # Node markers — size scaled by transit volume, colored uniformly so the
    # link colormap stays unambiguous.
    xs = [c % xDim for c in range(xDim * yDim)]
    ys = [c // xDim for c in range(xDim * yDim)]
    tmax = max(transit.values()) if transit else 1.0
    sizes = [220 + 480 * (transit.get((x, y), 0.0) / tmax) for x, y in zip(xs, ys)]
    ax.scatter(xs, ys, s=sizes, c="white", edgecolors="black",
               linewidths=1.2, zorder=3)
    for cid in range(xDim * yDim):
        ax.text(cid % xDim, cid // xDim, str(cid),
                ha="center", va="center", fontsize=7, zorder=4)

    ax.set_xlim(-0.7, xDim - 0.3)
    ax.set_ylim(-0.7, yDim - 0.3)
    ax.invert_yaxis()
    ax.set_aspect("equal")
    ax.set_xticks(range(xDim))
    ax.set_yticks(range(yDim))
    ax.set_xlabel("x (col)")
    ax.set_ylabel("y (row)")
    ax.set_title(title, fontsize=11)

    sm = plt.cm.ScalarMappable(cmap=cmap, norm=norm)
    sm.set_array([])
    cbar = fig.colorbar(sm, ax=ax, shrink=0.85)
    cbar.set_label("messages on link" + (" (log)" if isinstance(norm, LogNorm) else ""))

    total_link = link_vals.sum()
    top_link, top_val = max(loads.items(), key=lambda kv: kv[1])
    top_node, top_node_val = max(transit.items(), key=lambda kv: kv[1])
    ax.text(0.01, -0.12,
            f"links={len(loads)}  total={total_link:.0f}  "
            f"hottest link {top_link[0]}↔{top_link[1]}={top_val:.0f}  "
            f"hottest node {top_node}={top_node_val:.0f}",
            transform=ax.transAxes, fontsize=8, color="dimgray")

    fig.tight_layout()
    fig.savefig(out_path, dpi=400, bbox_inches="tight")
    plt.close(fig)


def main():
    parser = argparse.ArgumentParser(
        description="Mesh link load under X-Y routing, derived from "
                    "Chip-to-chip-traced pair counts.")
    parser.add_argument("--date", type=str, default="mesh-6x6",
                        help="run id (see analyze/_paths.py).")
    parser.add_argument("--benchmark", type=str, default=None,
                        help="only this benchmark (default: every CSV).")
    parser.add_argument("--kind", choices=["data", "ptw", "both"], default="both",
                        help="data=Chip-to-chip-traced, ptw=Chip-to-chip-ptw-traced, "
                             "both=sum (default).")
    parser.add_argument("--linear", action="store_true",
                        help="linear colormap (default: log).")
    args = parser.parse_args()

    run_dir = run_root(args.date)
    reports = report_dir(args.date)
    date_dir = run_dir_tag(run_dir)
    out_dir = os.path.join(".", DATA_KIND, date_dir)
    os.makedirs(out_dir, exist_ok=True)

    csvs = sorted(reports.glob("*.csv"))
    if args.benchmark:
        csvs = [p for p in csvs
                if p.stem == args.benchmark or p.stem.startswith(args.benchmark + "_")]
    if not csvs:
        print(f"no CSVs in {reports}")
        sys.exit(1)

    print(f"[mesh-link-load] reports: {reports}")
    print(f"[mesh-link-load] output:  {out_dir}")
    print(f"[mesh-link-load] kind={args.kind}  scale={'linear' if args.linear else 'log'}"
          f"  files={len(csvs)}")

    for path in csvs:
        bench = path.stem
        xDim, yDim, counts = _parse_csv(path)
        if xDim is None or yDim is None:
            print(f"  {bench}: xDim/yDim missing, skip")
            continue
        if args.kind == "data":
            pair_counts = counts["data"]
        elif args.kind == "ptw":
            pair_counts = counts["ptw"]
        else:
            pair_counts = defaultdict(float)
            for k, v in counts["data"].items():
                pair_counts[k] += v
            for k, v in counts["ptw"].items():
                pair_counts[k] += v
        loads = _link_loads(pair_counts, xDim)
        transit = _node_transit(pair_counts, xDim)
        out_png = os.path.join(out_dir, f"{bench}_{args.kind}.png")
        title = f"{bench}  —  X-Y routing link load ({args.kind}, {xDim}x{yDim})"
        _draw(loads, transit, xDim, yDim, title, out_png,
              log_scale=not args.linear)
        print(f"  saved {out_png}")


if __name__ == "__main__":
    main()
