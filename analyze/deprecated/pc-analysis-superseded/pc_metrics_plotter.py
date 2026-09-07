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

def get_pc_mapping(df, benchmark, top_n=10):

    np.random.seed(42)
    
    pc_totals = df.groupby('pc').agg({
        'access_count': 'mean'
    }).sort_values(by='access_count', ascending=False)
    top_pcs = pc_totals.head(top_n).index.tolist()
    
    global_colors = sns.color_palette("tab20", len(top_pcs))
    # 🎯 matplotlib이 바로 인식하는 형태로 변환
    pc_color_map = {pc: global_colors[i] for i, pc in enumerate(top_pcs)}  # list[float] 형태
    MARKER_LIST = ['o', 's', '^', 'D', 'v', '<', '>', 'p', '*', 'h']
    pc_marker_map = {pc: MARKER_LIST[i % len(MARKER_LIST)] for i, pc in enumerate(top_pcs)}
    
    return pc_color_map, pc_marker_map, len(top_pcs)


def plot_scatter(
        df, 
        benchmark, 
        result_dir, 
        value, 
        time_interval=10000, 
        top_pcs=None, 
        pc_num="all", 
        color_map=None, 
        marker_map=None, 
        max_legend_items=15
):
    if df.empty:
        print(f"Skip: {benchmark} data is empty.")
        return

    target_cols = ['interval', 'pc', f'{value}']
    if f'{value}' not in df.columns:
        print(f"Skip: f'{value}' not found in {benchmark}")
        return

    df_sub = df[target_cols].copy()
    
    if top_pcs is not None:
        df_sub = df_sub[df_sub['pc'].isin(top_pcs)]
    
    pivot_df = df_sub.pivot(index='interval', columns='pc', values=f'{value}')

    # 데이터가 많은 순(sum 기준)으로 정렬
    if top_pcs is not None:
        ordered_cols = [pc for pc in top_pcs if pc in pivot_df.columns]
        pivot_df = pivot_df[ordered_cols]
    else:
        pc_sums = pivot_df.sum().sort_values(ascending=False)
        pivot_df = pivot_df[pc_sums.index]

    sns.set_theme(style="whitegrid", rc={"axes.edgecolor": ".15", "xtick.bottom": True, "ytick.left": True})
    fig, ax = plt.subplots(figsize=(10, 4.5))

    unique_pcs = pivot_df.columns # 정렬된 PC 리스트

    # 정렬된 순서대로 플로팅하므로 범례 최상단에는 항상 데이터가 가장 많은 PC가 위치함
    for i, pc_val in enumerate(unique_pcs):
        # ✅ 변경: 외부에서 전달받은 color_map과 marker_map을 완벽하게 적용 (Fallback 포함)
        c = color_map.get(pc_val) if color_map else '#808080'
        m = marker_map.get(pc_val) if marker_map else 'o'
        
        # 라벨 설정
        label_str = f"PC {pc_val}" if isinstance(pc_val, (int, np.integer)) else f"{pc_val}"
        
        ax.scatter(pivot_df.index, pivot_df[pc_val], 
                   color=c, marker=m, s=15, alpha=0.8, label=label_str) # 점 크기(s) 살짝 상향 (5->15)

    clean_val_name = value.replace('_', ' ').title()
    ax.set_title(f"[{benchmark}] {clean_val_name} Over Time, ({pc_num}) PCs", fontsize=14, fontweight='bold', pad=15)
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

    # 범례 자르기 로직
    handles, labels = ax.get_legend_handles_labels()
    
    if len(handles) > max_legend_items:
        hidden_count = len(handles) - max_legend_items
        handles = handles[:max_legend_items]
        labels = labels[:max_legend_items]
        
        empty_handle = mlines.Line2D([], [], color='none', marker='none', linestyle='None')
        handles.append(empty_handle)
        labels.append(f"... and {hidden_count} more")
    
    legend = ax.legend(handles, labels,
              title="PCs", title_fontsize='10',
              bbox_to_anchor=(1.02, 1), loc='upper left',
              fontsize=9, frameon=True, edgecolor='0.8', markerscale=2) # 점이 커졌으므로 markerscale을 3->2로 조정
    legend.get_title().set_fontweight('bold')

    plt.tight_layout()

    out_dir = f"{result_dir}/{benchmark}/{pc_num}/{time_interval}"
    os.makedirs(out_dir, exist_ok=True)
    
    out_file = f"{out_dir}/{benchmark}_{value}_scatter.png"
    plt.savefig(out_file, dpi=300, bbox_inches='tight', transparent=False)
    plt.close(fig)

    print(f"✅ Saved plot: {out_file}")


