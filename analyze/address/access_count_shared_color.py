"""access count vs sharing-colored mapping.

raw memory_trace를 읽어 주소별로 집계:
    y축 = access count (log)
    색상 = shared chiplets (그 주소를 만진 칩렛 nunique)
    x축 = memory address

기존 plot_access_count_with_shared_degree(y=shared, color=access)의 축/색 반전판.

실행:
    analyze/run.sh address/access_count_shared_color.py mesh-6x6/latest
"""

import os
import sys
import argparse
import pandas as pd
from concurrent.futures import ProcessPoolExecutor

sys.path.append(os.path.abspath(os.path.join(os.path.dirname(__file__), "../common")))
sys.path.append(os.path.abspath(os.path.join(os.path.dirname(__file__), "../../scripts")))

from _paths import run_root, memory_trace_dir
from params import benchmarks

username = os.path.expanduser('~')

# fork된 worker에 상속될 globals
TRACE_DIR = None
OUT_DIR = None
XDIM = YDIM = 6


def plot_shared_degree_colored_by_access(df, benchmark, result_dir, save_fig=True):
    """y축=access count, 색상=shared chiplets (기존 mapping 반전)."""
    import matplotlib.pyplot as plt
    import matplotlib.ticker as mtick
    import matplotlib.colors as colors
    import numpy as np

    # 1. 주소별 집계: access count + 공유 칩렛 수
    addr_stats = df.groupby('addr').agg(
        access_count=('addr', 'size'),
        shared_chiplets=('chiplet', 'nunique'),
    ).reset_index()

    total_accesses = int(addr_stats['access_count'].sum())
    unique_cachelines = len(addr_stats)
    max_sharing = int(addr_stats['shared_chiplets'].max())

    # 색상이 적은 공유도(1)에 가려지지 않게 공유도 높은 점을 위로 그림
    plot_data = addr_stats.sort_values(by='shared_chiplets')

    # 2. 시각화
    plt.style.use('seaborn-v0_8-paper')
    fig, ax = plt.subplots(figsize=(16, 8), dpi=300)

    # shared chiplets = 정수 -> 이산 colormap (BoundaryNorm)
    bounds = np.arange(0.5, max_sharing + 1.5, 1)
    cmap = plt.get_cmap('viridis', max_sharing)
    norm = colors.BoundaryNorm(bounds, cmap.N)

    scatter = ax.scatter(plot_data['addr'], plot_data['access_count'],
                         c=plot_data['shared_chiplets'],
                         cmap=cmap,
                         norm=norm,
                         s=5.0,
                         alpha=1,
                         rasterized=True)

    # 3. 축 설정 — y축은 access count (편차 큼 -> log)
    ax.set_xlabel('Memory Address (Hex)', fontsize=12, fontweight='bold')
    ax.set_ylabel('Access Count (Log Scale)', fontsize=12, fontweight='bold')
    ax.set_yscale('log')
    ax.yaxis.set_major_formatter(mtick.StrMethodFormatter('{x:,.0f}'))

    @mtick.FuncFormatter
    def hex_formatter(x, pos):
        return f"0x{int(x):08X}"
    ax.xaxis.set_major_formatter(hex_formatter)

    # 4. 컬러바 — shared chiplets (이산 정수 눈금)
    if max_sharing <= 16:
        ticks = np.arange(1, max_sharing + 1)
    else:
        step = int(np.ceil(max_sharing / 12))
        ticks = np.arange(1, max_sharing + 1, step)
    cbar = plt.colorbar(scatter, label='Shared Chiplets (Nodes)', ticks=ticks)
    cbar.ax.set_yticklabels([f"{int(t)}" for t in ticks])

    # 5. 제목
    title_str = (
        f"Access Count vs Sharing-Colored Mapping: {benchmark}\n"
        f"(Total Accesses: {total_accesses:,}, Unique Lines: {unique_cachelines:,}, "
        f"Max Sharing: {max_sharing:,} Nodes)"
    )
    plt.title(title_str, fontsize=15, fontweight='bold', pad=35, loc='center')

    # 6. 디자인 정리
    ax.grid(True, linestyle='--', alpha=0.15)
    ax.spines['top'].set_visible(False)
    ax.spines['right'].set_visible(False)
    plt.xticks(rotation=30, ha='right')
    plt.tight_layout()

    if save_fig:
        plt.savefig(f"{result_dir}/{benchmark}_access_count_shared_color.png",
                    bbox_inches='tight')
    plt.close()


def process_benchmark(benchmark):
    print(f">>> Processing Benchmark: {benchmark}")
    trace = f"{TRACE_DIR}/{benchmark}_mesh_{XDIM}x{YDIM}_memory_trace.csv"
    if not os.path.exists(trace):
        print(f"   Skip: No file {trace}")
        return {'benchmark': benchmark, 'status': 'Skip (No File)'}

    # 데이터 입력: 다수파 관례 = 전체행 중복제거 먼저 -> 컬럼 선택
    try:
        df = pd.read_csv(trace, skipinitialspace=True)
    except Exception as e:
        print(f"[{benchmark}] read fail: {e}")
        return {'benchmark': benchmark, 'status': f'Error: {e}'}

    df = df.drop_duplicates()
    df = df[~df['type'].isin([6, 7, 8, 9])]   # evictor 합성행 제외
    df = df[['addr', 'chiplet']]        # 필요한 컬럼만
    if df.empty:
        print(f"   Skip: empty {benchmark}")
        return {'benchmark': benchmark, 'status': 'Skip (Empty)'}

    try:
        os.makedirs(OUT_DIR, exist_ok=True)
        print(f"[{benchmark}] - Generating Access Count (Shared-Colored) Plot...")
        plot_shared_degree_colored_by_access(df, benchmark, OUT_DIR, save_fig=True)
        print(f">>> Done: {benchmark}")
        return {'benchmark': benchmark, 'status': 'Success'}
    except Exception as e:
        print(f"[{benchmark}] Error: {e}")
        return {'benchmark': benchmark, 'status': f'Error: {e}'}


if __name__ == "__main__":
    ap = argparse.ArgumentParser()
    ap.add_argument('run_id', nargs='?', default='mesh-6x6')
    ap.add_argument('--workers', type=int, default=8)
    args = ap.parse_args()

    root = run_root(args.run_id)
    TRACE_DIR = str(memory_trace_dir(args.run_id))
    OUT_DIR = f"{username}/mgvm/analyze/_out/address/{os.path.basename(root)}/plots/shared-color-analysis"

    print(f"Trace: {TRACE_DIR}")
    print(f"Out:   {OUT_DIR}")

    with ProcessPoolExecutor(max_workers=args.workers) as executor:
        results = list(executor.map(process_benchmark, benchmarks))
