"""(고리 3a) 접근 횟수 쏠림 — 로렌츠 곡선.

X축 = 캐시라인 누적 비율(접근 횟수 내림차순으로 정렬), Y축 = 접근 누적 비율.
대각선이 "고른 분포" 기준선이고, 위로 휠수록 쏠림이 강함. 상위 10% 라인이
전체 접근의 몇 %를 차지하는지 주석으로 표시.

입력
----
  _out/address/address_access_stats/<run-id>/<bench>_address_stats.csv
  (없으면 `analyze/run.sh address/address_access_stats.py <date>` 먼저)

출력
----
  _out/access-count/access-skew/<run_tag>/all.{png,pdf}
  + 각 벤치 한 장 (_out/access-count/access-skew/<run_tag>/<bench>.{png,pdf})
"""

from __future__ import annotations

import argparse
import sys
from pathlib import Path

import numpy as np
import pandas as pd
import matplotlib.pyplot as plt

from _paths import address_stats_dir, domain_out_dir
from plot_style import apply_paper_style, color_for, save_fig, SIZE_SINGLE, SIZE_DOUBLE
from params import benchmarks


def lorenz(counts: np.ndarray) -> tuple[np.ndarray, np.ndarray, float]:
    """(line_cdf, access_cdf, top10_share)."""
    counts = np.sort(counts)[::-1]            # 내림차순
    n = len(counts)
    total = counts.sum()
    if n == 0 or total == 0:
        return np.zeros(1), np.zeros(1), 0.0
    line_cdf = np.arange(1, n + 1) / n
    access_cdf = np.cumsum(counts) / total
    k = max(1, int(np.ceil(0.10 * n)))
    top10 = float(counts[:k].sum() / total)
    return line_cdf, access_cdf, top10


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

    out = domain_out_dir("access-count", "access-skew", args.date)
    apply_paper_style()

    fig_all, ax_all = plt.subplots(figsize=SIZE_SINGLE)
    ax_all.plot([0, 1], [0, 1], color="#888", linestyle="--",
                linewidth=0.6, label="고른 분포 기준선")

    summary_rows = []
    drawn = 0
    for i, b in enumerate(benchmarks):
        f = sdir / f"{b}_address_stats.csv"
        if not f.exists():
            continue
        try:
            df = pd.read_csv(f, usecols=["access_count"])
        except Exception as e:
            print(f"  skip {b}: {e}")
            continue
        counts = df["access_count"].to_numpy()
        if len(counts) == 0:
            continue
        line_cdf, access_cdf, top10 = lorenz(counts)
        col = color_for(drawn)

        ax_all.plot(line_cdf, access_cdf, color=col, linewidth=1.0,
                    label=f"{b} (top10%={top10*100:.0f}%)", alpha=0.85)

        # per-bench
        fig_b, ax_b = plt.subplots(figsize=SIZE_SINGLE)
        ax_b.plot([0, 1], [0, 1], color="#888", linestyle="--", linewidth=0.6)
        ax_b.plot(line_cdf, access_cdf, color=col, linewidth=1.4)
        ax_b.set_xlabel("캐시라인 누적 비율")
        ax_b.set_ylabel("접근 횟수 누적 비율")
        ax_b.set_title(f"{b}: 접근 쏠림 (상위 10% 라인 → 전체의 {top10*100:.1f}%)")
        ax_b.set_xlim(0, 1); ax_b.set_ylim(0, 1.02)
        ax_b.grid(alpha=0.2)
        save_fig(fig_b, out, b)

        summary_rows.append(dict(benchmark=b, n_lines=len(counts),
                                 top10_share=top10,
                                 top1_share=float(counts[:max(1, len(counts)//100)].sum() / counts.sum())))
        drawn += 1

    if drawn == 0:
        print("no bench stats found.", file=sys.stderr)
        sys.exit(1)

    ax_all.set_xlabel("캐시라인 누적 비율 (접근 횟수 내림차순)")
    ax_all.set_ylabel("접근 횟수 누적 비율")
    ax_all.set_title("접근 쏠림 로렌츠 곡선")
    ax_all.set_xlim(0, 1); ax_all.set_ylim(0, 1.02)
    ax_all.legend(fontsize=5, frameon=False, loc="lower right", ncol=2)
    ax_all.grid(alpha=0.2)
    save_fig(fig_all, out, "all")

    pd.DataFrame(summary_rows).to_csv(out / "skew_summary.csv", index=False)
    print(f"wrote {out}/all.{{png,pdf}} + per-bench + skew_summary.csv")


if __name__ == "__main__":
    main()
