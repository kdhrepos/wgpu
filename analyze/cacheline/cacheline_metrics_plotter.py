import pandas as pd
import re
import os
username = os.path.expanduser('~')
import math
import numpy as np
import matplotlib.pyplot as plt
import matplotlib.patches as mpatches
import seaborn as sns
import matplotlib.lines as mlines
import matplotlib.ticker as ticker


def plot_stack_distance(
    df, 
    benchmark, 
    result_dir=f'{username}/mgvm/analyze/_out/cacheline/',
    hot_threshold_pct=1, 
    global_threshold_pct=10, 
    save_fig=True,
): 
    # --- [1단계] 데이터 정렬 및 그룹화 (기존 동일) ---
    df = df.sort_values(by=['cycle']).reset_index(drop=True)
    
    global_counts = df.groupby('addr').size().sort_values(ascending=False).reset_index(name='access_count')
    total_unique_addrs = len(global_counts)
    
    top_n = int(total_unique_addrs * (hot_threshold_pct / 100))
    mid_n = int(total_unique_addrs * (global_threshold_pct / 100))
    
    heat_map = {addr: 'Other (Cold)' for addr in global_counts['addr']}
    for addr in global_counts.iloc[:mid_n]['addr']:
        heat_map[addr] = 'Mid Hot (Top 10%)'
    for addr in global_counts.iloc[:top_n]['addr']:
        heat_map[addr] = 'Top Hot (Top 1%)'

    df['heat_group'] = df['addr'].map(heat_map)

    # --- [2단계] BIT(Fenwick Tree)를 이용한 고속 Stack Distance 계산 ---
    # Naive iteration 대비 O(N log N)으로 압도적으로 빠릅니다.
    print(f"Calculating Stack Distance (Fast BIT) for {benchmark}...")
    df['stack_distance'] = np.nan

    for chiplet_id, group in df.groupby('chiplet'):
        idx_list = group.index.values
        addrs = group['addr'].values
        n = len(addrs)
        
        # 주소 문자열/숫자를 0부터 시작하는 정수로 인코딩
        unique_addrs, addr_map = np.unique(addrs, return_inverse=True)
        
        bit = np.zeros(n + 1, dtype=np.int32)
        last_pos = np.full(len(unique_addrs), -1, dtype=np.int32)
        dist_results = np.full(n, np.nan)

        # BIT functions
        def _update(i, delta):
            i += 1
            while i <= n:
                bit[i] += delta
                i += i & (-i)

        def _query(i):
            i += 1
            s = 0
            while i > 0:
                s += bit[i]
                i -= i & (-i)
            return s

        # 핵심 로직: 이전 위치를 BIT에서 빼고 현재 위치를 더하며 구간합 계산
        for i in range(n):
            a_idx = addr_map[i]
            prev = last_pos[a_idx]
            
            if prev != -1:
                # (prev + 1) ~ (i - 1) 사이의 유니크한 주소 개수 쿼리
                dist_results[i] = _query(i - 1) - _query(prev)
                _update(prev, -1) # 이전 위치 마킹 제거
            
            _update(i, 1) # 현재 위치 마킹 추가
            last_pos[a_idx] = i
        
        df.loc[idx_list, 'stack_distance'] = dist_results

    # --- [3단계] 시각화 데이터 정리 ---
    plot_df = df.dropna(subset=['stack_distance']).copy()
    plot_df['stack_distance'] = plot_df['stack_distance'].astype(float)
    plot_df = plot_df[plot_df['stack_distance'] > 0]

    # --- [4단계] 히스토그램 시각화 (기존 동일) ---
    plt.figure(figsize=(12, 8))
    palette = {'Top Hot (Top 1%)': 'crimson', 'Mid Hot (Top 10%)': 'orange', 'Other (Cold)': 'silver'}

    ax = sns.histplot(
        data=plot_df, x='stack_distance', hue='heat_group', 
        hue_order=['Other (Cold)', 'Mid Hot (Top 10%)', 'Top Hot (Top 1%)'],
        palette=palette, bins=100, log_scale=True, alpha=0.7,
        edgecolor='black', linewidth=0.5, multiple="stack" 
    )
    
    # Top Hot 중앙값 및 범례 로직
    top_data = plot_df[plot_df['heat_group'] == 'Top Hot (Top 1%)']
    if not top_data.empty:
        top_median = top_data['stack_distance'].median()
        line = plt.axvline(top_median, color='crimson', linestyle='--', linewidth=2, 
                           label=f'Top Hot Median: {top_median:.0f}')

    handles, labels = ax.get_legend_handles_labels()
    desired_order = ['Top Hot (Top 1%)', 'Mid Hot (Top 10%)', 'Other (Cold)']
    final_handles = [handles[labels.index(name)] for name in desired_order if name in labels]
    final_labels = [name for name in desired_order if name in labels]
    
    if not top_data.empty:
        final_handles.append(line); final_labels.append(line.get_label())

    plt.legend(final_handles, final_labels, title="Access Rank", loc='upper left', bbox_to_anchor=(1, 1))
    plt.title(f'[{benchmark}] Stack Distance \nUnique Cachelines: {total_unique_addrs:,}\nAccesses: {len(df):,}', fontweight='bold')
    plt.xlabel('Stack Distance (Log Scale)'); plt.ylabel('Access Count'); plt.grid(True, which="both", alpha=0.2)
    plt.tight_layout()

    if save_fig:
        os.makedirs(result_dir, exist_ok=True)
        plt.savefig(f"{result_dir}/{benchmark}_stack_distance.png", dpi=400)
    
    plt.close()


