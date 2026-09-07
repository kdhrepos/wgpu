"""Hot 캐시 라인 eviction 에서 PC 별 점유 비율.

질문: "Hot 캐시 라인이 evict 될 때, 어떤 PC 가 install 한 데이터가 가장 많이 쫓겨나는가?"

방법:
  1. demand access (type 0~5) 기준 상위 --hot-pct % 캐시 라인 = Hot set
  2. victim row (type 8/9) 중 Hot set 에 속하는 것만 추출
  3. victim PC (= 그 라인을 원래 install 한 PC) 별 eviction 이벤트 수 집계
  4. share (%) = PC eviction 수 / 전체 hot-line eviction 수

출력:
  - 콘솔: 벤치마크별 top-N PC 테이블
  - _out/pc-stats/<run-id>/<benchmark>_hot_eviction_pc_share.png  (벤치마크 개별 figure)
  - _out/pc-stats/<run-id>/pc_eviction_ratio.xlsx  (전체 데이터)

사용:
  analyze/run.sh pc-stats/pc_eviction_ratio.py mesh-6x6/20260624-002942
  analyze/run.sh pc-stats/pc_eviction_ratio.py latest --hot-pct 10 --top 20
"""

import argparse
import os
import sys
import numpy as np
import pandas as pd
import matplotlib.pyplot as plt

parent_dir = os.path.abspath(os.path.join(os.path.dirname(__file__), "../../"))
if parent_dir not in sys.path:
    sys.path.append(parent_dir)
scripts_dir = os.path.abspath(os.path.join(os.path.dirname(__file__), "../../../scripts"))
if scripts_dir not in sys.path:
    sys.path.append(scripts_dir)

from concurrent.futures import ProcessPoolExecutor
from itertools import repeat
from params import benchmarks
from _paths import run_root, memory_trace_dir, run_tag

DEMAND_TYPES = {0, 1, 2, 3, 4, 5}
VICTIM_TYPES = {8, 9}


def process_benchmark(benchmark, args):
    trace_file = os.path.join(
        args.trace_dir,
        f"{benchmark}_mesh_{args.xDim}x{args.yDim}_memory_trace.csv",
    )
    if not os.path.exists(trace_file):
        return None

    print(f"[{benchmark}] ...", flush=True)
    try:
        df = pd.read_csv(
            trace_file, skipinitialspace=True,
            dtype={'addr': 'uint64', 'pc': 'uint64',
                   'type': 'int8', 'is_ptw': 'int8'},
        )
        df = df.drop_duplicates()
        df = df[['addr', 'pc', 'type', 'is_ptw']]
        df = df[df['is_ptw'] == 0]
        df['cacheline'] = df['addr'].to_numpy() >> 6

        demand = df[df['type'].isin(DEMAND_TYPES)]
        victim = df[df['type'].isin(VICTIM_TYPES)]

        if demand.empty or victim.empty:
            return None

        # Hot 캐시 라인 = demand access 상위 hot_pct%
        line_access = demand.groupby('cacheline', sort=False).size()
        hot_thr = np.percentile(line_access.values, 100 - args.hot_pct)
        hot_lines = set(line_access.index[line_access >= hot_thr].tolist())

        # victim 중 hot 라인에 해당하는 것만
        hot_victim = victim[victim['cacheline'].isin(hot_lines)]
        total_hot_evictions = len(hot_victim)
        if total_hot_evictions == 0:
            return None

        # PC 별 집계
        pc_counts = hot_victim.groupby('pc').size().sort_values(ascending=False)
        pc_share = (pc_counts / total_hot_evictions * 100).round(3)

        result = pd.DataFrame({
            'benchmark':       benchmark,
            'pc':              pc_counts.index.map(lambda v: str(int(v))),
            'eviction_count':  pc_counts.values,
            'share_%':         pc_share.values,
        })
        result['cumulative_%'] = result['share_%'].cumsum().round(3)
        result.insert(0, 'rank', range(1, len(result) + 1))
        result['total_hot_evictions'] = total_hot_evictions
        result['hot_lines'] = len(hot_lines)
        return result

    except Exception as e:
        print(f"[{benchmark}] Error: {e}", flush=True)
        return None


