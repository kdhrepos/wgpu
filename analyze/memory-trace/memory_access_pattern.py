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

from _paths import memory_trace_dir

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

def compute_sharing_status(df):
    """
    원본 데이터에서 실시간으로 RO Shared / RW Shared / Private 판별
    """
    curr_chip_cols = [str(i) for i in range(36)]
    
    # addr별 집계 (전체 데이터 대상)
    agg_dict = {col: 'sum' for col in curr_chip_cols}
    agg_dict['rw'] = 'max'
    agg_dict['owner'] = 'first'
    
    addr_agg = df.groupby('addr').agg(agg_dict).reset_index()
    
    # NumPy 기반 벡터화 마스킹 (owner 접근 0으로 치환)
    owner_vals = np.array(addr_agg['owner'], dtype=str)[:, None]
    col_vals = np.array(curr_chip_cols, dtype=str)
    is_owner = owner_vals == col_vals
    addr_agg[curr_chip_cols] = addr_agg[curr_chip_cols].mask(is_owner, 0)
    
    # Sharing 여부 판별
    addr_agg['is_shared'] = (addr_agg[curr_chip_cols] != 0).any(axis=1)
    
    # 원본 데이터에 병합
    df = df.merge(addr_agg[['addr', 'is_shared']], on='addr', how='left')
    
    return df

def plot_memory_access_pattern(df, benchmark):
    """
    Time vs Addr 접근 패턴 시각화 (RO Shared / RW Shared / Private 3가지)
    """
    # is_shared 컬럼이 없으면 실시간 계산
    if 'is_shared' not in df.columns:
        print(f"Computing sharing status for {benchmark}...")
        df = compute_sharing_status(df)

    # 시간/주소 정규화
    df_plot = df.copy()
    t_min = df_plot['time'].min()
    df_plot['norm_time'] = (df_plot['time'] - t_min) * 1e-9
    
    addr_min, addr_max = df_plot['addr'].min(), df_plot['addr'].max()
    df_plot['norm_addr'] = (df_plot['addr'] - addr_min) / (addr_max - addr_min)

    # 논문용 고품질 세팅
    sns.set_theme(style="white", rc={
        "axes.edgecolor": ".15", "xtick.bottom": True, "ytick.left": True, "font.size": 10
    })
    
    fig, ax = plt.subplots(figsize=(12, 6))

    FIXED_SIZE = 25

    # 1. RO Shared (읽기 + 공유)
    ro_shared_mask = (df_plot['rw'] == 0) & (df_plot['is_shared'] == True)
    ax.scatter(df_plot.loc[ro_shared_mask, 'norm_time'], df_plot.loc[ro_shared_mask, 'norm_addr'],
               c='#1f77b4', s=FIXED_SIZE, alpha=0.7, label='RO Shared', 
               edgecolors='white', linewidth=0.5)

    # 2. RW Shared (읽기/쓰기 + 공유)
    rw_shared_mask = (df_plot['rw'] == 1) & (df_plot['is_shared'] == True)
    ax.scatter(df_plot.loc[rw_shared_mask, 'norm_time'], df_plot.loc[rw_shared_mask, 'norm_addr'],
               c='#ff7f0e', s=FIXED_SIZE, alpha=0.7, label='RW Shared', 
               edgecolors='white', linewidth=0.5)

    # 3. Private (공유되지 않은 접근, 주로 RO)
    private_mask = (df_plot['is_shared'] == False)
    ax.scatter(df_plot.loc[private_mask, 'norm_time'], df_plot.loc[private_mask, 'norm_addr'],
               c='#9f0eff', s=FIXED_SIZE, alpha=0.7, label='Private', 
               edgecolors='white', linewidth=0.5)

    # 그래프 꾸미기
    ax.set_title(f"({benchmark}) Memory Access Pattern", fontsize=16, fontweight='bold', pad=15)
    ax.set_xlabel("Normalized Time (ns)", fontsize=12, fontweight='medium')
    ax.set_ylabel("Normalized Address", fontsize=12, fontweight='medium')
    
    ax.set_xlim(0, df_plot['norm_time'].max())
    ax.set_ylim(0, 1)

    ax.grid(True, linestyle='--', linewidth=0.7, alpha=0.3)
    sns.despine(left=False, bottom=False)

    ax.legend(loc='upper right', frameon=True, edgecolor='0.8', fontsize=10)
    ax.xaxis.set_major_locator(plt.MaxNLocator(nbins=8))

    plt.tight_layout()

    # 저장
    out_dir = f"{username}/mgvm/analyze/_out/memory-trace/memory-access-pattern/{benchmark}"
    os.makedirs(out_dir, exist_ok=True)
    out_file = f"{out_dir}/{benchmark}_access_pattern.png"
    
    plt.savefig(out_file, dpi=400, bbox_inches='tight', facecolor='white')
    plt.close()
    
    print(f"✅ Saved access pattern: {out_file}")

if __name__=="__main__":
    import argparse
    parser = argparse.ArgumentParser()
    parser.add_argument('--date', type=str, default='20260221-223953', help='')
    args = parser.parse_args()

    date = args.date
    expanded_dir = f"{memory_trace_dir(date)}/expanded"

    # 폴더 미리 생성
    os.makedirs(f"{username}/mgvm/analyze/_out/memory-trace/memory-access-pattern", exist_ok=True)

    for benchmark in benchmarks:
        trace_file = f"{expanded_dir}/{benchmark}_mesh_6x6_memory_trace.csv"
        
        if not os.path.exists(trace_file):
            print(f"Skip: No {trace_file}")
            continue
        
        print(f"Reading {benchmark}")
        
        df = pd.read_csv(trace_file, skipinitialspace=True)
        df = df.drop_duplicates()
        plot_memory_access_pattern(df, benchmark)
    
    print("✅ 모든 접근 패턴 그래프 생성 완료!")
