"""Time-window별 캐시 라인 공유 정도 분포.

질문: "시간이 지남에 따라 각 캐시 라인은 몇 개의 칩렛에서 접근되는가?"

방법:
  1. demand access (type 0~5, is_ptw=0) 만 사용
  2. 실행 시간을 --window-size 사이클 크기의 구간으로 분할 (1GHz 가정)
  3. 각 구간 내에서 캐시 라인별 고유 chiplet 수 집계
  4. 공유 정도 구간별 비율 집계 → stacked 100% bar 그래프

공유 구간 (36 chiplet 기준):
  1          = private
  2~3        = lightly shared
  4~9        = moderately shared
  10~18      = heavily shared (~quarter~half)
  19+        = very widely shared (> half)

출력:
  - _out/cacheline/<run-id>/<benchmark>_sharing_over_time.png
  - _out/cacheline/<run-id>/cacheline_sharing_over_time.xlsx

사용:
  analyze/run.sh cacheline/cacheline_sharing_over_time.py mesh-6x6/20260624-002942
  analyze/run.sh cacheline/cacheline_sharing_over_time.py latest --window-size 500000
"""

import argparse
import math
import os
import sys

import numpy as np
import pandas as pd
import matplotlib.pyplot as plt

parent_dir = os.path.abspath(os.path.join(os.path.dirname(__file__), "../../"))
if parent_dir not in sys.path:
    sys.path.insert(0, parent_dir)
scripts_dir = os.path.abspath(os.path.join(os.path.dirname(__file__), "../../../scripts"))
if scripts_dir not in sys.path:
    sys.path.insert(0, scripts_dir)

from concurrent.futures import ProcessPoolExecutor
from itertools import repeat
from params import benchmarks
from _paths import run_root, memory_trace_dir, run_tag

DEMAND_TYPES  = frozenset({0, 1, 2, 3, 4, 5})
CLOCK_HZ      = 1e9   # 1 GHz → 1 cycle = 1e-9 s

_FILTER_CHOICES = ["all", "rdma-only", "local-only", "rdma-miss-only"]

def _extra_filter_cols(f):
    if f in ("rdma-only", "local-only"):
        return ["owner"]
    if f == "rdma-miss-only":
        return ["owner", "rdma"]
    return []

def _apply_filter(df, f):
    if f == "rdma-only":
        return df[df["chiplet"] != df["owner"]]
    if f == "local-only":
        return df[df["chiplet"] == df["owner"]]
    if f == "rdma-miss-only":
        return df[(df["chiplet"] != df["owner"]) & (df["rdma"] == True)]
    return df

# 공유 구간 정의 (right-inclusive)
BIN_EDGES  = [0, 1, 3, 9, 18, 36]
BIN_LABELS = ['1', '2~3', '4~9', '10~18', '19+']
# 초록(private) → 빨강(wide-share) 5단계
BIN_COLORS = ['#2ca02c', '#78c679', '#fdae61', '#f46d43', '#d73027']


def process_benchmark(benchmark, args):
    trace_file = os.path.join(
        args.trace_dir,
        f"{benchmark}_mesh_{args.xDim}x{args.yDim}_memory_trace.csv",
    )
    if not os.path.exists(trace_file):
        return None

    print(f"[{benchmark}] reading ...", flush=True)
    try:
        usecols = ['time', 'addr', 'chiplet', 'type', 'is_ptw'] + _extra_filter_cols(args.filter)
        df = pd.read_csv(
            trace_file, skipinitialspace=True,
            dtype={'addr': 'uint64', 'type': 'int8',
                   'chiplet': 'int16', 'is_ptw': 'int8'},
        )
        df = df.drop_duplicates()
        df = df[usecols]
        df = df[(df['is_ptw'] == 0) & df['type'].isin(DEMAND_TYPES)]
        df = _apply_filter(df, args.filter)
        if df.empty:
            return None

        df['cacheline'] = df['addr'].to_numpy() >> 6

        t_min = df['time'].min()
        t_max = df['time'].max()
        if t_min == t_max:
            return None

        win_sec  = args.window_size / CLOCK_HZ
        n_windows = math.ceil((t_max - t_min) / win_sec)
        edges     = t_min + np.arange(n_windows + 1) * win_sec
        edges[-1] = t_max + 1e-15   # include 마지막 이벤트

        # window 인덱스 할당
        df['window'] = pd.cut(
            df['time'],
            bins=edges,
            labels=False,
            include_lowest=True,
        ).astype('int32')

        # (window, cacheline) → 고유 칩렛 수
        sharing = (
            df.groupby(['window', 'cacheline'], sort=False)['chiplet']
            .nunique()
            .rename('n_chiplets')
            .reset_index()
        )

        # 공유 구간 분류
        sharing['bin'] = pd.cut(
            sharing['n_chiplets'],
            bins=BIN_EDGES,
            labels=BIN_LABELS,
            right=True,
        )

        counts = (
            sharing.groupby(['window', 'bin'], observed=True)
            .size()
            .unstack(fill_value=0)
        )
        for lbl in BIN_LABELS:
            if lbl not in counts.columns:
                counts[lbl] = 0
        counts = counts[BIN_LABELS]
        counts = counts.reindex(range(n_windows), fill_value=0)

        totals = counts.sum(axis=1)
        pct = counts.div(totals.replace(0, np.nan), axis=0).fillna(0) * 100

        pct.index.name = 'window'
        pct['benchmark'] = benchmark
        pct['cycle_start'] = (pct.index.values * args.window_size).astype('int64')
        pct['cycle_end']   = ((pct.index.values + 1) * args.window_size).astype('int64')
        pct['n_lines']     = totals.values

        return pct

    except Exception as e:
        print(f"[{benchmark}] Error: {e}", flush=True)
        import traceback; traceback.print_exc()
        return None


