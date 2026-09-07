"""(고리 3b) 공유도(sharing degree) 분포 — 라인 비중 vs 접근 비중.

캐시라인의 sharing_degree 를 다섯 buckets {1, 2-4, 5-8, 9-16, 17-36} 로 묶고
벤치마크별로 (좌) 라인 수 비중 / (우) 접근 수 비중 을 grouped bar 로.
중간 bin(2-4 / 5-8 / 9-16) 은 sweet spot 으로 강조 — NearFetch 같은 좁은
공유나 강한 broadcast 양극단이 아니라 "여러 칩렛이 적당히 공유" 구간이
도드라지는지 보기 위함.

입력
----
  _out/address/address_access_stats/<run-id>/<bench>_sharing_dist.csv

출력
----
  _out/address/sharing-degree/<run_tag>/all.{png,pdf}
"""

from __future__ import annotations

import argparse
import sys

import numpy as np
import pandas as pd
import matplotlib.pyplot as plt

from _paths import address_stats_dir, domain_out_dir
from plot_style import apply_paper_style, color_for, save_fig, SIZE_DOUBLE
from params import benchmarks


BIN_EDGES = [(1, 1), (2, 4), (5, 8), (9, 16), (17, 36)]
BIN_LABELS = ["1", "2-4", "5-8", "9-16", "17-36"]
SWEET_SPOT = {"2-4", "5-8", "9-16"}


def bucketize(df: pd.DataFrame) -> pd.DataFrame:
    """sharing_dist.csv 한 벤치 → 5개 bucket 별 (n_addrs, total_access) 합산."""
    rows = []
    for label, (lo, hi) in zip(BIN_LABELS, BIN_EDGES):
        sel = df[(df["sharing_degree"] >= lo) & (df["sharing_degree"] <= hi)]
        rows.append(dict(
            bucket=label,
            n_addrs=int(sel["n_addrs"].sum()) if not sel.empty else 0,
            total_access=int(sel["total_access"].sum()) if not sel.empty else 0,
        ))
    return pd.DataFrame(rows)


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--date", default="mesh-6x6/latest")
    args = ap.parse_args()

    sdir = address_stats_dir(args.date)
    if not sdir.exists():
        print(f"no input dir: {sdir}\n"
              f"  먼저 'analyze/run.sh address/address_access_stats.py {args.date}' 실행.",
              file=sys.stderr)
        sys.exit(1)

    bench_buckets: dict[str, pd.DataFrame] = {}
    for b in benchmarks:
        f = sdir / f"{b}_sharing_dist.csv"
        if not f.exists():
            continue
        try:
            df = pd.read_csv(f)
        except Exception as e:
            print(f"  skip {b}: {e}")
            continue
        if {"sharing_degree", "n_addrs", "total_access"} - set(df.columns):
            print(f"  skip {b}: unexpected columns {df.columns.tolist()}")
            continue
        bench_buckets[b] = bucketize(df)

    if not bench_buckets:
        print("no bench sharing dist found.", file=sys.stderr)
        sys.exit(1)

    benches = list(bench_buckets.keys())
    n_b = len(benches)
    n_bk = len(BIN_LABELS)

    addrs_pct = np.zeros((n_b, n_bk))
    access_pct = np.zeros((n_b, n_bk))
    for i, b in enumerate(benches):
        bb = bench_buckets[b]
        ta = bb["n_addrs"].sum() or 1
        tc = bb["total_access"].sum() or 1
        addrs_pct[i] = bb["n_addrs"].to_numpy() / ta * 100.0
        access_pct[i] = bb["total_access"].to_numpy() / tc * 100.0

    apply_paper_style()
    fig, (ax_a, ax_c) = plt.subplots(1, 2, figsize=SIZE_DOUBLE)

    x = np.arange(n_b)
    bw = 0.16
    for k, label in enumerate(BIN_LABELS):
        col = color_for(k)
        edge = "black" if label in SWEET_SPOT else "none"
        lw = 0.6 if label in SWEET_SPOT else 0
        ax_a.bar(x + (k - n_bk/2)*bw + bw/2, addrs_pct[:, k], bw,
                 color=col, edgecolor=edge, linewidth=lw, label=label)
        ax_c.bar(x + (k - n_bk/2)*bw + bw/2, access_pct[:, k], bw,
                 color=col, edgecolor=edge, linewidth=lw)

    for ax, title, ylab in [(ax_a, "라인 수 분포", "라인 비중 (%)"),
                            (ax_c, "접근 수 분포", "접근 비중 (%)")]:
        ax.set_xticks(x)
        ax.set_xticklabels(benches, rotation=40, ha="right", fontsize=6)
        ax.set_ylabel(ylab)
        ax.set_title(f"공유도 buckets — {title}")
        ax.grid(axis="y", alpha=0.2)

    ax_a.legend(title="공유도 (칩렛 수)", ncol=5, frameon=False, fontsize=6,
                loc="upper left", bbox_to_anchor=(0, 1.18))

    out = domain_out_dir("address", "sharing-degree", args.date)
    save_fig(fig, out, "all")
    pd.DataFrame(addrs_pct, index=benches, columns=BIN_LABELS).to_csv(out / "addrs_pct.csv")
    pd.DataFrame(access_pct, index=benches, columns=BIN_LABELS).to_csv(out / "access_pct.csv")
    print(f"wrote {out}/all.{{png,pdf}} + addrs_pct.csv + access_pct.csv")


if __name__ == "__main__":
    main()
