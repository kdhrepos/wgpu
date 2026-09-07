"""각 칩렛(GPM) PC table의 HLL(WSAddrs footprint) 추정 일관성 분석.

입력: results/<config>/<ts>/pc-footprint/<bench>_<config>_pc_footprint.csv
컬럼: cycle, sim_time, chiplet_id, pc, hll_footprint, hotness, req_count
    -> 매 dump cycle마다 각 칩렛의 PC table 스냅샷 (한 PC entry = 한 행).

질문: pctable-windowed-classifier에서, 서로 다른 GPM이 관측한 같은 PC의
      HLL footprint 추정값이 얼마나 일치하는가?

핵심 지표 (per (bench, cycle, pc), 그 PC를 관측한 칩렛들만 대상):
  n_chiplets           = 이 (cycle,pc)를 담고 있는 칩렛 수
  hll_mean/median/min/max/std   = HLL 추정 분포
  cv = std / mean      = 변동계수 (일관성 지표, 0=완벽 일치)
  spread_ratio = max/min (min>0)  = 최악/최선 배율
  total_req_count      = 그 (cycle,pc) 관측 칩렛들의 req_count 합
  hotness_modal        = 다수결 hotness 라벨
  hotness_agreement    = modal 표수 / n_chiplets  (1.0=만장일치)

집계:
  per_benchmark_summary.csv   벤치마크별 median/p90 CV, 만장일치 비율 등
  <bench>_hll_consistency.csv (cycle,pc) 단위 상세
  _all_hll_consistency.csv    전 벤치마크 concat

그림 (~/mgvm/figures/pc-hll-consistency/<config>-<tag>/):
  cv_cdf.png                    벤치마크별 CV CDF (req_count 가중)
  cv_by_benchmark_box.png       벤치마크별 CV 박스플롯
  cv_vs_reqcount.png            req_count 합 vs CV (hot PC는 일관성 높은지)
  hotness_agreement.png         벤치마크별 hotness 일치도 CDF

사용:
  analyze/run.sh pc-stats/pc_hll_consistency.py <run-id>
  analyze/run.sh pc-stats/pc_hll_consistency.py <run-id> -j 8
  analyze/run.sh pc-stats/pc_hll_consistency.py <run-id> --min-chiplets 4
"""

from __future__ import annotations

import argparse
import os
from concurrent.futures import ProcessPoolExecutor
from itertools import repeat
from pathlib import Path

import numpy as np
import pandas as pd
import matplotlib.pyplot as plt

from _paths import pc_footprint_dir, run_config, run_tag
from plot_style import apply_paper_style, color_for, save_fig, SIZE_SINGLE, SIZE_DOUBLE
from params import benchmarks

USECOLS = ['cycle', 'chiplet_id', 'pc', 'hll_footprint', 'hotness', 'req_count']
DTYPES = {
    'cycle': 'int64',
    'chiplet_id': 'int32',
    'pc': 'uint64',
    'hll_footprint': 'int64',
    'hotness': 'int8',
    'req_count': 'int64',
}


def _consistency_for_group(g: pd.DataFrame) -> pd.Series:
    hll = g['hll_footprint'].to_numpy(dtype=np.float64)
    rc = g['req_count'].to_numpy(dtype=np.int64)
    hot = g['hotness'].to_numpy(dtype=np.int8)
    n = hll.size
    mean = hll.mean()
    std = hll.std(ddof=0)
    hmin = hll.min()
    hmax = hll.max()
    hmed = np.median(hll)
    cv = float(std / mean) if mean > 0 else np.nan
    spread = float(hmax / hmin) if hmin > 0 else np.nan

    vals, counts = np.unique(hot, return_counts=True)
    modal_ix = counts.argmax()
    modal = int(vals[modal_ix])
    agree = float(counts[modal_ix]) / n

    return pd.Series({
        'n_chiplets': n,
        'hll_mean': mean,
        'hll_median': hmed,
        'hll_min': int(hmin),
        'hll_max': int(hmax),
        'hll_std': std,
        'cv': cv,
        'spread_ratio': spread,
        'total_req_count': int(rc.sum()),
        'mean_req_count': float(rc.mean()),
        'hotness_modal': modal,
        'hotness_agreement': agree,
    })