def plot_benchmark(pct, benchmark, out_dir, window_size):
    from plot_style import apply_paper_style, SIZE_DOUBLE_TALL
    apply_paper_style()

    n_win = len(pct)
    x     = np.arange(n_win)

    fig, ax = plt.subplots(figsize=(max(SIZE_DOUBLE_TALL[0], 0.28 * n_win + 1.5),
                                     SIZE_DOUBLE_TALL[1]))

    bottom = np.zeros(n_win)
    for lbl, color in zip(BIN_LABELS, BIN_COLORS):
        vals = pct[lbl].values
        ax.bar(x, vals, width=0.85, bottom=bottom,
               color=color, label=lbl, edgecolor='white', linewidth=0.3,
               zorder=3)
        bottom += vals

    # x축: 사이클 수 (M cycles 단위)
    cycles = pct['cycle_start'].values
    step   = max(1, n_win // 10)
    ax.set_xticks(x[::step])
    ax.set_xticklabels(
        [f"{c / 1e6:.0f}" for c in cycles[::step]],
        rotation=35, ha='right', fontsize=6.5,
    )
    ax.set_xlabel(f'Cycle (M cycles,  window size = {window_size // 1000}K cycles)')

    ax.set_ylim(0, 100)
    ax.set_yticks([0, 25, 50, 75, 100])
    ax.set_ylabel('Cacheline Distribution (%)')
    ax.set_xlim(-0.5, n_win - 0.5)

    ax.set_title(
        f'{benchmark}  —  Cacheline Sharing Degree over Time\n'
        f'({n_win} windows × {window_size // 1000}K cycles)',
        fontsize=8, pad=6,
    )
    ax.yaxis.grid(True, linestyle='--', linewidth=0.4, color='#cccccc', zorder=0)
    ax.set_axisbelow(True)

    ax.legend(
        title='# Chiplets', title_fontsize=7,
        loc='upper left', bbox_to_anchor=(1.01, 1.0),
        fontsize=7, frameon=False, borderaxespad=0,
    )

    out_path = os.path.join(out_dir, f"{benchmark}_sharing_over_time.png")
    fig.savefig(out_path, dpi=400, bbox_inches='tight', pad_inches=0.03)
    plt.close(fig)
    return out_path


if __name__ == "__main__":
    ap = argparse.ArgumentParser(description=__doc__.split('\n')[0])
    ap.add_argument('run_id', nargs='?', default='latest',
                    help="run-id (latest, <config>, <config>/<timestamp>, ...)")
    ap.add_argument('--xDim',        type=int, default=6)
    ap.add_argument('--yDim',        type=int, default=6)
    ap.add_argument('--window-size', type=int, default=10000,
                    dest='window_size',
                    help="window 크기 (사이클, default 1,000,000)")
    ap.add_argument('--filter', choices=_FILTER_CHOICES, default='all',
                    help='all: 모든 demand 접근; rdma-only: chiplet!=owner; local-only: chiplet==owner; rdma-miss-only: chiplet!=owner & rdma==True')
    args = ap.parse_args()

    run = run_root(args.run_id)
    args.trace_dir = str(memory_trace_dir(args.run_id))
    print(f"run         = {run}")
    print(f"trace dir   = {args.trace_dir}")
    print(f"window size = {args.window_size:,} cycles")

    with ProcessPoolExecutor() as ex:
        results = list(ex.map(process_benchmark, benchmarks, repeat(args)))

    results = [r for r in results if r is not None]
    if not results:
        print("No results.")
        sys.exit(0)

    username = os.path.expanduser('~')
    out_dir  = f"{username}/mgvm/analyze/_out/cacheline/{run_tag(args.run_id)}"
    os.makedirs(out_dir, exist_ok=True)

    for pct in results:
        bench = pct['benchmark'].iloc[0]
        png   = plot_benchmark(
            pct[BIN_LABELS + ['cycle_start', 'cycle_end', 'n_lines']].copy(),
            bench, out_dir, args.window_size,
        )
        print(f"{bench}: {png}")

    xlsx_path = os.path.join(out_dir, f"cacheline_sharing_over_time_{args.filter}.xlsx")
    with pd.ExcelWriter(xlsx_path) as w:
        for pct in results:
            bench = pct['benchmark'].iloc[0]
            pct.drop(columns=['benchmark']).to_excel(
                w, sheet_name=bench[:31], index=True)
    print(f"\nxlsx: {xlsx_path}")
