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

base_dir=f'{username}/mgvm/results/memory-traces'
date = "20260221-223953"
expanded_dir = f"{base_dir}/{date}/expanded"

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


def get_pc_mapping(df, benchmark, top_n=10):
    np.random.seed(42)
    
    pc_totals = df.groupby('pc')['ro_shared_access_mean'].mean()
    top_pcs = pc_totals.sort_values(ascending=False).head(top_n).index.tolist()
    
    global_colors = sns.color_palette("tab20", len(top_pcs))
    # 🎯 matplotlib이 바로 인식하는 형태로 변환
    pc_color_map = {pc: global_colors[i] for i, pc in enumerate(top_pcs)}  # list[float] 형태
    MARKER_LIST = ['o', 's', '^', 'D', 'v', '<', '>', 'p', '*', 'h']
    pc_marker_map = {pc: MARKER_LIST[i % len(MARKER_LIST)] for i, pc in enumerate(top_pcs)}
    
    return pc_color_map, pc_marker_map, len(top_pcs)


# ---------------------------------------------------------
# 1. Memory Access Pattern
# ---------------------------------------------------------
def plot_memory_access_pattern(
    df, benchmark, result_dir, 
    color_map=None, marker_map=None, 
    max_legend_items=15):

    if df.empty:
        print(f"Skip: {benchmark} trace data is empty.")
        return

    plot_df = df[['time', 'addr', 'pc']].copy()

    if plot_df['addr'].dtype == 'O':
        plot_df['addr_int'] = plot_df['addr'].apply(int, base=16) 
    else:
        plot_df['addr_int'] = plot_df['addr']

    sns.set_theme(style="whitegrid", rc={"axes.edgecolor": ".15"})
    fig, ax = plt.subplots(figsize=(15, 6))

    pc_counts = plot_df['pc'].value_counts().sort_values(ascending=False)
    sorted_pcs = pc_counts.index.tolist()

    added_labels = set()

    for pc in sorted_pcs:
        pc_data = plot_df[plot_df['pc'] == pc]
        
        # ✅ 인자로 받은 color_map과 marker_map을 바로 사용 (없을 경우 기본값 fallback)
        c = color_map.get(pc, '#808080') if color_map else '#808080'
        m = marker_map.get(pc, 'o') if marker_map else 'o'
        
        if pc not in added_labels:
            label = f"PC {pc}" if isinstance(pc, (int, np.integer)) else f"{pc}"
            added_labels.add(pc)
        else:
            label = "_nolegend_"

        ax.scatter(pc_data['time'], pc_data['addr_int'], 
                   color=c, label=label, 
                   s=3.0, alpha=0.3, marker=m, edgecolors='none')

    ax.set_title(f"[{benchmark}] Memory Access Pattern over Time", fontsize=15, fontweight='bold', pad=15)
    ax.set_xlabel("Time (ns/cycles)", fontsize=12, fontweight='medium')
    ax.set_ylabel("Memory Address", fontsize=12, fontweight='medium')

    ax.yaxis.set_major_formatter(ticker.FuncFormatter(lambda x, pos: f"0x{int(x):X}"))

    y_min, y_max = plot_df['addr_int'].min(), plot_df['addr_int'].max()
    padding = (y_max - y_min) * 0.05
    ax.set_ylim(y_min - padding, y_max + padding)

    handles, labels = ax.get_legend_handles_labels()
    
    if len(handles) > max_legend_items:
        hidden_count = len(handles) - max_legend_items
        handles = handles[:max_legend_items]
        labels = labels[:max_legend_items]
        
        empty_handle = mlines.Line2D([], [], color='none', marker='none', linestyle='None')
        handles.append(empty_handle)
        labels.append(f"... and {hidden_count} more")

    legend = ax.legend(handles, labels,
                       title="PCs", title_fontsize='11',
                       bbox_to_anchor=(1.02, 1), loc='upper left',
                       fontsize=10, markerscale=8.0, frameon=True, edgecolor='0.8') 
    legend.get_title().set_fontweight('bold')
    
    for handle in legend.legend_handles:
        try:
            handle.set_alpha(1.0)
        except AttributeError:
            pass 

    plt.tight_layout()

    out_dir = f"{result_dir}/{benchmark}"
    os.makedirs(out_dir, exist_ok=True)
    out_file = f"{out_dir}/{benchmark}_memory_access_pattern.png"
    
    plt.savefig(out_file, dpi=300, bbox_inches='tight', transparent=False)
    plt.close()
    print(f"✅ Saved Memory Access Pattern plot: {out_file}")