def process_benchmark(bench: str, pcf_dir: str, suffix: str, out_dir: str,
                      min_chiplets: int):
    src = Path(pcf_dir) / f"{bench}{suffix}_pc_footprint.csv"
    if not src.exists():
        return f"[{bench}] missing {src.name}"
    try:
        df = pd.read_csv(src, usecols=USECOLS, dtype=DTYPES)
        if df.empty:
            return f"[{bench}] empty"

        # (cycle, pc) 단위 그룹핑. 칩렛 하나만 관측한 항목은 min_chiplets 로 필터.
        grouped = df.groupby(['cycle', 'pc'], sort=False)
        stat = grouped.apply(_consistency_for_group).reset_index()
        stat = stat[stat['n_chiplets'] >= min_chiplets].copy()
        if stat.empty:
            return f"[{bench}] no (cycle,pc) with n_chiplets>={min_chiplets}"

        stat['benchmark'] = bench
        stat['pc_hex'] = stat['pc'].apply(lambda v: f"0x{int(v):016x}")
        stat = stat.sort_values(['cycle', 'total_req_count'],
                                ascending=[True, False])
        cols = ['benchmark', 'cycle', 'pc_hex', 'pc',
                'n_chiplets', 'hll_mean', 'hll_median', 'hll_min', 'hll_max',
                'hll_std', 'cv', 'spread_ratio',
                'total_req_count', 'mean_req_count',
                'hotness_modal', 'hotness_agreement']
        stat = stat[cols]

        out = Path(out_dir) / f"{bench}_hll_consistency.csv"
        stat.to_csv(out, index=False)

        cv_med = float(stat['cv'].median(skipna=True))
        cv_p90 = float(stat['cv'].quantile(0.90))
        agree_med = float(stat['hotness_agreement'].median())
        return (f"[{bench}] {len(stat):,} (cycle,pc) samples "
                f"cv med={cv_med:.3f} p90={cv_p90:.3f} "
                f"hotness_agree med={agree_med:.3f} -> {out.name}")
    except Exception as e:
        import traceback
        traceback.print_exc()
        return f"[{bench}] ERROR {e}"


# ── 그림 -----------------------------------------------------------------
def _weighted_cdf(vals: np.ndarray, w: np.ndarray):
    order = np.argsort(vals)
    v = vals[order]
    wt = w[order].astype(np.float64)
    total = wt.sum()
    if total == 0:
        return v, np.zeros_like(v, dtype=np.float64)
    return v, wt.cumsum() / total


def plot_cv_cdf(all_df: pd.DataFrame, out_dir: str):
    apply_paper_style()
    fig, ax = plt.subplots(figsize=SIZE_SINGLE)
    for i, b in enumerate(sorted(all_df['benchmark'].unique())):
        sub = all_df[(all_df['benchmark'] == b) & all_df['cv'].notna()]
        if sub.empty:
            continue
        x, y = _weighted_cdf(sub['cv'].to_numpy(),
                             sub['total_req_count'].to_numpy())
        ax.plot(x, y, label=b, color=color_for(i), linewidth=1.4)
    ax.set_xlabel("CV of HLL footprint across GPMs (per (cycle, pc))")
    ax.set_ylabel("CDF (weighted by req_count)")
    ax.set_xlim(0, min(2.0, all_df['cv'].quantile(0.99) if all_df['cv'].notna().any() else 2.0))
    ax.set_ylim(0, 1.0)
    ax.grid(True, alpha=0.3)
    ax.legend(fontsize=6, ncol=2, loc='lower right')
    save_fig(fig, out_dir, "cv_cdf")


