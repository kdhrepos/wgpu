"""(고리 2b) 메모리 트레이스 직접 소비 — L2 미스 접근의 요청↔홈 Manhattan 거리 분포.

L2 miss(type ∈ {2,5}) 행만 골라 (chiplet=요청, owner=홈) 사이 hop 수를 카운트한다.
좌: 거리 누적 분포, 우: 평균 거리 막대.

큰 트레이스 대비 chunk 단위로 읽으며 hop별 누적 카운트만 유지(메모리 ~수 KB).

입력
----
  memory-traces/<bench>_<config>_memory_trace.csv

출력
----
  _out/network/distance-histogram/<run_tag>/all.{png,pdf}
"""

from __future__ import annotations

import argparse
import sys
from pathlib import Path

import numpy as np
import pandas as pd
import matplotlib.pyplot as plt

from _paths import memory_trace_dir, run_root, domain_out_dir
from plot_style import apply_paper_style, color_for, save_fig, SIZE_DOUBLE
from motivation_common import (trace_chunks, filter_l2miss, manhattan_matrix,
                               NUM_CHIPLETS_DEFAULT, file_suffix_for_config,
                               ALL_TRACE_COLS)
from params import benchmarks


def collect_hops(path: Path) -> np.ndarray:
    """hop별 누적 카운트."""
    dmat = manhattan_matrix()
    max_hop = int(dmat.max())
    hist = np.zeros(max_hop + 1, dtype=np.int64)

    for chunk in trace_chunks(path, usecols=ALL_TRACE_COLS):
        chunk = chunk.drop_duplicates()          # full-row 완전-중복 제거
        chunk = filter_l2miss(chunk)
        if chunk.empty:
            continue
        c = chunk["chiplet"].to_numpy()
        o = chunk["owner"].to_numpy()
        ok = (c >= 0) & (c < NUM_CHIPLETS_DEFAULT) & (o >= 0) & (o < NUM_CHIPLETS_DEFAULT)
        hops = dmat[c[ok], o[ok]]
        hist += np.bincount(hops, minlength=max_hop + 1)
    return hist


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--date", default="mesh-6x6/latest")
    args = ap.parse_args()

    td = memory_trace_dir(args.date)
    config_name = td.parent.parent.name
    suffix = file_suffix_for_config(config_name)

    bench_hist: dict[str, np.ndarray] = {}
    bench_mean: dict[str, float] = {}

    for b in benchmarks:
        f = td / f"{b}_{suffix}_memory_trace.csv"
        if not f.exists():
            continue
        print(f"  {b} ...", flush=True)
        try:
            h = collect_hops(f)
        except Exception as e:
            print(f"    skip {b}: {e}")
            continue
        if h.sum() == 0:
            continue
        bench_hist[b] = h / h.sum()
        bench_mean[b] = float(np.average(np.arange(len(h)), weights=h))

    if not bench_hist:
        print("no usable benchmarks.", file=sys.stderr)
        sys.exit(1)

    max_len = max(len(h) for h in bench_hist.values())

    apply_paper_style()
    fig, (ax_cdf, ax_mean) = plt.subplots(1, 2, figsize=SIZE_DOUBLE,
                                          gridspec_kw={"width_ratios": [1.4, 1]})

    for i, (b, h) in enumerate(bench_hist.items()):
        padded = np.zeros(max_len)
        padded[:len(h)] = h
        ax_cdf.plot(np.arange(max_len), np.cumsum(padded),
                    color=color_for(i), label=b, linewidth=1.0, alpha=0.85)
    ax_cdf.set_xlabel("요청 칩렛 ↔ 홈 칩렛 Manhattan 거리")
    ax_cdf.set_ylabel("누적 접근 비중")
    ax_cdf.set_title("L2 미스 거리 누적 분포")
    ax_cdf.set_ylim(0, 1.02)
    ax_cdf.grid(alpha=0.2)
    if len(bench_hist) <= 12:
        ax_cdf.legend(fontsize=6, frameon=False, loc="lower right", ncol=2)

    means = pd.Series(bench_mean).sort_values()
    ax_mean.barh(np.arange(len(means)), means.values,
                 color=[color_for(i) for i in range(len(means))])
    ax_mean.set_yticks(np.arange(len(means)))
    ax_mean.set_yticklabels(means.index, fontsize=6)
    ax_mean.set_xlabel("평균 거리 (hop)")
    ax_mean.set_title("평균 거리 (벤치별)")
    ax_mean.grid(axis="x", alpha=0.2)

    out = domain_out_dir("network", "distance-histogram", args.date)
    save_fig(fig, out, "all")
    pd.DataFrame({b: pd.Series(h) for b, h in bench_hist.items()}).to_csv(
        out / "distance_pmf.csv")
    means.to_csv(out / "mean_distance.csv", header=["mean_distance"])
    print(f"wrote {out}/all.{{png,pdf}}")


if __name__ == "__main__":
    main()
