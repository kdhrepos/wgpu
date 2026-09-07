import pandas as pd
import os
from ast import literal_eval
from collections import Counter
import numpy as np
import matplotlib.pyplot as plt
import matplotlib.patches as mpatches
import matplotlib.ticker as ticker
from matplotlib.lines import Line2D
import seaborn as sns

from params import benchmarks
from _paths import memory_trace_dir, data_allocation_log_dir

username = os.path.expanduser('~')


def plot_stride_ratio(df, benchmark, result_dir, save_fig=True, top_n=10):
    # 1. DataID가 -1인 데이터 제외 및 비율 계산
    df_filtered = df[df['DataID'] != -1].copy()
    stride_ratios = df_filtered.groupby('DataID')['stride'].value_counts(normalize=True).mul(100).reset_index(name='Ratio (%)')
    
    # 2. 각 DataID별로 상위 N개와 Others 처리
    unique_ids = sorted(stride_ratios['DataID'].unique())
    processed_data = []

    for d_id in unique_ids:
        data_subset = stride_ratios[stride_ratios['DataID'] == d_id].sort_values(by='Ratio (%)', ascending=False)
        top_data = data_subset.head(top_n).copy()
        others_val = data_subset.iloc[top_n:]['Ratio (%)'].sum()
        
        if others_val > 0:
            others_row = pd.DataFrame({'DataID': [d_id], 'stride': ['Others'], 'Ratio (%)': [others_val]})
            top_data = pd.concat([top_data, others_row], ignore_index=True)
        
        processed_data.append(top_data)
    
    full_df = pd.concat(processed_data)

    # 3. 누적 막대 그래프를 위한 피벗 테이블 생성
    # 행: DataID, 열: stride, 값: Ratio (%)
    pivot_df = full_df.pivot(index='DataID', columns='stride', values='Ratio (%)').fillna(0)
    
    # stride 라벨 정렬 (Others를 맨 위로 보내기 위해)
    cols = [c for c in pivot_df.columns if c != 'Others']
    if 'Others' in pivot_df.columns:
        cols = cols + ['Others']
    pivot_df = pivot_df[cols]

    # 4. 시각화
    plt.figure(figsize=(12, 8))
    # 'stride' 값에 따라 색상을 입혀 누적 그래프 생성
    pivot_df.plot(kind='bar', stacked=True, ax=plt.gca(), colormap='tab20', edgecolor='white', width=0.7)

    # 그래프 포맷팅
    plt.title("Memory Access Stride Composition by DataID (Excluding ID -1)", fontsize=15, fontweight='bold')
    plt.xlabel("Data Structure ID", fontsize=12)
    plt.ylabel("Ratio (%)", fontsize=12)
    plt.legend(title="Stride Value", bbox_to_anchor=(1.05, 1), loc='upper left', fontsize=9)
    plt.xticks(rotation=0)
    plt.grid(axis='y', linestyle='--', alpha=0.5)
    
    # 막대 위에 합계가 100%인지 확인용 (선택 사항)
    plt.ylim(0, 110) 
    
    plt.tight_layout()

    if save_fig:
        plt.savefig(f"{result_dir}/{benchmark}_stride_ratio.png", dpi=400, bbox_inches='tight')

    plt.close()


def df_with_data_id(df, df_alloc):
    df = df.sort_values('addr')
    df_alloc = df_alloc.sort_values('StartAddr')

    df_trace_mapped = pd.merge_asof(
        df, 
        df_alloc[['StartAddr', 'EndAddr', 'DataID']], 
        left_on='addr', 
        right_on='StartAddr', 
        direction='backward'
    )

    mask = (df_trace_mapped['addr'] <= df_trace_mapped['EndAddr'])
    df_trace_mapped.loc[~mask, 'DataID'] = -1

    df = df_trace_mapped.drop(columns=['StartAddr', 'EndAddr'])
    df = df.sort_values(by='time')
    
    return df

if __name__=="__main__":
    import argparse
    parser = argparse.ArgumentParser()
    parser.add_argument('--date', type=str, default='20260409-113720-mesh', help='')
    args = parser.parse_args()

    date = args.date

    trace_dir = memory_trace_dir(date)

    sub_dir = "20260415-153843-mesh"
    data_log_dir = data_allocation_log_dir(sub_dir)

    for benchmark in benchmarks:
        # if benchmark != "bfs":
            # continue

        trace_file = f"{trace_dir}/{benchmark}_mesh_6x6_memory_trace.csv"
        data_alloc_file = f"{data_log_dir}/{benchmark}_mesh_6x6_data_allocation_log.csv"

        if not os.path.exists(trace_file) or not os.path.exists(data_alloc_file):
            print(f"No {trace_file} or {data_alloc_file}, skip")
            continue
        else:
            print(f"Processing {benchmark}")

        df = pd.read_csv(trace_file, skipinitialspace=True)
        df = df.drop_duplicates()
        
        df_alloc = pd.read_csv(data_alloc_file, skipinitialspace=True)

        df = df_with_data_id(df, df_alloc)
        df['stride'] = df.groupby(['DataID', 'chiplet'])['addr'].diff()
        
        result_dir = f"{username}/mgvm/analyze/_out/data-structure/data-structures-strides"
        os.makedirs(result_dir, exist_ok=True)

        plot_stride_ratio(df, benchmark=benchmark, result_dir=result_dir, top_n=10)