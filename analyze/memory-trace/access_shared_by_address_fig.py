"""x=address, y=access_count / shared_degree, color=PC top 10.

참조:
    - 코드: analyze/pc/pc_rdma_metrics_fig.py + addr_metrics_plotter.plot_address_metrics
    - 이미지:
        _out/pc-analysis-data/pc-metrics-fig/mis/mis_access_count_by_address.png
        _out/pc-analysis-data/pc-metrics-fig/mis/mis_shared_degree_by_address.png

기본 입력 = stack_distance.py 출력 CSV
    _out/stack-distance/<config>-<ts>/<bench>_stack_distance.csv
columns: pc, addr, owner, access_count, shared_degree, ...
없으면 raw memory trace에서 fallback 집계.

실행 (stack_distance.py / stack_distance_fig.py와 동일 인터페이스):
    analyze/run.sh memory-trace/access_shared_by_address_fig.py mesh-6x6/latest
    analyze/run.sh memory-trace/access_shared_by_address_fig.py <run-id>
    analyze/run.sh memory-trace/access_shared_by_address_fig.py <run-id> --top-n 10 --workers 8
"""

import os
import re
import sys
import argparse
import numpy as np
import pandas as pd
import matplotlib
matplotlib.use('Agg')
import matplotlib.pyplot as plt
import matplotlib.lines as mlines
import matplotlib.ticker as ticker
import seaborn as sns
from concurrent.futures import ProcessPoolExecutor

sys.path.append(os.path.abspath(os.path.join(os.path.dirname(__file__), "../common")))
sys.path.append(os.path.abspath(os.path.join(os.path.dirname(__file__), "../../scripts")))

from _paths import run_root, memory_trace_dir, run_config, run_tag
from params import benchmarks


MARKER_LIST = ['o', 's', '^', 'D', 'v', '<', '>', 'p', '*', 'h']
BG_COLOR = '#bfbfbf'

# fork된 worker에 상속될 globals
TRACE_DIR = None
CSV_DIR = None
OUT_ROOT = None
XDIM = YDIM = 6
TOP_N = 10


# ----------------------------- 데이터 로더 ----------------------------------
def load_from_csv(csv_path):
    df = pd.read_csv(csv_path)
    need = {'pc', 'addr', 'access_count', 'shared_degree'}
    if not need.issubset(df.columns):
        return None
    return df[['pc', 'addr', 'access_count', 'shared_degree']]


def load_from_trace(trace_path):
    # raw 전체 row 기준으로 dedup해야 같은 time의 중복만 제거되고,
    # 다른 time의 같은 (addr,pc,chiplet) 접근은 별개로 보존됨.
    cols = ['time', 'addr', 'pc', 'type', 'chiplet']
    dtype = {'time': 'float64', 'addr': 'uint64', 'pc': 'uint64',
             'type': 'int8', 'chiplet': 'int16'}
    df = pd.read_csv(trace_path, usecols=cols, dtype=dtype, skipinitialspace=True)
    df = df.drop_duplicates()
    df = df[~df['type'].isin([6, 7, 8, 9])]
    if df.empty:
        return None
    agg = df.groupby(['pc', 'addr'], as_index=False).agg(
        access_count=('chiplet', 'size'),
    )
    shared = df.groupby('addr', as_index=False).agg(
        shared_degree=('chiplet', 'nunique'),
    )
    return agg.merge(shared, on='addr', how='left')


# ----------------------------- plotter ------------------------------------
def build_pc_maps(df, top_n=10):
    pc_totals = df.groupby('pc')['access_count'].sum().sort_values(ascending=False)
    top_pcs = pc_totals.head(top_n).index.tolist()
    palette = sns.color_palette("tab10", len(top_pcs))
    color_map = {pc: palette[i] for i, pc in enumerate(top_pcs)}
    marker_map = {pc: MARKER_LIST[i % len(MARKER_LIST)] for i, pc in enumerate(top_pcs)}
    return top_pcs, color_map, marker_map


