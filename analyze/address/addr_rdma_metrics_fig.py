import pandas as pd
import re
import os
from ast import literal_eval
from collections import Counter
import math
import numpy as np
import matplotlib.pyplot as plt
import matplotlib.patches as mpatches
import seaborn as sns
import matplotlib.lines as mlines
import matplotlib.ticker as ticker

username = os.path.expanduser('~')

num_chiplets = 36
ns = 0.000000001
chip_cols = [str(i) for i in range(36)]

benchmarks = [
    # Low-sharing Apps
    "fdtd2d", "jacobi1d", "fastwalshtransform", "relu", "fft", "fir", 
    "sssp", "convolution2d", "color", "bitonicsort", "pagerank", "simpleconvolution",
    # High-sharing Apps
    'gups', 'syr2k', 'spmv', 'mis', 'stencil2d', 'gemm', 'lu', 
    'matrixmultiplication', 'mm2', 'mm3', 'gaussian', 'matrixtranspose', 
    'im2col', 'vgg16', 'bfs',
]

clusters_3x3 = {
    'cluster_0_3x3': [0, 1, 2, 6, 7, 8, 12, 13, 14],
    'cluster_1_3x3': [3, 4, 5, 9, 10, 11, 15, 16, 17],
    'cluster_2_3x3': [18, 19, 20, 24, 25, 26, 30, 31, 32],
    'cluster_3_3x3': [21, 22, 23, 27, 28, 29, 33, 34, 35],
}

clusters_2x2 = {
    'cluster_0_2x2': [0, 1, 6, 7],       # 좌측 상단
    'cluster_1_2x2': [2, 3, 8, 9],       # 상단 중앙
    'cluster_2_2x2': [4, 5, 10, 11],     # 우측 상단
    'cluster_3_2x2': [12, 13, 18, 19],   # 좌측 중앙
    'cluster_4_2x2': [14, 15, 20, 21],   # 정중앙
    'cluster_5_2x2': [16, 17, 22, 23],   # 우측 중앙
    'cluster_6_2x2': [24, 25, 30, 31],   # 좌측 하단
    'cluster_7_2x2': [26, 27, 32, 33],   # 하단 중앙
    'cluster_8_2x2': [28, 29, 34, 35]    # 우측 하단
}

clusters_3x3_cols = [k for k in clusters_3x3.keys()]
clusters_2x2_cols = [k for k in clusters_2x2.keys()]

chiplet_to_cluster_2x2 = {}
for cluster_name, chiplet_ids in clusters_2x2.items():
    for chip_id in chiplet_ids:
        chiplet_to_cluster_2x2[chip_id] = cluster_name

chiplet_to_cluster_3x3 = {}
for cluster_name, chiplet_ids in clusters_3x3.items():
    for chip_id in chiplet_ids:
        chiplet_to_cluster_3x3[chip_id] = cluster_name

MARKER_LIST = ['o', 's', '^', 'D', 'v', 'p', '*', 'h', 'X', '<', '>', 'd', 'H', 'P']

