import pandas as pd
import re
import os
import math
import numpy as np
import matplotlib.pyplot as plt
import matplotlib.patches as mpatches
import seaborn as sns
import matplotlib.lines as mlines
import matplotlib.ticker as ticker


def plot_memory_access_pattern(
    df, 
    benchmark, 
    result_dir, 
    color_map=None, 
    marker_map=None, 
    max_legend_items=15
):
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
        c = color_map.get(pc) if color_map else '#808080'
        m = marker_map.get(pc) if marker_map else 'o'
        
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
    
    plt.savefig(out_file, dpi=400, bbox_inches='tight', transparent=False)
    plt.close()
    print(f"✅ Saved Memory Access Pattern plot: {out_file}")


def plot_address_metrics(
    df, 
    benchmark, 
    result_dir, 
    color_map=None, 
    marker_map=None, 
    value='access_count', 
    max_legend_items=15
):
    if df.empty:
        print(f"Skip: {benchmark} trace data is empty.")
        return

    # 1. 데이터 집계 (PC, Addr 별로 합산)
    # df에 이미 value(access_count)가 있으므로 sum으로 집계합니다.
    plot_df = df.groupby(['pc', 'addr'])[value].sum().reset_index()

    # 주소 타입 변환 (안정성 강화)
    if plot_df['addr'].dtype == 'O':
        plot_df['addr_int'] = plot_df['addr'].apply(lambda x: int(str(x), 16) if isinstance(x, str) else x)
    else:
        plot_df['addr_int'] = plot_df['addr']

    # 2. 스타일 설정
    sns.set_theme(style="whitegrid", rc={"axes.edgecolor": ".15"})
    fig, ax = plt.subplots(figsize=(15, 6), dpi=300)

    # 데이터가 많은 PC 순으로 정렬 (그리기 순서 및 범례 순서 최적화)
    pc_totals = plot_df.groupby('pc')[value].sum().sort_values(ascending=False)
    sorted_pcs = pc_totals.index.tolist()

    # 기본 컬러/마커 설정 (인자가 None일 경우 대비)
    default_palette = sns.color_palette("husl", len(sorted_pcs))
    default_markers = ['o', 's', '^', 'v', '<', '>', 'D', 'p']

    # 3. 데이터 렌더링 루프
    for i, pc in enumerate(sorted_pcs):
        group_data = plot_df[plot_df['pc'] == pc]
        
        # ✅ 인자로 받은 맵에 해당 PC가 없으면 기본값 사용 (에러 방지)
        c = color_map.get(pc) if (color_map and pc in color_map) else default_palette[i % len(default_palette)]
        m = marker_map.get(pc) if (marker_map and pc in marker_map) else default_markers[i % len(default_markers)]
        
        # 레이블 형식 (정수는 16진수, 아니면 그대로)
        label = f"{pc}" if isinstance(pc, (int, np.integer)) else f"{pc}"

        ax.scatter(group_data['addr_int'], group_data[value], 
                   color=c, marker=m, label=label, 
                   s=35.0, alpha=0.6, edgecolors='none',
                   rasterized=True) # 점이 많을 때 저장 속도/용량 최적화
    
    # 4. 축 및 포맷 설정
    clean_val_name = value.replace('_', ' ').title()
    ax.set_title(f"[{benchmark}] {clean_val_name} by Address", fontsize=15, fontweight='bold', pad=15)
    ax.set_xlabel("Memory Address (Hex)", fontsize=12, fontweight='medium')
    ax.set_ylabel(f"Total {clean_val_name}", fontsize=12, fontweight='medium')

    ax.xaxis.set_major_formatter(ticker.FuncFormatter(lambda x, pos: f"0x{int(x):X}"))
    plt.xticks(rotation=45, ha='right') 

    # Y축 범위 (0부터 시작)
    ax.set_ylim(0, plot_df[value].max() * 1.1) 

    # 5. 범례(Legend) 최적화
    handles, labels = ax.get_legend_handles_labels()
    
    if len(handles) > max_legend_items:
        hidden_count = len(handles) - max_legend_items
        handles = handles[:max_legend_items]
        labels = labels[:max_legend_items]
        
        empty_handle = mlines.Line2D([], [], color='none', marker='none', linestyle='None')
        handles.append(empty_handle)
        labels.append(f"... and {hidden_count} more")

    legend = ax.legend(handles, labels, title="PCs", bbox_to_anchor=(1.02, 1), loc='upper left') 
    legend.get_title().set_fontweight('bold')
    
    # 범례 아이콘 투명도 제거 (시인성)
    for handle in legend.legend_handles:
        if hasattr(handle, 'set_alpha'): handle.set_alpha(1.0)

    plt.tight_layout()

    # 6. 저장
    out_dir = os.path.join(result_dir, benchmark)
    os.makedirs(out_dir, exist_ok=True)
    out_file = os.path.join(out_dir, f"{benchmark}_{value}_by_address.png")
    
    plt.savefig(out_file, bbox_inches='tight')
    plt.close()
    print(f"✅ Saved plot: {out_file}")