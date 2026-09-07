import pandas as pd
import re
import matplotlib.pyplot as plt
import matplotlib.ticker as mtick
import os
import numpy as np
import seaborn as sns

from params import benchmarks
from formatter import num_chiplets, ns, chip_cols
from _paths import memory_trace_dir

username = os.path.expanduser('~')


def plot_stride_ratio(local_res, rdma_res, benchmark, output_dir):
    # 1. 데이터 재구조화 (Pivot)
    local_res = local_res.copy()
    rdma_res = rdma_res.copy()
    local_res['Type'] = 'Local'
    rdma_res['Type'] = 'RDMA'
    
    combined = pd.concat([local_res, rdma_res], ignore_index=True)
    
    # x축: Type, 범례(Stack): Stride, 값: Ratio (%) 가 되도록 피벗
    # Stride를 문자열로 변환하여 처리
    combined['Stride'] = combined['Stride'].astype(str)
    pivot_df = combined.pivot(index='Type', columns='Stride', values='Ratio (%)').fillna(0)
    
    # 2. 'Others'가 있다면 가장 마지막에 오도록 컬럼 순서 조정
    cols = [c for c in pivot_df.columns if c != 'Others']
    if 'Others' in pivot_df.columns:
        cols.append('Others')
    pivot_df = pivot_df[cols]

    # 3. 그래프 그리기
    # 벤치마크별로 색상 팔레트 설정 (Stride 개수에 맞춰)
    ax = pivot_df.plot(kind='bar', stacked=True, figsize=(8, 10), colormap='tab20')

    # 4. 스타일링
    plt.title(f"Memory Access Stride Distribution: {benchmark}", fontsize=15, pad=20)
    plt.ylabel("Cumulative Ratio (%)", fontsize=12)
    plt.xlabel("Access Type", fontsize=12)
    plt.xticks(rotation=0) # Local, RDMA 글자 똑바로
    plt.ylim(0, 100)
    
    # 범례를 그래프 밖으로 이동 (Stride가 많을 수 있으므로)
    plt.legend(title="Stride", bbox_to_anchor=(1.05, 1), loc='upper left')

    # 막대 내부에 퍼센트 텍스트 추가 (비중이 큰 것 위주로)
    for p in ax.patches:
        width, height = p.get_width(), p.get_height()
        if height > 5: # 5% 이상인 경우만 텍스트 표시 (가독성)
            x, y = p.get_xy() 
            ax.text(x + width/2, 
                    y + height/2, 
                    f'{height:.1f}%', 
                    ha='center', va='center', fontsize=9, color='white', fontweight='bold')

    plt.tight_layout()
    
    # 5. 저장
    save_path = f"{output_dir}/{benchmark}_stride_ratio.png"
    plt.savefig(save_path, dpi=400, bbox_inches='tight')
    plt.close()
    print(f"   Stacked plot saved: {save_path}")


def summarize_strides(stride_series, threshold=1.0):
    """1% 이하의 비율을 가진 Stride들을 'Others'로 통합하는 함수"""
    if stride_series.empty:
        return pd.DataFrame(columns=['Stride', 'Ratio (%)'])
    
    # 비율 계산
    counts = stride_series.value_counts(normalize=True).mul(100).reset_index()
    counts.columns = ['Stride', 'Ratio (%)']
    
    # 1% 기준으로 분리
    major = counts[counts['Ratio (%)'] > threshold].copy()
    minor_ratio = counts[counts['Ratio (%)'] <= threshold]['Ratio (%)'].sum()
    
    if minor_ratio > 0:
        others_df = pd.DataFrame([{'Stride': 'Others', 'Ratio (%)': minor_ratio}])
        result = pd.concat([major, others_df], ignore_index=True)
    else:
        result = major
        
    return result


if __name__ == "__main__":
    date = "20260409-113720-mesh"
    trace_dir = memory_trace_dir(date)
    print(f"{trace_dir}")

    for benchmark in benchmarks:
        print(f">>> Processing Benchmark: {benchmark}")

        trace_file = f"{trace_dir}/{benchmark}_mesh_6x6_memory_trace.csv"
        if not os.path.exists(trace_file):
            print(f"   Skip: No file {trace_file}")
            continue

        result_dir = f"{username}/mgvm/analyze/_out/data-structure/local-remote-strides"
        os.makedirs(result_dir, exist_ok=True)

        df = pd.read_csv(trace_file, skipinitialspace=True)
        df = df.drop_duplicates()

        local_df = df[df['rdma'] == False]
        local_df = local_df.sort_values(by='time')
        # chiplet별로 그룹화하여 stride 계산 (shift 연산 방지)
        local_strides = local_df.groupby(['chiplet'])['addr'].diff().dropna()
        local_results = summarize_strides(local_strides)

        # 2. RDMA (Remote) Stride 분석
        rdma_df = df[df['rdma'] == True]
        rdma_df = rdma_df.sort_values(by='time')
        rdma_strides = rdma_df.groupby(['chiplet'])['addr'].diff().dropna()
        rdma_results = summarize_strides(rdma_strides)

        local_results.to_csv(f"{result_dir}/{benchmark}_local_strides.csv", index=False)
        rdma_results.to_csv(f"{result_dir}/{benchmark}_rdma_strides.csv", index=False)

        plot_stride_ratio(local_results, rdma_results, benchmark, result_dir)