def plot_benchmark(df, benchmark, out_dir, top_k, hot_pct):
    from plot_style import apply_paper_style, SIZE_DOUBLE_TALL
    apply_paper_style()

    top          = df.head(top_k).copy()
    total_hot_ev = int(df['total_hot_evictions'].iloc[0])
    hot_lines    = int(df['hot_lines'].iloc[0])
    n_pcs        = len(df)
    top_sum      = top['share_%'].sum()

    # 나머지를 "Others" 로 묶기
    if n_pcs > top_k:
        others_row = pd.DataFrame([{
            'rank':            top_k + 1,
            'benchmark':       benchmark,
            'pc':              f'Others\n({n_pcs - top_k})',
            'eviction_count':  total_hot_ev - top['eviction_count'].sum(),
            'share_%':         round(100 - top_sum, 3),
        }])
        top = pd.concat([top, others_row], ignore_index=True)

    n = len(top)
    bar_w  = 0.6
    fig_w  = max(SIZE_DOUBLE_TALL[0], 0.42 * n + 1.0)
    fig, ax = plt.subplots(figsize=(fig_w, SIZE_DOUBLE_TALL[1]))

    x_pos  = np.arange(n)
    colors = ['#0072B2'] * top_k + ['#aaaaaa'] * (n - top_k)
    bars   = ax.bar(x_pos, top['share_%'], width=bar_w,
                    color=colors, edgecolor='white', linewidth=0.5,
                    zorder=3)

    # 바 위 수치 레이블 (1% 이상만)
    for bar, val in zip(bars, top['share_%']):
        if val >= 1.0:
            ax.text(
                bar.get_x() + bar.get_width() / 2,
                bar.get_height() + 0.4,
                f'{val:.1f}',
                ha='center', va='bottom', fontsize=6, color='#333333',
            )

    # 누적 비율 꺾은선 (secondary y)
    ax2 = ax.twinx()
    cum = top['share_%'].cumsum().values
    ax2.plot(x_pos, cum, color='#D55E00', linewidth=1.0,
             marker='o', markersize=3, zorder=4)
    ax2.set_ylim(0, 105)
    ax2.set_ylabel('Cumulative Share (%)', fontsize=8)
    ax2.tick_params(axis='y', labelsize=7)
    ax2.spines['top'].set_visible(False)
    ax2.yaxis.grid(False)
    # 80% 수평선
    ax2.axhline(80, color='#D55E00', linewidth=0.6,
                linestyle=':', alpha=0.7, zorder=2)
    ax2.text(n - 0.4, 81, '80%', fontsize=6, color='#D55E00', va='bottom')

    ax.set_xticks(x_pos)
    ax.set_xticklabels(top['pc'], rotation=45, ha='right', fontsize=6.5)
    ax.set_xlim(-0.5, n - 0.5)
    ax.set_ylim(0, max(top['share_%'].max() * 1.18, 5))
    ax.set_ylabel('Share of Hot Cacheline Evictions (%)')
    ax.set_xlabel('Victim PC (Install PC of Evicted Line)')
    ax.set_title(
        f'{benchmark}  —  PC-level Hot Cacheline Eviction Share\n'
        f'hot lines: {hot_lines:,}  |  hot evictions: {total_hot_ev:,}  |  top-{hot_pct:.0f}%',
        fontsize=8, pad=6,
    )
    ax.yaxis.grid(True, linestyle='--', linewidth=0.4,
                  color='#cccccc', alpha=1, zorder=0)
    ax.set_axisbelow(True)

    # 범례 (두 시리즈)
    from matplotlib.patches import Patch
    from matplotlib.lines import Line2D
    handles = [
        Patch(facecolor='#0072B2', edgecolor='white', label='Top PCs'),
        Patch(facecolor='#aaaaaa', edgecolor='white', label='Others'),
        Line2D([0], [0], color='#D55E00', linewidth=1.0,
               marker='o', markersize=3, label='Cumulative'),
    ]
    ax.legend(handles=handles, loc='upper right', fontsize=7, frameon=False)

    out_path = os.path.join(out_dir, f"{benchmark}_hot_eviction_pc_share.png")
    fig.savefig(out_path, dpi=400, bbox_inches='tight', pad_inches=0.03)
    plt.close(fig)
    return out_path


if __name__ == "__main__":
    ap = argparse.ArgumentParser(description=__doc__.split('\n')[0])
    ap.add_argument('run_id', nargs='?', default='latest',
                    help="run-id (latest, <config>, <config>/<timestamp>, ...)")
    ap.add_argument('--xDim', type=int, default=6)
    ap.add_argument('--yDim', type=int, default=6)
    ap.add_argument('--hot-pct', type=float, default=5.0,
                    help="Hot 임계: demand access 상위 백분위 (default 5)")
    ap.add_argument('--top', type=int, default=15,
                    help="figure 에 표시할 상위 PC 수 (default 15)")
    args = ap.parse_args()

    run = run_root(args.run_id)
    args.trace_dir = str(memory_trace_dir(args.run_id))
    print(f"run       = {run}")
    print(f"trace dir = {args.trace_dir}")
    print(f"hot_pct   = {args.hot_pct}%")

    with ProcessPoolExecutor() as ex:
        results = list(ex.map(process_benchmark, benchmarks, repeat(args)))

    results = [r for r in results if r is not None]
    if not results:
        print("No results (type 8/9 victim rows 없음 — 2026-06-23 이후 sweep 필요).")
        sys.exit(0)

    username = os.path.expanduser('~')
    out_dir = f"{username}/mgvm/analyze/_out/pc-stats/{run_tag(args.run_id)}"
    os.makedirs(out_dir, exist_ok=True)

    # 콘솔 출력 + figure
    for df in results:
        bench = df['benchmark'].iloc[0]
        total = int(df['total_hot_evictions'].iloc[0])
        top_share = df.head(args.top)['share_%'].sum()
        print(f"\n{'='*80}")
        print(f"{bench}  (hot evictions={total:,}, top-{args.top} cover {top_share:.1f}%)")
        print(df.head(args.top)[['rank','pc','eviction_count','share_%','cumulative_%']].to_string(index=False))
        png = plot_benchmark(df, bench, out_dir, args.top, args.hot_pct)
        print(f"figure: {png}")

    # xlsx 전체 저장
    all_df = pd.concat(results, ignore_index=True)
    xlsx_path = os.path.join(out_dir, "pc_eviction_ratio.xlsx")
    with pd.ExcelWriter(xlsx_path) as w:
        for df in results:
            bench = df['benchmark'].iloc[0]
            df.to_excel(w, sheet_name=bench[:31], index=False)
    print(f"\nxlsx: {xlsx_path}")