def plot_value_by_addr(df, top_pcs, color_map, marker_map,
                       benchmark, value, ylabel, out_path,
                       log_y=False, max_legend_items=12):
    plot_df = df[['pc', 'addr', value]].copy()
    plot_df = plot_df[plot_df[value].notna()]
    if plot_df.empty:
        print(f"skip {benchmark} {value}: empty")
        return

    sns.set_theme(style="whitegrid", rc={"axes.edgecolor": ".15"})
    fig, ax = plt.subplots(figsize=(15, 6), dpi=200)

    bg = plot_df[~plot_df['pc'].isin(top_pcs)]
    if not bg.empty:
        ax.scatter(bg['addr'], bg[value],
                   color=BG_COLOR, marker='o', s=6.0,
                   alpha=0.30, edgecolors='none', rasterized=True,
                   label='_nolegend_')

    fg = plot_df[plot_df['pc'].isin(top_pcs)]
    pc_order = [pc for pc in top_pcs if pc in fg['pc'].unique()]
    for pc in pc_order:
        g = fg[fg['pc'] == pc]
        c = color_map.get(pc, BG_COLOR)
        m = marker_map.get(pc, 'o')
        ax.scatter(g['addr'], g[value],
                   color=c, marker=m, s=35.0,
                   alpha=0.7, edgecolors='none', rasterized=True,
                   label=f"PC {int(pc)}")

    clean = value.replace('_', ' ').title()
    ax.set_title(f"[{benchmark}] {clean} by Address",
                 fontsize=15, fontweight='bold', pad=15)
    ax.set_xlabel("Memory Address (Hex)", fontsize=12, fontweight='medium')
    ax.set_ylabel(ylabel, fontsize=12, fontweight='medium')
    ax.xaxis.set_major_formatter(ticker.FuncFormatter(lambda x, p: f"0x{int(x):X}"))
    plt.xticks(rotation=45, ha='right')

    vmax = plot_df[value].max()
    if log_y:
        ax.set_yscale('symlog', linthresh=1.0)
        if pd.notna(vmax) and vmax > 0:
            ax.set_ylim(0, vmax * 1.5)
    else:
        if pd.notna(vmax) and vmax > 0:
            ax.set_ylim(0, vmax * 1.08)

    ax.grid(axis='y', linestyle='--', linewidth=0.7, alpha=0.5)
    sns.despine(left=False, bottom=False)

    handles, labels = ax.get_legend_handles_labels()
    if len(handles) > max_legend_items:
        hidden = len(handles) - max_legend_items
        handles = handles[:max_legend_items]
        labels = labels[:max_legend_items]
        handles.append(mlines.Line2D([], [], color='none', marker='none', linestyle='None'))
        labels.append(f"... and {hidden} more")
    legend = ax.legend(handles, labels, title="PCs",
                       title_fontsize='11', fontsize=10,
                       bbox_to_anchor=(1.02, 1), loc='upper left',
                       frameon=True, edgecolor='0.8', markerscale=1.5)
    legend.get_title().set_fontweight('bold')
    for h in legend.legend_handles:
        if hasattr(h, 'set_alpha'):
            h.set_alpha(1.0)

    plt.tight_layout()
    os.makedirs(os.path.dirname(out_path), exist_ok=True)
    plt.savefig(out_path, dpi=400, bbox_inches='tight')
    plt.close(fig)


# ----------------------------- benchmark 1개 -------------------------------
def process_benchmark(benchmark):
    out_dir = f"{OUT_ROOT}/{benchmark}"
    out_access = f"{out_dir}/{benchmark}_access_count_by_address.png"
    out_shared = f"{out_dir}/{benchmark}_shared_degree_by_address.png"

    csv = f"{CSV_DIR}/{benchmark}_stack_distance.csv"
    df = None
    src = None
    if os.path.exists(csv):
        df = load_from_csv(csv)
        src = "csv"
    if df is None:
        trace = f"{TRACE_DIR}/{benchmark}_mesh_{XDIM}x{YDIM}_memory_trace.csv"
        if not os.path.exists(trace):
            return f"[{benchmark}] no csv, no trace"
        print(f"[{benchmark}] fallback raw trace...", flush=True)
        df = load_from_trace(trace)
        src = "trace"
        if df is None:
            return f"[{benchmark}] empty"

    print(f"[{benchmark}] plot ({src}, {len(df):,} rows)...", flush=True)
    top_pcs, color_map, marker_map = build_pc_maps(df, top_n=TOP_N)

    plot_value_by_addr(df, top_pcs, color_map, marker_map,
                       benchmark, 'access_count',
                       "Total Access Count",
                       out_access, log_y=True)
    plot_value_by_addr(df, top_pcs, color_map, marker_map,
                       benchmark, 'shared_degree',
                       "Shared Degree (unique chiplets)",
                       out_shared, log_y=False)
    return f"[{benchmark}] saved -> {out_dir}"


# ----------------------------- main ----------------------------------------
if __name__ == "__main__":
    ap = argparse.ArgumentParser()
    ap.add_argument('run_id', nargs='?', default='mesh-6x6',
                    help="run-id (_paths.run_root 해석. 기본 mesh-6x6 최신)")
    ap.add_argument('--top-n', type=int, default=10)
    ap.add_argument('--csv-dir', type=str, default=None,
                    help="명시하면 그 폴더의 *_stack_distance.csv 사용")
    ap.add_argument('--workers', type=int,
                    default=int(os.environ.get("ADDR_FIG_WORKERS", "8")))
    args = ap.parse_args()

    root = run_root(args.run_id)
    TRACE_DIR = str(memory_trace_dir(args.run_id))
    config, ts = run_config(args.run_id), run_tag(args.run_id)
    m = re.search(r'(\d+)x(\d+)', config)
    if m:
        XDIM, YDIM = int(m.group(1)), int(m.group(2))
    TOP_N = args.top_n
    tag = f"{config}-{ts}"

    CSV_DIR = args.csv_dir or os.path.abspath(os.path.join(
        os.path.dirname(__file__), "..", "_out", "stack-distance", tag))
    OUT_ROOT = os.path.abspath(os.path.join(
        os.path.dirname(__file__), "..", "_out", "memory-trace",
        "access-shared-by-address", ts))
    os.makedirs(OUT_ROOT, exist_ok=True)

    print(f"Run:     {config}/{ts}")
    print(f"Trace:   {TRACE_DIR}")
    print(f"CSV in:  {CSV_DIR}")
    print(f"Fig out: {OUT_ROOT}")
    print(f"Grid:    {XDIM}x{YDIM}   top_n={TOP_N}   workers={args.workers}")

    with ProcessPoolExecutor(max_workers=args.workers) as ex:
        for msg in ex.map(process_benchmark, benchmarks):
            print(msg, flush=True)
