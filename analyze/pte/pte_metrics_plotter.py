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
from matplotlib.ticker import MaxNLocator


def plot_pte_metrics(df, benchmark, result_dir):
    # 0. 데이터 전처리 (상위 N개만 추출하거나 전체를 쓰되 라벨만 조절)
    # 여기서는 가독성을 위해 상위 100개 정도로 제한하는 것을 추천하지만, 
    # 전체 데이터를 다 그린다면 아래 라벨 조절 로직이 핵심입니다.
    df = df.sort_values('pte').reset_index(drop=True)

    # 1. 스타일 설정
    sns.set_theme(style="ticks")
    plt.rcParams['font.family'] = 'sans-serif'

    # 2. 그래프 생성
    fig, ax1 = plt.subplots(figsize=(14, 7))

    # --- 왼쪽 축: Access Count (Bar) ---
    sns.barplot(
        data=df, x=df.index, y='access_count', hue='level', 
        ax=ax1, palette='viridis', alpha=0.7, edgecolor='black', dodge=False
    )
    ax1.set_ylabel('Access Count (Bar)', fontweight='bold', fontsize=12)
    ax1.set_xlabel('PTE Address', fontweight='bold', fontsize=12)
    
    # [핵심 수정] X축 라벨 개수 제한
    # 촘촘한 라벨 대신 약 20개 정도의 대표 라벨만 표시
    ax1.xaxis.set_major_locator(MaxNLocator(nbins=20)) 
    
    # 틱 위치에 맞는 실제 PTE 주소 값을 매핑
    ticks = ax1.get_xticks().astype(int)
    # 인덱스 범위를 벗어나지 않는 선에서 라벨 설정
    valid_ticks = [t for t in ticks if t < len(df)]
    ax1.set_xticks(valid_ticks)
    ax1.set_xticklabels([df['pte'].iloc[t] for t in valid_ticks], rotation=45, ha='right', fontsize=9)

    if not df['access_count'].empty:
        ax1.set_ylim(0, df['access_count'].max() * 1.15)

    # --- 오른쪽 축: Shared Chiplets (Line) ---
    ax2 = ax1.twinx()
    sns.lineplot(
        x=df.index, y=df['shared_chiplets'], 
        ax=ax2, 
        color='#d62728', 
        marker='o', 
        markersize=4, 
        linestyle='none',  # [수정] 선 연결을 하지 않음
        label='Shared Chiplets'
    )
    ax2.set_ylabel('Shared Chiplets (Scatter)', color='#d62728', fontweight='bold', fontsize=12)
    ax2.tick_params(axis='y', labelcolor='#d62728')
    ax2.grid(False)
    
    if not df['shared_chiplets'].empty:
        ax2.set_ylim(0, df['shared_chiplets'].max() * 1.15)

    # 3. 범례 통합 및 차트 밖으로 배치
    lines, labels = ax1.get_legend_handles_labels()
    lines2, labels2 = ax2.get_legend_handles_labels()

    if ax1.get_legend(): ax1.get_legend().remove()
    if ax2.get_legend(): ax2.get_legend().remove()
    
    ax1.legend(lines + lines2, labels + labels2, title='Metrics & Level', 
               bbox_to_anchor=(1.12, 1), loc='upper left', frameon=True)

    plt.title(f'PTE Metrics Analysis: {benchmark}, \n # PTEs: {df["pte"].nunique()}', fontsize=16, fontweight='bold', pad=25)
    sns.despine(right=False)
    
    # 4. 저장 및 레이아웃
    os.makedirs(result_dir, exist_ok=True)
    out_file = os.path.join(result_dir, f"{benchmark}_pte_metrics.png")
    
    plt.tight_layout()
    fig.subplots_adjust(right=0.82) 

    plt.savefig(out_file, bbox_inches='tight', dpi=400)
    plt.close(fig)
    
    print(f"✅ Saved plot with optimized X-axis: {out_file}")