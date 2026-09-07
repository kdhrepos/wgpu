"""
Plot the time-windowed local/remote + per-data_id ratios that
`local_remote_per_time.py` writes under _out/access-count/{date}/.

Per benchmark, a 2-panel figure sharing the x-axis (time window index 0,1,2,...):
  - top    : local vs remote access ratio (stacked area, sums to 1)
  - bottom : per-data_id access ratio     (stacked area; gray top band = unmatched)

Run `local_remote_per_time.py <run-id>` first so the CSVs exist.

    analyze/run.sh access-count/plot_local_remote_per_time.py mesh-6x6/latest
    analyze/run.sh access-count/plot_local_remote_per_time.py mesh-6x6/latest --benchmark lud
"""

import os
import sys
import glob
import argparse
import numpy as np
import pandas as pd

parent_dir = os.path.abspath(os.path.join(os.path.dirname(__file__), "../../"))
if parent_dir not in sys.path:
    sys.path.append(parent_dir)

import matplotlib
matplotlib.use("Agg")
import matplotlib.pyplot as plt

from _paths import run_root as _run_root, run_tag as _run_tag
from plot_style import (apply_paper_style, color_for, save_fig,
                        SIZE_DOUBLE_TALL)

username = os.path.expanduser('~')

_run_id = next((a for a in sys.argv[1:] if not a.startswith('-')), 'latest')

UNMATCHED_COLOR = "#bbbbbb"


def _window_ns(data_dir, fallback_starts):
    summary = os.path.join(data_dir, "local_remote_per_time_summary.csv")
    if os.path.exists(summary):
        s = pd.read_csv(summary)
        if "window_ns" in s.columns and len(s):
            return int(s["window_ns"].iloc[0])
    pos = fallback_starts[fallback_starts > 0]
    return int(np.gcd.reduce(pos.astype("int64"))) if len(pos) else 1


def _reindex_full(df, win_col, max_win):
    return df.set_index(win_col).reindex(range(max_win + 1))


def _plot_local_remote(ax, lr_df, w):
    lr_df = lr_df.copy()
    lr_df["win"] = (lr_df["start"] // w).astype(int)
    max_win = int(lr_df["win"].max())
    cols = ["win", "mean_local_ratio", "mean_remote_ratio", "std_remote_ratio"]
    full = _reindex_full(lr_df[cols], "win", max_win).fillna(0.0)
    x = full.index.to_numpy()
    local = full["mean_local_ratio"].to_numpy()
    remote = full["mean_remote_ratio"].to_numpy()
    std = full["std_remote_ratio"].to_numpy()

    ax.stackplot(x, local, remote,
                 labels=["local", "remote"],
                 colors=[color_for(0), color_for(1)], linewidth=0)
    # inter-chiplet spread on the local/remote boundary (boundary = mean_local)
    ax.fill_between(x, np.clip(local - std, 0, 1), np.clip(local + std, 0, 1),
                    color="white", alpha=0.35, linewidth=0, zorder=3)
    ax.plot(x, local, color="white", linewidth=0.6, zorder=4)
    ax.set_ylim(0, 1)
    ax.set_ylabel("mean access ratio")
    ax.set_title("local vs remote (chiplet-averaged, band = ±1σ)", loc="left")
    ax.legend(loc="upper right", ncol=2)
    return max_win


def _plot_data_id(ax, did_df, w, max_win):
    if did_df.empty:
        ax.set_title("per-data_id (no matched data_id rows)", loc="left")
        return
    did_df = did_df.copy()
    did_df["win"] = (did_df["start"] // w).astype(int)
    wide = did_df.pivot_table(index="win", columns="data_id", values="mean_ratio",
                              aggfunc="sum", fill_value=0.0)
    wide = wide.reindex(range(max_win + 1), fill_value=0.0)
    data_ids = sorted(wide.columns)
    x = wide.index.to_numpy()

    series = [wide[d].to_numpy() for d in data_ids]
    colors = [color_for(i) for i in range(len(data_ids))]
    labels = [f"id {int(d)}" for d in data_ids]

    unmatched = np.clip(1.0 - np.sum(series, axis=0), 0.0, 1.0)
    series.append(unmatched)
    colors.append(UNMATCHED_COLOR)
    labels.append("unmatched")

    ax.stackplot(x, *series, labels=labels, colors=colors, linewidth=0)
    ax.set_ylim(0, 1)
    ax.set_ylabel("mean access ratio")
    ax.set_xlabel("time window index")
    ax.set_title("per-data_id (chiplet-averaged share)", loc="left")
    ncol = min(len(labels), 6)
    ax.legend(loc="upper center", bbox_to_anchor=(0.5, -0.35), ncol=ncol)


def plot_benchmark(benchmark, data_dir, out_dir):
    lr_path = os.path.join(data_dir, f"{benchmark}_local_remote_per_time.csv")
    did_path = os.path.join(data_dir, f"{benchmark}_data_id_per_time.csv")
    if not os.path.exists(lr_path):
        return False

    lr_df = pd.read_csv(lr_path)
    if lr_df.empty:
        return False
    did_df = pd.read_csv(did_path) if os.path.exists(did_path) else pd.DataFrame()

    w = _window_ns(data_dir, lr_df["start"].to_numpy())

    fig, (ax_lr, ax_did) = plt.subplots(2, 1, figsize=SIZE_DOUBLE_TALL, sharex=True)
    max_win = _plot_local_remote(ax_lr, lr_df, w)
    _plot_data_id(ax_did, did_df, w, max_win)
    fig.suptitle(f"{benchmark}  (window = {w} ns)")

    save_fig(fig, out_dir, f"{benchmark}_local_remote_per_time")
    print(f"  {benchmark}: {max_win + 1} windows")
    return True


if __name__ == "__main__":
    parser = argparse.ArgumentParser(
        description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    parser.add_argument('run_id', nargs='?', default=_run_id,
                        help='run id (e.g. mesh-6x6/latest)')
    parser.add_argument('--benchmark', default=None,
                        help='only this benchmark (default: all with CSVs)')
    args = parser.parse_args()

    date = _run_tag(args.run_id)
    data_dir = f"{username}/mgvm/analyze/_out/access-count/{date}"
    out_dir = f"{data_dir}/figs"

    if not os.path.isdir(data_dir):
        print(f"No data dir {data_dir}; run local_remote_per_time.py {args.run_id} first.")
        sys.exit(1)

    apply_paper_style()

    if args.benchmark:
        benches = [args.benchmark]
    else:
        benches = sorted(os.path.basename(p).replace("_local_remote_per_time.csv", "")
                         for p in glob.glob(f"{data_dir}/*_local_remote_per_time.csv"))

    if not benches:
        print(f"No *_local_remote_per_time.csv under {data_dir}; "
              f"run local_remote_per_time.py {args.run_id} first.")
        sys.exit(1)

    print(f"Plotting {len(benches)} benchmark(s) -> {out_dir}")
    n = 0
    for b in benches:
        if plot_benchmark(b, data_dir, out_dir):
            n += 1
    print(f"\nSaved {n} figure(s) under {out_dir}")