def plot_cv_box(all_df: pd.DataFrame, out_dir: str):
    apply_paper_style()
    fig, ax = plt.subplots(figsize=SIZE_DOUBLE)
    order = sorted(all_df['benchmark'].unique())
    data = [all_df.loc[(all_df['benchmark'] == b) & all_df['cv'].notna(), 'cv']
                .clip(upper=2.0).to_numpy()
            for b in order]
    ax.boxplot(data, labels=order, showfliers=False)
    ax.set_ylabel("CV of HLL footprint (clipped to 2.0)")
    ax.set_xticklabels(order, rotation=30, ha='right', fontsize=7)
    ax.grid(True, axis='y', alpha=0.3)
    save_fig(fig, out_dir, "cv_by_benchmark_box")


def plot_cv_vs_reqcount(all_df: pd.DataFrame, out_dir: str):
    apply_paper_style()
    fig, ax = plt.subplots(figsize=SIZE_SINGLE)
    sub = all_df[all_df['cv'].notna() & (all_df['total_req_count'] > 0)]
    if not sub.empty:
        ax.scatter(sub['total_req_count'], sub['cv'],
                   s=2, alpha=0.15, c=color_for(0), rasterized=True)
    ax.set_xscale('log')
    ax.set_xlabel("total req_count across GPMs (per (cycle, pc))")
    ax.set_ylabel("CV of HLL footprint")
    ax.set_ylim(0, 2.0)
    ax.grid(True, alpha=0.3)
    save_fig(fig, out_dir, "cv_vs_reqcount")


def plot_hotness_agreement_cdf(all_df: pd.DataFrame, out_dir: str):
    apply_paper_style()
    fig, ax = plt.subplots(figsize=SIZE_SINGLE)
    for i, b in enumerate(sorted(all_df['benchmark'].unique())):
        sub = all_df[all_df['benchmark'] == b]
        if sub.empty:
            continue
        x, y = _weighted_cdf(sub['hotness_agreement'].to_numpy(),
                             sub['total_req_count'].to_numpy())
        ax.plot(x, y, label=b, color=color_for(i), linewidth=1.4)
    ax.set_xlabel("fraction of GPMs agreeing on modal hotness")
    ax.set_ylabel("CDF (weighted by req_count)")
    ax.set_xlim(0, 1.0)
    ax.set_ylim(0, 1.0)
    ax.grid(True, alpha=0.3)
    ax.legend(fontsize=6, ncol=2, loc='upper left')
    save_fig(fig, out_dir, "hotness_agreement")


