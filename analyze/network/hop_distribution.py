"""(고리 1b) 칩렛 간 트래픽의 hop 분포 — 평균 거리/누적 분포.

리포트에 per-hop count 메트릭이 따로 없어, `Chip-to-chip-traced` 트래픽 행렬에서
Manhattan 거리별 트랜잭션 수를 직접 집계한다(데이터 평면, PTW 제외).

좌: hop 누적 분포(CDF), 우: 평균 hop 막대.

입력: reports/<bench>_<config>.csv
출력: _out/network/hop-distribution/<run_tag>/all.{png,pdf}
"""

from __future__ import annotations

import argparse
import sys

import numpy as np
import pandas as pd
import matplotlib.pyplot as plt

from _paths import report_dir, domain_out_dir
from plot_style import apply_paper_style, color_for, save_fig, SIZE_DOUBLE
from motivation_common import (load_report, traffic_matrix, manhattan_matrix,
                               NUM_CHIPLETS_DEFAULT, file_suffix_for_config)
from params import benchmarks


def hop_histogram(mat: np.ndarray, dmat: np.ndarray) -> np.ndarray:
    """hop별 트래픽 합 — index = hop, value = count."""
    flat_d = dmat.ravel().astype(int)
    flat_v = mat.ravel()
    max_h = int(flat_d.max())
    out = np.zeros(max_h + 1, dtype=np.float64)
    # bincount: weights 로 합산
    out = np.bincount(flat_d, weights=flat_v, minlength=max_h + 1)
    return out


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--date", default="mesh-6x6/latest")
    args = ap.parse_args()

    rd = report_dir(args.date)
    config_name = rd.parent.parent.name
    suffix = file_suffix_for_config(config_name)
    dmat = manhattan_matrix()

    bench_hist: dict[str, np.ndarray] = {}
    bench_mean: dict[str, float] = {}

    for b in benchmarks:
        f = rd / f"{b}_{suffix}.csv"
        if not f.exists():
            continue
        rep = load_report(f)
        mat = traffic_matrix(rep, "Chip-to-chip-traced", NUM_CHIPLETS_DEFAULT)
        total = mat.sum()
        if total <= 0:
            continue
        h = hop_histogram(mat, dmat)
        bench_hist[b] = h / h.sum()
        bench_mean[b] = float(np.average(np.arange(len(h)), weights=h))

    if not bench_hist:
        print("no usable benchmarks.", file=sys.stderr)
        sys.exit(1)

    max_len = max(len(h) for h in bench_hist.values())
    apply_paper_style()
    fig, (ax_cdf, ax_mean) = plt.subplots(1, 2, figsize=SIZE_DOUBLE,
                                          gridspec_kw={"width_ratios": [1.4, 1]})

    # CDF
    for i, (b, h) in enumerate(bench_hist.items()):
        # zero-pad to max_len
        padded = np.zeros(max_len)
        padded[:len(h)] = h
        cdf = np.cumsum(padded)
        ax_cdf.plot(np.arange(max_len), cdf, color=color_for(i),
                    label=b, linewidth=1.0, alpha=0.85)
    ax_cdf.set_xlabel("Hop 수 (Manhattan 거리)")
    ax_cdf.set_ylabel("누적 트래픽 비중")
    ax_cdf.set_title("Hop 누적 분포")
    ax_cdf.set_ylim(0, 1.02)
    ax_cdf.grid(alpha=0.2)
    if len(bench_hist) <= 12:
        ax_cdf.legend(fontsize=6, frameon=False, loc="lower right", ncol=2)

    # mean hop bar
    means = pd.Series(bench_mean).sort_values()
    ax_mean.barh(np.arange(len(means)), means.values,
                 color=[color_for(i) for i in range(len(means))])
    ax_mean.set_yticks(np.arange(len(means)))
    ax_mean.set_yticklabels(means.index, fontsize=6)
    ax_mean.set_xlabel("평균 hop")
    ax_mean.set_title("평균 hop (벤치별)")
    ax_mean.grid(axis="x", alpha=0.2)

    out = domain_out_dir("network", "hop-distribution", args.date)
    save_fig(fig, out, "all")
    pd.DataFrame({b: pd.Series(h) for b, h in bench_hist.items()}).to_csv(
        out / "hop_pmf.csv")
    means.to_csv(out / "mean_hop.csv", header=["mean_hop"])
    print(f"wrote {out}/all.{{png,pdf}} + hop_pmf.csv + mean_hop.csv")


if __name__ == "__main__":
    main()
