"""Per-chiplet received-request histogram.

Sums Chip-to-chip-traced rows by destination, giving the number of requests
each chiplet served (i.e. it was the dst). data and ptw can be split or
combined via --kind.

Recall the semantics fixed by hoptracer.go:
  - numC2CTraced[src][dst]++ counts one *transaction* (request + response).
  - src is the chiplet that issued the request; dst is the chiplet that
    served it. So summing over src for a fixed dst yields "how many
    requests landed on chiplet dst".

Output:
    ./<config>-<timestamp>/routing-load/<bench>_<kind>_recv.png
"""

import argparse
import os
import sys

import matplotlib.pyplot as plt
import numpy as np
from matplotlib.ticker import FuncFormatter, LogLocator

parent_dir = os.path.abspath(os.path.dirname(__file__))
if parent_dir not in sys.path:
    sys.path.append(parent_dir)

from _paths import report_dir, run_dir_tag, run_root
from plot_mesh_link_load import _parse_csv
from plot_mesh_link_load_quant import _merge

DATA_KIND = "chiplet-received"


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


def _received(pair_counts, num_chiplets):
    recv = np.zeros(num_chiplets, dtype=float)
    for (src, dst), v in pair_counts.items():
        if src == dst:
            continue
        if 0 <= dst < num_chiplets:
            recv[dst] += v
    return recv


def _draw(bench, kind, csv_path, out_path, log_scale):
    xDim, yDim, counts = _parse_csv(csv_path)
    if xDim is None or yDim is None:
        print(f"  {bench}: xDim/yDim missing, skip")
        return

    num_chiplets = xDim * yDim
    pair_counts = _merge(counts, kind)
    recv = _received(pair_counts, num_chiplets)

    fig, ax = plt.subplots(figsize=(12, 5.5))
    chiplets = np.arange(num_chiplets)
    nonzero_mask = recv > 0
    ax.bar(chiplets[nonzero_mask], recv[nonzero_mask],
           color="steelblue", edgecolor="none")

    ax.set_xlabel("chiplet ID")
    ax.set_ylabel(f"requests received ({kind})")
    ax.set_title(f"{bench} — per-chiplet received requests "
                 f"({num_chiplets} chiplets, {xDim}x{yDim} mesh)", fontsize=11)
    ax.set_xticks(chiplets)
    ax.tick_params(axis="x", labelsize=8)
    ax.set_xlim(-0.6, num_chiplets - 0.4)
    ax.grid(True, axis="y", alpha=0.3, which="both")

    if log_scale and nonzero_mask.any():
        ax.set_yscale("log")
        ax.yaxis.set_major_locator(LogLocator(base=10, numticks=20))
        ax.yaxis.set_minor_locator(
            LogLocator(base=10, subs=[2, 3, 4, 5, 6, 7, 8, 9], numticks=200))
        ax.yaxis.set_major_formatter(FuncFormatter(_fmt_int))
        ax.yaxis.set_minor_formatter(FuncFormatter(_fmt_minor))
        ax.tick_params(axis="y", which="major", labelsize=9)
        ax.tick_params(axis="y", which="minor", labelsize=6.5, colors="dimgray")
        ymin, ymax = ax.get_ylim()
        ax.set_ylim(ymin, ymax * 6)
    else:
        ax.yaxis.set_major_formatter(FuncFormatter(_fmt_int))

    # Zero-receiver chiplets — explicit × marker
    zero_idx = np.where(~nonzero_mask)[0]
    if len(zero_idx) > 0:
        ymin, _ = ax.get_ylim()
        y_mark = ymin * 1.6 if log_scale else ymin
        ax.scatter(zero_idx, [y_mark] * len(zero_idx),
                   marker="x", color="firebrick", s=50, lw=1.7,
                   zorder=5, clip_on=False)
        ax.scatter([], [], marker="x", color="firebrick", s=50, lw=1.7,
                   label=f"zero received ({len(zero_idx)} chiplets)")
        ax.legend(loc="upper right", fontsize=9, frameon=False)

    # Mean reference line (over nonzero) to flag under/over-utilized
    if nonzero_mask.any():
        mean_all = recv.mean()
        ax.axhline(mean_all, color="firebrick", ls="--", lw=0.9, alpha=0.7,
                   zorder=2)
        ax.text(num_chiplets - 0.5, mean_all, f"  mean={mean_all:.0f}",
                ha="left", va="center", fontsize=8, color="firebrick")

    # Bottom summary
    total = recv.sum()
    if nonzero_mask.any():
        argmax = int(np.argmax(recv))
        nz_values = recv[nonzero_mask]
        argmin_in_nz = int(chiplets[nonzero_mask][int(np.argmin(nz_values))])
        cv = recv.std() / recv.mean() if recv.mean() > 0 else 0
        ax.text(0.99, -0.18,
                f"total={total:.0f}  "
                f"max=chiplet[{argmax}]={recv[argmax]:.0f}  "
                f"min nonzero=chiplet[{argmin_in_nz}]={recv[argmin_in_nz]:.0f}  "
                f"zero={(~nonzero_mask).sum()}/{num_chiplets}  "
                f"std/mean={cv:.2f}",
                transform=ax.transAxes, ha="right", fontsize=8, color="dimgray")

    fig.tight_layout()
    fig.savefig(out_path, dpi=400, bbox_inches="tight")
    plt.close(fig)


def main():
    parser = argparse.ArgumentParser(
        description="Per-chiplet received-request histogram, "
                    "summed over Chip-to-chip-traced rows.")
    parser.add_argument("--date", type=str, default="mesh-6x6",
                        help="run id (see analyze/_paths.py).")
    parser.add_argument("--benchmark", type=str, default=None,
                        help="only this benchmark (default: every CSV).")
    parser.add_argument("--kind", choices=["data", "ptw", "both"], default="both",
                        help="data=Chip-to-chip-traced, ptw=Chip-to-chip-ptw-traced, "
                             "both=sum (default).")
    parser.add_argument("--linear", action="store_true",
                        help="linear y-axis (default: log).")
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

    print(f"[chiplet-recv] reports: {reports}")
    print(f"[chiplet-recv] output:  {out_dir}")
    print(f"[chiplet-recv] kind={args.kind}  "
          f"scale={'linear' if args.linear else 'log'}  files={len(csvs)}")

    for path in csvs:
        bench = path.stem
        out_png = os.path.join(out_dir, f"{bench}_{args.kind}_recv.png")
        _draw(bench, args.kind, path, out_png, log_scale=not args.linear)
        print(f"  saved {out_png}")


if __name__ == "__main__":
    main()