def plot_access_mean_per_owner(
        df, 
        benchmark, 
        result_dir, 
        owner=0, 
        time_interval=10000, 
        color_map=None, 
        marker_map=None, 
        max_legend_items=15
):
    import os
    import pandas as pd
    import numpy as np
    import matplotlib.pyplot as plt
    import matplotlib.lines as mlines
    import seaborn as sns

    if df.empty:
        print(f"Skip: {benchmark} data is empty.")
        return

    # 1. 데이터 집계 및 전처리
    # time_interval (ns) 단위를 반영하여 binning
    ns = 1e-9
    window_size = time_interval * ns 
    df = df.copy()
    df['time_bin'] = (df['time'] // window_size) * window_size

    # 시간, 칩렛(owner), PC별 집계
    plot_df = df.groupby(['time_bin', 'owner', 'pc']).agg(
        access_count=('access_count', 'sum'),
        unique_cache_lines=('addr', 'nunique')
    ).reset_index()
    
    # Access Mean 계산
    plot_df['access_mean'] = plot_df['access_count'] / plot_df['unique_cache_lines']
    
    # 특정 Owner 필터링
    plot_df = plot_df[plot_df['owner'] == owner]
    
    if plot_df.empty:
        print(f"Skip: No data for Owner {owner} in {benchmark}")
        return

    # 2. 피벗 및 정렬 (데이터가 많은 순서대로 범례 상단 배치)
    pivot_df = plot_df.pivot(index='time_bin', columns='pc', values='access_mean')
    pc_sums = pivot_df.sum().sort_values(ascending=False)
    ordered_pcs = pc_sums.index

    # 3. 그래프 스타일 설정
    sns.set_theme(style="whitegrid", rc={"axes.edgecolor": ".15", "xtick.bottom": True, "ytick.left": True})
    fig, ax = plt.subplots(figsize=(10, 4.5), dpi=300)

    # 4. 매핑 정보를 활용한 플로팅
    for pc_val in ordered_pcs:
        c = color_map.get(pc_val) if color_map else '#808080'
        m = marker_map.get(pc_val) if marker_map else 'o'
        
        label_str = f"PC {int(pc_val)}" if isinstance(pc_val, (int, np.integer, float)) else f"{pc_val}"
        
        ax.scatter(pivot_df.index, pivot_df[pc_val], 
                   color=c, marker=m, s=15, alpha=0.8, label=label_str)

    # 5. 축 및 레이아웃 디자인
    ax.set_title(f"[{benchmark}] Owner {owner} Access Mean Over Time", fontsize=14, fontweight='bold', pad=15)
    ax.set_xlabel("Time Interval (ns)", fontsize=11, fontweight='medium')
    ax.set_ylabel("Ro Shared Access Mean", fontsize=11, fontweight='medium')

    # Y축 스케일 설정 (이미지 특성 반영)
    actual_max = plot_df['access_mean'].max()
    if pd.isna(actual_max) or actual_max == 0:
        plt.close(fig)
        return
    
    ax.set_ylim(0, actual_max * 1.1)
    ax.set_yscale('symlog', linthresh=1.0)
    
    ax.grid(axis='y', linestyle='--', linewidth=0.7, alpha=0.5)
    ax.grid(axis='x', visible=False)
    sns.despine(left=False, bottom=False)

    # 6. 범례 자르기 로직 (plot_scatter와 동일)
    handles, labels = ax.get_legend_handles_labels()
    if len(handles) > max_legend_items:
        hidden_count = len(handles) - max_legend_items
        handles = handles[:max_legend_items]
        labels = labels[:max_legend_items]
        
        empty_handle = mlines.Line2D([], [], color='none', marker='none', linestyle='None')
        handles.append(empty_handle)
        labels.append(f"... and {hidden_count} more")
    
    legend = ax.legend(handles, labels,
                       title="PCs", title_fontsize='10',
                       bbox_to_anchor=(1.02, 1), loc='upper left',
                       fontsize=9, frameon=True, edgecolor='0.8', markerscale=2)
    legend.get_title().set_fontweight('bold')

    plt.tight_layout()

    # 7. 저장
    out_dir = f"{result_dir}/{benchmark}/owner_{owner}/{time_interval}"
    os.makedirs(out_dir, exist_ok=True)
    out_file = f"{out_dir}/{benchmark}_access_mean_scatter.png"
    
    plt.savefig(out_file, dpi=300, bbox_inches='tight', transparent=False)
    plt.close(fig)

    print(f"✅ Saved Access Mean plot: {out_file}")


def plot_cumulative_access_per_owner(
        df, benchmark, result_dir, owner=0, time_interval=10000, 
        color_map=None, marker_map=None, max_legend_items=15
):
    import os
    import pandas as pd
    import numpy as np
    import matplotlib.pyplot as plt
    import matplotlib.lines as mlines
    import seaborn as sns

    if df.empty: return

    # --- 1. 누적 데이터 계산 (중요) ---
    df = df.sort_values('time').copy()
    
    # 캐시 라인 주소 생성 (64B 기준)
    df['line_addr'] = df['addr'] // 64

    # [Cumulative Access Count] PC별 접근 횟수 누적 합
    df['cum_access_count'] = df.groupby(['owner', 'pc'])['access_count'].cumsum()

    # [Cumulative Unique Lines] 해당 PC가 이 주소를 처음 봤는지 체크 (Working Set 확장)
    # subset에 addr가 포함되어야 처음 등장하는 주소만 1로 카운트됨
    df['is_first_touch'] = ~df.duplicated(subset=['owner', 'pc', 'line_addr'])
    df['cum_unique_lines'] = df.groupby(['owner', 'pc'])['is_first_touch'].cumsum()

    # --- 2. 시간대별(Interval) 샘플링 ---
    ns = 1e-9
    window_size = time_interval * ns
    df['time_bin'] = (df['time'] // window_size) * window_size

    # 각 bin의 마지막 시점의 누적 값을 가져옴 (마지막 상태가 그 시간대의 누적값이므로)
    plot_df = df[df['owner'] == owner].groupby(['time_bin', 'pc']).agg({
        'cum_access_count': 'last',
        'cum_unique_lines': 'last'
    }).reset_index()

    # 누적 평균 접근 횟수 계산
    # $$ \text{Access Mean}_{cum} = \frac{\sum \text{Access Count}}{\text{Total Unique Lines Seen So Far}} $$
    plot_df['access_mean'] = plot_df['cum_access_count'] / plot_df['cum_unique_lines']

    # --- 3. 그래프 스타일 및 플로팅 (기존 scatter 구조 유지) ---
    sns.set_theme(style="whitegrid", rc={"axes.edgecolor": ".15", "xtick.bottom": True, "ytick.left": True})
    fig, ax = plt.subplots(figsize=(10, 4.5), dpi=300)

    # 데이터가 많은 순서대로 정렬 (범례용)
    pc_sums = plot_df.groupby('pc')['access_mean'].last().sort_values(ascending=False)
    ordered_pcs = pc_sums.index

    for pc_val in ordered_pcs:
        c = color_map.get(pc_val) if color_map else '#808080'
        m = marker_map.get(pc_val) if marker_map else 'o'
        label_str = f"PC {int(pc_val)}" if isinstance(pc_val, (int, np.integer, float)) else f"{pc_val}"
        
        subset = plot_df[plot_df['pc'] == pc_val]
        ax.scatter(subset['time_bin'], subset['access_mean'], 
                   color=c, marker=m, s=15, alpha=0.8, label=label_str)

    # --- 4. 축 및 디자인 ---
    ax.set_title(f"[{benchmark}] Owner {owner} Cumulative Access Mean", fontsize=14, fontweight='bold', pad=15)
    ax.set_xlabel("Time Interval (ns)", fontsize=11)
    ax.set_ylabel("Cumulative Access Mean", fontsize=11)
    
    ax.set_yscale('symlog', linthresh=1.0)
    ax.grid(axis='y', linestyle='--', alpha=0.5)
    sns.despine(left=False, bottom=False)

    # 범례 로직 (max_legend_items 반영)
    handles, labels = ax.get_legend_handles_labels()
    if len(handles) > max_legend_items:
        hidden_count = len(handles) - max_legend_items
        handles, labels = handles[:max_legend_items], labels[:max_legend_items]
        handles.append(mlines.Line2D([], [], color='none', marker='none'))
        labels.append(f"... and {hidden_count} more")
    
    ax.legend(handles, labels, title="PCs", bbox_to_anchor=(1.02, 1), loc='upper left', frameon=True, markerscale=2)

    plt.tight_layout()
    
    # 저장
    out_dir = f"{result_dir}/{benchmark}/owner_{owner}/cumulative"
    os.makedirs(out_dir, exist_ok=True)
    plt.savefig(f"{out_dir}/{benchmark}_cum_access_mean.png", bbox_inches='tight')
    plt.close(fig)

    print(f"✅ Saved Cumulative Access Mean plot: {out_dir}/{benchmark}_cum_access_mean.png")