def _summarize(all_df: pd.DataFrame, out_dir: str):
    rows = []
    for b, g in all_df.groupby('benchmark'):
        cv = g['cv'].dropna()
        w = g.loc[cv.index, 'total_req_count'].to_numpy(dtype=np.float64)
        v = cv.to_numpy()
        if v.size == 0 or w.sum() == 0:
            wmed = wp90 = np.nan
        else:
            order = np.argsort(v)
            vv = v[order]; ww = w[order]
            cum = ww.cumsum() / ww.sum()
            wmed = float(vv[np.searchsorted(cum, 0.5)])
            wp90 = float(vv[np.searchsorted(cum, 0.9)])
        rows.append({
            'benchmark': b,
            'n_samples': int(len(g)),
            'cv_median': float(cv.median()) if len(cv) else np.nan,
            'cv_p90': float(cv.quantile(0.90)) if len(cv) else np.nan,
            'cv_weighted_median': wmed,
            'cv_weighted_p90': wp90,
            'frac_cv_lt_0.1': float((cv < 0.1).mean()) if len(cv) else np.nan,
            'frac_cv_lt_0.3': float((cv < 0.3).mean()) if len(cv) else np.nan,
            'hotness_agree_median': float(g['hotness_agreement'].median()),
            'frac_hotness_unanimous': float((g['hotness_agreement'] == 1.0).mean()),
            'mean_n_chiplets': float(g['n_chiplets'].mean()),
        })
    summary = pd.DataFrame(rows).sort_values('cv_median')
    p = Path(out_dir) / "per_benchmark_summary.csv"
    summary.to_csv(p, index=False)
    print(f"\nSummary -> {p}")
    with pd.option_context('display.max_columns', None, 'display.width', 200,
                           'display.float_format', lambda v: f"{v:.3f}"):
        print(summary.to_string(index=False))


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument('run_id', nargs='?', default='dynamic-clustering-6x6-score/latest')
    ap.add_argument('-j', '--workers', type=int, default=8)
    ap.add_argument('--min-chiplets', type=int, default=2,
                    help='최소 관측 GPM 수 (기본 2, CV 정의 위해)')
    ap.add_argument('--no-figs', action='store_true')
    args = ap.parse_args()

    pcf_dir = str(pc_footprint_dir(args.run_id))
    config = run_config(args.run_id)
    tag = run_tag(args.run_id)
    # 리포트 파일명 접미어 = _<config-normalized>. 파일 실물에서 확인.
    # 예: bptree_dynamic-clustering_6x6_pc_footprint.csv 이므로
    # suffix = "_dynamic-clustering_6x6". config에서 <platform>-<WxH> → <platform>_<W>x<H> 로 변환.
    parts = config.split('-')
    # config = e.g. dynamic-clustering-6x6-score
    #        = <platform tokens>-<WxH>-[modifiers]
    # 파일 접미어는 <platform>_<WxH> 만 씀. 첫 W×H 매칭 토큰 찾기.
    dim_ix = None
    for i, p in enumerate(parts):
        if 'x' in p and all(c.isdigit() or c == 'x' for c in p):
            dim_ix = i
            break
    if dim_ix is None:
        raise SystemExit(f"config {config} 에서 W×H 토큰을 찾을 수 없음")
    platform = '-'.join(parts[:dim_ix])
    wh = parts[dim_ix]
    suffix = f"_{platform}_{wh}"

    out_dir = os.path.abspath(
        os.path.expanduser(f"~/mgvm/figures/pc-hll-consistency/{config}-{tag}"))
    os.makedirs(out_dir, exist_ok=True)

    print(f"Src   : {pcf_dir}")
    print(f"Config: {config}  suffix={suffix}")
    print(f"Out   : {out_dir}")
    print(f"Min GPMs: {args.min_chiplets}   Workers: {args.workers}")

    if args.workers <= 1:
        results = [process_benchmark(b, pcf_dir, suffix, out_dir, args.min_chiplets)
                   for b in benchmarks]
    else:
        with ProcessPoolExecutor(max_workers=args.workers) as ex:
            results = list(ex.map(process_benchmark, benchmarks,
                                  repeat(pcf_dir), repeat(suffix),
                                  repeat(out_dir), repeat(args.min_chiplets)))
    for r in results:
        print(r, flush=True)

    dfs = []
    for b in benchmarks:
        f = Path(out_dir) / f"{b}_hll_consistency.csv"
        if f.exists():
            dfs.append(pd.read_csv(f))
    if not dfs:
        print("no per-benchmark output; skip figures/summary")
        return
    all_df = pd.concat(dfs, ignore_index=True)
    out_all = Path(out_dir) / "_all_hll_consistency.csv"
    all_df.to_csv(out_all, index=False)
    print(f"\nAll -> {out_all} ({all_df['benchmark'].nunique()} benchmarks, "
          f"{len(all_df):,} rows)")

    _summarize(all_df, out_dir)

    if args.no_figs:
        return
    plot_cv_cdf(all_df, out_dir)
    plot_cv_box(all_df, out_dir)
    plot_cv_vs_reqcount(all_df, out_dir)
    plot_hotness_agreement_cdf(all_df, out_dir)
    print(f"figures -> {out_dir}")


if __name__ == '__main__':
    main()