# ---------------------------------------------------------
# 2. Address Metrics (Access Count & Sharing Degree)
# ---------------------------------------------------------
def plot_address_metrics(df, benchmark, result_dir, color_map=None, marker_map=None, value='access_count', max_legend_items=15):
    if df.empty:
        print(f"Skip: {benchmark} trace data is empty.")
        return

    cols = ['addr', 'pc']
    if f'{value}' in df.columns:
        cols.append(f'{value}')
        
    plot_df = df[cols].copy()

    if f'{value}' not in plot_df.columns:
        plot_df = plot_df.groupby(['pc', 'addr']).size().reset_index(name=f'{value}')
    else:
        plot_df = plot_df.groupby(['pc', 'addr'])[f'{value}'].sum().reset_index()

    if plot_df['addr'].dtype == 'O':
        plot_df['addr_int'] = plot_df['addr'].apply(int, base=16)
    else:
        plot_df['addr_int'] = plot_df['addr']

    sns.set_theme(style="whitegrid", rc={"axes.edgecolor": ".15"})
    fig, ax = plt.subplots(figsize=(15, 6))

    added_labels = set()

    # 데이터가 많은 PC 순으로 정렬하여 렌더링
    pc_totals = plot_df.groupby('pc')[f'{value}'].sum().sort_values(ascending=False)
    sorted_pcs = pc_totals.index.tolist()

    for pc in sorted_pcs:
        group_data = plot_df[plot_df['pc'] == pc]
        
        # ✅ 인자로 받은 color_map과 marker_map을 바로 사용
        c = color_map.get(pc, '#808080') if color_map else '#808080'
        m = marker_map.get(pc, 'o') if marker_map else 'o'
        
        if pc not in added_labels:
            label = f"PC {pc}" if isinstance(pc, (int, np.integer)) else f"{pc}"
            added_labels.add(pc)
        else:
            label = "_nolegend_"

        ax.scatter(group_data['addr_int'], group_data[f'{value}'], 
                   color=c, marker=m, label=label, 
                   s=25.0, alpha=0.6, edgecolors='none')
    
    clean_val_name = value.replace('_', ' ').title()
    ax.set_title(f"[{benchmark}] {clean_val_name} by Address", fontsize=15, fontweight='bold', pad=15)
    
    ax.set_xlabel("Memory Address", fontsize=12, fontweight='medium')
    ax.set_ylabel(f"Total {clean_val_name}", fontsize=12, fontweight='medium')

    ax.xaxis.set_major_formatter(ticker.FuncFormatter(lambda x, pos: f"0x{int(x):X}"))
    plt.xticks(rotation=45) 

    y_min, y_max = plot_df[f'{value}'].min(), plot_df[f'{value}'].max()
    padding = (y_max - y_min) * 0.05
    if padding == 0: padding = 1
    ax.set_ylim(max(0, y_min - padding), y_max + padding) 

    handles, labels = ax.get_legend_handles_labels()
    
    if len(handles) > max_legend_items:
        hidden_count = len(handles) - max_legend_items
        handles = handles[:max_legend_items]
        labels = labels[:max_legend_items]
        
        empty_handle = mlines.Line2D([], [], color='none', marker='none', linestyle='None')
        handles.append(empty_handle)
        labels.append(f"... and {hidden_count} more")

    legend = ax.legend(handles, labels,
                       title="PCs", title_fontsize='11',
                       bbox_to_anchor=(1.02, 1), loc='upper left',
                       fontsize=10, markerscale=2, frameon=True, edgecolor='0.8') 
    legend.get_title().set_fontweight('bold')
    
    for handle in legend.legend_handles:
        try:
            handle.set_alpha(1.0)
        except AttributeError:
            pass 

    plt.tight_layout()

    out_dir = f"{result_dir}/{benchmark}"
    os.makedirs(out_dir, exist_ok=True)
    out_file = f"{out_dir}/{benchmark}_{value}_by_address.png"
    
    plt.savefig(out_file, dpi=300, bbox_inches='tight', transparent=False)
    plt.close()
    print(f"✅ Saved Access Count by Address plot: {out_file}")