def plot_scatter_addr(df, benchmark, result_dir, value,
                      time_interval=10000, top_addrs=None,
                      addr_num="all", color_map=None, marker_map=None,
                      max_legend_items=15):
    if df.empty:
        print(f"Skip: {benchmark} data is empty.")
        return

    target_cols = ['interval', 'addr', value]
    if value not in df.columns:
        print(f"Skip: '{value}' not found in {benchmark}")
        return

    df_sub = df[target_cols].copy()

    if top_addrs is not None:
        df_sub = df_sub[df_sub['addr'].isin(top_addrs)]

    pivot_df = df_sub.pivot(index='interval', columns='addr', values=value)

    # 많이 나온 addr 순 정렬
    if top_addrs is not None:
        ordered_cols = [a for a in top_addrs if a in pivot_df.columns]
        pivot_df = pivot_df[ordered_cols]
    else:
        addr_sums = pivot_df.sum().sort_values(ascending=False)
        pivot_df = pivot_df[addr_sums.index]

    sns.set_theme(style="whitegrid",
                  rc={"axes.edgecolor": ".15",
                      "xtick.bottom": True,
                      "ytick.left": True})
    fig, ax = plt.subplots(figsize=(10, 4.5))

    unique_addrs = pivot_df.columns

    for i, addr_val in enumerate(unique_addrs):
        c = color_map.get(addr_val, '#808080') if color_map else '#808080'
        m = marker_map.get(addr_val, 'o') if marker_map else 'o'
        label_str = f"addr {addr_val}"
        ax.scatter(pivot_df.index, pivot_df[addr_val],
                   color=c, marker=m, s=15, alpha=0.8, label=label_str)

    clean_val_name = value.replace('_', ' ').title()
    ax.set_title(f"[{benchmark}] {clean_val_name} Over Time, ({addr_num}) addrs",
                 fontsize=14, fontweight='bold', pad=15)
    ax.set_xlabel("Time Interval (ns)", fontsize=11, fontweight='medium')
    ax.set_ylabel(clean_val_name, fontsize=11, fontweight='medium')

    ax.set_xlim(pivot_df.index.min(), pivot_df.index.max())

    if 'ratio' in value:
        ax.set_ylim(0, 1.05)
    else:
        actual_max = pivot_df.max().max()
        if pd.isna(actual_max) or actual_max == 0:
            print(f"Skip: {benchmark} - '{value}' is all zero/NaN.")
            plt.close(fig)
            return
        ax.set_ylim(0, actual_max * 1.05)
        if "degree" not in value:
            ax.set_yscale('symlog')

    ax.grid(axis='y', linestyle='--', linewidth=0.7, alpha=0.5)
    ax.grid(axis='x', visible=False)
    sns.despine(left=False, bottom=False)

    handles, labels = ax.get_legend_handles_labels()
    if len(handles) > max_legend_items:
        hidden_count = len(handles) - max_legend_items
        handles = handles[:max_legend_items]
        labels = labels[:max_legend_items]
        empty_handle = mlines.Line2D([], [], color='none', marker='none', linestyle='None')
        handles.append(empty_handle)
        labels.append(f"... and {hidden_count} more")

    legend = ax.legend(handles, labels,
                       title="Addrs", title_fontsize='10',
                       bbox_to_anchor=(1.02, 1), loc='upper left',
                       fontsize=9, frameon=True, edgecolor='0.8', markerscale=2)
    legend.get_title().set_fontweight('bold')

    plt.tight_layout()

    out_dir = f"{result_dir}/{benchmark}/{addr_num}/{time_interval}"
    os.makedirs(out_dir, exist_ok=True)
    out_file = f"{out_dir}/{benchmark}_{value}_scatter_addr.png"
    plt.savefig(out_file, dpi=400, bbox_inches='tight', transparent=False)
    plt.close(fig)

    print(f"✅ Saved plot: {out_file}")


if __name__ == "__main__":
    # time_intervals = [1000, 5000, 10000, 20000, 50000, 100000]
    time_intervals = [1000, 10000]
    shared_types = ['shared']  # addr 기준이면 is_shared 플래그/degree 위주로 쓰는 식으로 조정

    for benchmark in benchmarks:
        for time_interval in time_intervals:
            addr_metrics_file = f"{username}/mgvm/analyze/_out/address/addr-rdma-metrics/{benchmark}/{benchmark}_addr_rdma_metrics_{time_interval}.csv"
            if not os.path.exists(addr_metrics_file):
                print(f"{benchmark} addr metrics not exists")
                continue

            df = pd.read_csv(addr_metrics_file, skipinitialspace=True)

            # 필요하면 per-interval 비율 재계산 가능 (이미 CSV에 들어있다면 생략)
            # df['access_ratio'] = df['access_count'] / df['interval_total_access']

            # 상위 addr 선택 기준: 예) access_count 평균이 큰 addr
            addr_totals = df.groupby('addr')['access_count'].mean()
            top_n = 10
            top_addrs = addr_totals.sort_values(ascending=False).head(top_n).index.tolist()

            global_colors = sns.color_palette("tab20", top_n) if top_n <= 20 else sns.color_palette("husl", top_n)
            addr_color_map = dict(zip(top_addrs, global_colors))
            addr_marker_map = {a: MARKER_LIST[i % len(MARKER_LIST)] for i, a in enumerate(top_addrs)}

            addr_num = "all" if top_n == df['addr'].nunique() else f"{top_n}"
            result_dir = f"{username}/mgvm/analyze/_out/address/addr-rdma-metrics-fig"
            os.makedirs(result_dir, exist_ok=True)

            # 예시: access_ratio, access_count, shared_degree 등 그림
            for value in ['access_ratio', 'access_count',
                          'shared_degree', 'shared_degree_2x2', 'shared_degree_3x3']:
                if value not in df.columns:
                    continue
                plot_scatter_addr(df=df, benchmark=benchmark, result_dir=result_dir,
                                  value=value, time_interval=time_interval,
                                  top_addrs=top_addrs, addr_num=addr_num,
                                  color_map=addr_color_map, marker_map=addr_marker_map)