def plot_reuse_interval(
    df,
    benchmark,
    result_dir,
    hot_threshold_pct=10, # 상위 10% 접근 빈도 주소를 Hot data로 정의
    global_threshold_pct=1, # 전체 데이터 중 상위 1% 이상 접근 빈도 주소를 Global Hot data로 정의
    save_fig=True,
):
    # 2. 주소 개수 기반 상위 N% 식별
    global_counts = df.groupby('addr').size().sort_values(ascending=False).reset_index(name='access_count')
    total_unique_addrs = len(global_counts)
    
    top_n = int(total_unique_addrs * (hot_threshold_pct / 100))
    mid_n = int(total_unique_addrs * (global_threshold_pct / 100))
    
    # 매핑 (Top Hot을 가장 나중에 덮어씌워 우선순위 확보)
    heat_map = {addr: 'Other (Cold)' for addr in global_counts['addr']}
    for addr in global_counts.iloc[:mid_n]['addr']:
        heat_map[addr] = 'Mid Hot (Top 10%)'
    for addr in global_counts.iloc[:top_n]['addr']:
        heat_map[addr] = 'Top Hot (Top 1%)'

    df['heat_group'] = df['addr'].map(heat_map)

    # 3. Reuse Interval 계산
    df = df.sort_values(by=['chiplet', 'addr', 'cycle'])
    df['reuse_interval'] = df.groupby(['chiplet', 'addr'])['cycle'].diff()

    # 4. 데이터 준비
    plot_df = df.dropna(subset=['reuse_interval'])
    plot_df = plot_df[plot_df['reuse_interval'] > 0]
    never_reused_ratio = df['reuse_interval'].isna().mean() * 100

    # 5. 히스토그램 그리기
    plt.figure(figsize=(12, 8))
    
    palette = {
        'Top Hot (Top 1%)': 'crimson',
        'Mid Hot (Top 10%)': 'orange',
        'Other (Cold)': 'silver'
    }

    # hue_order: 뒤에 적힌 것일수록 나중에 그려져서 '앞'으로 옵니다.
    # Top Hot이 가장 나중에 그려지도록 설정
    ax = sns.histplot(
        data=plot_df, 
        x='reuse_interval', 
        hue='heat_group', 
        hue_order=['Other (Cold)','Mid Hot (Top 10%)', 'Top Hot (Top 1%)'],
        palette=palette, 
        bins=100, 
        log_scale=True, 
        alpha=0.7, # 겹침을 보기 위해 불투명도 약간 상향
        edgecolor='black',
        linewidth=0.5,
        multiple="stack" # 쌓아올림
    )
    
    # 중앙값 선 추가
    top_median = plot_df[plot_df['heat_group'] == 'Top Hot (Top 1%)']['reuse_interval'].median()
    line = plt.axvline(top_median, color='crimson', linestyle='--', linewidth=2, 
                       label=f'Top Hot Median: {top_median:.0e}')

    # sns.histplot의 핸들과 라벨을 가져옴
    handles, labels = ax.get_legend_handles_labels()
    
    # [수정] 인덱스 에러 방지용 안전한 정렬 로직
    # 원래 우리가 원했던 순서
    desired_order = ['Top Hot (Top 1%)', 'Mid Hot (Top 10%)', 'Other (Cold)']
    
    final_handles = []
    final_labels = []

    # 원하는 순서대로 순회하면서, 실제로 생성된 라벨이 있을 때만 추가
    for label_name in desired_order:
        if label_name in labels:
            idx = labels.index(label_name)
            final_handles.append(handles[idx])
            final_labels.append(labels[idx])

    # 중앙값 선(line) 추가
    final_handles.append(line)
    final_labels.append(line.get_label())

    # 범례 설정
    plt.legend(final_handles, final_labels, title="Access Rank (High -> Low)", 
               loc='upper left', bbox_to_anchor=(1, 1), frameon=True)

    # 스타일링
    plt.title(f'[{benchmark}] Reuse Interval Distribution\n'
              f'Total Lines: {total_unique_addrs:,} | Never Reused: {never_reused_ratio:.1f}%', 
              fontsize=14, fontweight='bold', pad=15)
    plt.xlabel('Reuse Interval (Cycles, Log Scale)', fontsize=12)
    plt.ylabel('Access Count', fontsize=12)
    plt.grid(True, which="both", ls="--", alpha=0.2)
    plt.tight_layout()

    if save_fig:
        os.makedirs(result_dir, exist_ok=True)
        save_path = f"{result_dir}/{benchmark}_reuse_interval.png"
        plt.savefig(save_path, dpi=400)
        print(f"Saved: {save_path}")
    
    plt.close()