# ---------------------------------------------------------
# 3. Address Lifespan by PC
# ---------------------------------------------------------
def plot_addr_lifespan_by_pc(df, benchmark="benchmark", result_dir=".", time_interval=10000, pc_num="all", color_map=None, marker_map=None):
    plot_df = df[['interval', 'pc', 'addr', 'active_intervals_count']].drop_duplicates().copy()
    
    if plot_df['addr'].dtype == 'O':
        plot_df['addr_int'] = plot_df['addr'].apply(int, base=16)
    else:
        plot_df['addr_int'] = plot_df['addr']

    plt.rcParams['figure.dpi'] = 300
    plt.rcParams['savefig.dpi'] = 300
    plt.rcParams['font.family'] = 'sans-serif'
    plt.rcParams['axes.axisbelow'] = True
    
    fig, ax = plt.subplots(figsize=(10, 5))
    
    ax.set_facecolor('white')
    ax.spines['top'].set_visible(False)
    ax.spines['right'].set_visible(False)
    ax.grid(True, axis='y', linestyle='--', alpha=0.5, color='gray')

    unique_pcs = sorted(plot_df['pc'].unique().tolist())
    grouped_df = plot_df.groupby('pc')
    
    for pc in unique_pcs:
        if pc not in grouped_df.groups: continue
        pc_data = grouped_df.get_group(pc)
        
        # ✅ 인자로 받은 color_map과 marker_map을 바로 사용
        color = color_map.get(pc, '#808080') if color_map else '#808080'
        marker = marker_map.get(pc, 'o') if marker_map else 'o'
        
        label_str = f"PC {pc}" if isinstance(pc, (int, np.integer)) else f"PC {pc}"
        
        ax.scatter(
            pc_data['addr_int'], 
            pc_data['active_intervals_count'], 
            color=color, marker=marker, label=label_str,
            alpha=0.7, edgecolors='white', linewidth=0.5, s=20
        )

    ax.set_xlabel('Memory Address (Hex)', fontsize=11, fontweight='bold', labelpad=10)
    ax.set_ylabel('Active Intervals Count (Lifespan)', fontsize=11, fontweight='bold', labelpad=10)
    ax.set_title(f'[{benchmark}] Address Lifespan by Generating PC', fontsize=13, fontweight='bold', pad=15)

    max_interval_count = plot_df['interval'].nunique()
    ax.set_ylim(0, max_interval_count * 1.05) 
    
    ax.xaxis.set_major_locator(ticker.MaxNLocator(nbins=15))
    ax.xaxis.set_major_formatter(ticker.FuncFormatter(lambda x, pos: f"0x{int(x):X}"))

    plt.setp(ax.get_xticklabels(), rotation=45, ha='right', fontsize=9)
    plt.setp(ax.get_yticklabels(), fontsize=9)

    ax.legend(loc='upper left', bbox_to_anchor=(1.02, 1), title='Program Counter (PC)', 
              title_fontsize=10, fontsize=9, frameon=True, edgecolor='lightgray', markerscale=1.5)

    fig.tight_layout()

    out_dir = f"{result_dir}/{benchmark}/{pc_num}/{time_interval}"
    os.makedirs(out_dir, exist_ok=True)
    out_file = f"{out_dir}/{benchmark}_addr_lifespan_scatter.png"
    
    fig.savefig(out_file, dpi=300, bbox_inches='tight', transparent=False)
    plt.close(fig)
    print(f"✅ Saved figure: {out_file}")

# ---------------------------------------------------------
# 4. Address Mean Access by PC
# ---------------------------------------------------------
def plot_addr_mean_access_by_pc(df, result_dir=".", benchmark="benchmark", 
                                time_interval=10000, pc_color_map=None, pc_marker_map=None):
    plot_df = df[['pc', 'addr', 'mean_access_per_interval']].copy()
    
    if plot_df['addr'].dtype == 'O':
        plot_df['addr_int'] = plot_df['addr'].apply(int, base=16)
    else:
        plot_df['addr_int'] = plot_df['addr']

    plt.rcParams['figure.dpi'] = 300
    plt.rcParams['savefig.dpi'] = 300
    plt.rcParams['font.family'] = 'sans-serif'
    plt.rcParams['axes.axisbelow'] = True
    
    fig, ax = plt.subplots(figsize=(10, 5))
    ax.set_facecolor('white')
    ax.spines['top'].set_visible(False)
    ax.spines['right'].set_visible(False)
    ax.grid(True, axis='y', linestyle='--', alpha=0.6, color='lightgray')

    unique_pcs = sorted(plot_df['pc'].unique().tolist())
    grouped_df = plot_df.groupby('pc')
    
    for pc in unique_pcs:
        if pc not in grouped_df.groups: continue
        pc_data = grouped_df.get_group(pc)
        
        # ✅ 인자로 받은 pc_color_map과 pc_marker_map을 바로 사용
        color = pc_color_map.get(pc, '#808080') if pc_color_map else '#808080'
        marker = pc_marker_map.get(pc, 'o') if pc_marker_map else 'o'
        
        label_str = f"PC {pc}" if isinstance(pc, (int, np.integer)) else f"{pc}"
        
        ax.scatter(
            pc_data['addr_int'], 
            pc_data['mean_access_per_interval'], 
            color=color, marker=marker, label=label_str,
            alpha=0.7, edgecolors='white', linewidth=0.5, s=25
        )

    ax.set_xlabel('Memory Address (Hex)', fontsize=11, fontweight='bold', labelpad=10)
    ax.set_ylabel('Mean Access Count per Interval', fontsize=11, fontweight='bold', labelpad=10)
    ax.set_title(f'[{benchmark}] Mean Access Count by Generating PC', fontsize=13, fontweight='bold', pad=15)
    
    ax.xaxis.set_major_locator(ticker.MaxNLocator(nbins=15))
    ax.xaxis.set_major_formatter(ticker.FuncFormatter(lambda x, pos: f"0x{int(x):X}"))

    plt.setp(ax.get_xticklabels(), rotation=45, ha='right', fontsize=9)
    plt.setp(ax.get_yticklabels(), fontsize=9)

    ax.legend(loc='upper left', bbox_to_anchor=(1.02, 1), title='Program Counter (PC)', 
              title_fontsize=10, fontsize=9, frameon=True, edgecolor='lightgray', markerscale=1.5)

    fig.tight_layout()

    out_dir = f"{result_dir}/{benchmark}/{pc_num}/{time_interval}"
    os.makedirs(out_dir, exist_ok=True)
    out_file = f"{out_dir}/{benchmark}_addr_mean_access_scatter.png"
    
    fig.savefig(out_file, dpi=300, bbox_inches='tight', transparent=False)
    plt.close(fig)
    print(f"✅ Saved figure: {out_file}")


if __name__=="__main__":
    time_intervals = [1000, 5000, 10000, 20000, 50000, 100000]

    for benchmark in benchmarks:
        # if benchmark != 'bfs':
            # continue

        trace_path = f"{expanded_dir}/{benchmark}_mesh_6x6_memory_trace.csv"
        if not os.path.exists(trace_path):
            print(f"No trace of {benchmark}")
            continue

        # 1. 트레이스 원본 로드
        trace_df = pd.read_csv(trace_path)
        t_min = trace_df['time'].min()
        time_vals = trace_df['time'].to_numpy()

        result_dir = f"./pc-rdma-metrics-fig"
        os.makedirs(f"{result_dir}", exist_ok=True)
        
        for time_interval in time_intervals:
            rdma_metrics_file = f"./pc-rdma-metrics/{benchmark}/{benchmark}_pc_rdma_metrics_{time_interval}.csv"

            if not os.path.exists(rdma_metrics_file):
                print(f"{benchmark} RDMA metrics ({time_interval}) not exists") 
                continue

            # 2. 메트릭 파일 로드 (여기서 df 정의)
            df = pd.read_csv(rdma_metrics_file, skipinitialspace=True)

            # 3. 메트릭 기반으로 PC 정렬 및 색상/마커 부여 (단일 벤치마크 내 완벽한 일관성)
            pc_totals = df.groupby('pc')['ro_shared_access_mean'].mean()
            top_n = len(pc_totals)
            top_pcs = pc_totals.sort_values(ascending=False).head(top_n).index.tolist()
            pc_num = "all" if top_n == trace_df['pc'].nunique() else f"{top_n}"

            pc_color_map, pc_marker_map, pc_num = get_pc_mapping(df, benchmark, top_n)
            print(f"✅ Generated {pc_num} PCs mapping for {benchmark}")

            # 4. 안전하게 원본 데이터 필터링 (top_pcs가 정의된 후)
            filtered_trace_df = trace_df[trace_df['pc'].isin(top_pcs)].copy()

            # 5. interval 계산 (trace_df에 시간 구간 부여)
            filtered_trace_df['interval'] = ((filtered_trace_df['time'].to_numpy() - t_min) / ns // time_interval) * time_interval

            # 6. 통계량 집계 (필터링된 데이터 사용)
            interval_pc_addr_stats = filtered_trace_df.groupby(['interval', 'pc', 'addr'], as_index=False).agg(
                access_count_in_interval=('access_count', 'sum')
            )

            pc_addr_lifespan = filtered_trace_df.groupby(['pc', 'addr'], as_index=False).agg(
                active_intervals_count=('interval', 'nunique')
            )

            # 수명 데이터 병합
            result_df = interval_pc_addr_stats.merge(pc_addr_lifespan, on=['pc', 'addr'], how='left')
            result_df = result_df.sort_values(by=['interval'])

            # 평균 접근 횟수 집계
            access_mean_per_interval = result_df.groupby(['pc', 'addr'], as_index=False).agg(
                mean_access_per_interval=('access_count_in_interval', 'mean'),
                std_access_per_interval=('access_count_in_interval', 'std')
            )

            # 7. 그래프 그리기 (모두 동일한 인자명 color_map, marker_map, 혹은 pc_color_map, pc_marker_map을 정확히 맞춰서 전달)
            plot_addr_lifespan_by_pc(df=result_df, benchmark=benchmark, 
                                    result_dir=result_dir, time_interval=time_interval,
                                    pc_num=pc_num, color_map=pc_color_map, marker_map=pc_marker_map)

            plot_addr_mean_access_by_pc(df=access_mean_per_interval, benchmark=benchmark, time_interval=time_interval,
                                        result_dir=result_dir, pc_color_map=pc_color_map, pc_marker_map=pc_marker_map)

        pc_totals = df.groupby('pc')['ro_shared_access_mean'].mean()
        top_n = len(pc_totals)
        top_pcs = pc_totals.sort_values(ascending=False).head(top_n).index.tolist()
        pc_color_map, pc_marker_map, pc_num = get_pc_mapping(df, benchmark, top_n)

        plot_memory_access_pattern(df=filtered_trace_df, benchmark=benchmark, 
                                    result_dir=result_dir, color_map=pc_color_map, marker_map=pc_marker_map)
        
        # 8. Address 단위 추가 분석 (curr_chip_cols 활용)
        curr_chip_cols = [str(i) for i in range(36)]
        agg_dict = {col: 'sum' for col in curr_chip_cols}
        agg_dict['owner'] = 'first' 
        agg_dict['access_count'] = 'sum'

        addr_df = filtered_trace_df.groupby(['pc', 'addr']).agg(agg_dict).reset_index()
        addr_df['sharing_degree'] = (addr_df[curr_chip_cols] != 0).sum(axis=1)

        # plot_address_metrics에도 누락되었던 marker_map 추가
        plot_address_metrics(df=addr_df, benchmark=benchmark, 
                            result_dir=result_dir, color_map=pc_color_map, marker_map=pc_marker_map,
                            value="access_count")
                        
        plot_address_metrics(df=addr_df, benchmark=benchmark, 
                            result_dir=result_dir, color_map=pc_color_map, marker_map=pc_marker_map,
                            value="sharing_degree")