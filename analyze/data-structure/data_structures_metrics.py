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


def plot_data_structures(df, benchmark, save_fig=True):
    if df['StartAddr'].dtype == 'object':
        df['Start_num'] = df['StartAddr'].apply(lambda x: int(x, 16))
    else:
        df['Start_num'] = df['StartAddr']

    plt.figure(figsize=(15, 5))
    
    # 1. 데이터 그리기 (투박하게 한 방에)
    scatter = plt.scatter(df['Start_num'], df['DataID'], 
                          c=df['DataID'], cmap='tab10', 
                          s=150, marker='|', alpha=0.6)

    # 2. 범례 박스 크게 만들기 (핵심 포인트)
    unique_ids = sorted(df['DataID'].unique())
    cmap = plt.cm.get_cmap('tab10')
    
    # 범례용 가짜 핸들 생성 (마커 크기를 15로 크게 설정)
    legend_elements = [
        Line2D([0], [0], marker='s', color='w', label=f'DataID {d_id}',
               markerfacecolor=cmap(i / (len(unique_ids)-1 if len(unique_ids) > 1 else 1)), 
               markersize=15) # 여기서 박스 크기 조절
        for i, d_id in enumerate(unique_ids)
    ]

    # 3. 포맷팅
    ax = plt.gca()
    ax.xaxis.set_major_formatter(ticker.FuncFormatter(lambda x, pos: f'0x{int(x):X}'))
    
    plt.xlabel('Physical Address (Hex)')
    plt.ylabel('Data ID')
    plt.title('Memory Map')
    plt.grid(True, axis='x', ls='--', alpha=0.3)
    
    # 범례 출력
    plt.legend(handles=legend_elements, title="Data ID", 
               loc='upper left', bbox_to_anchor=(1, 1), fontsize=12)

    plt.tight_layout()

    if save_fig:
        out_dir = f"{username}/mgvm/analyze/_out/data-structure/data-structure-metrics/figures"
        plt.savefig(f"{out_dir}/{benchmark}_data_structure_mapping.png", bbox_inches='tight')


def metrics_per_data_id(addr_counts, df_alloc):
    # 2. 정렬 (필수)
    df_alloc = df_alloc.sort_values('StartAddr').reset_index(drop=True)
    addr_counts = addr_counts.sort_values('addr').reset_index(drop=True)

    # 3. merge_asof 실행
    # addr_counts에 이미 DataID가 있으므로 결과는 DataID_y를 봐야 함
    matched = pd.merge_asof(
        addr_counts, 
        df_alloc[['StartAddr', 'EndAddr', 'DataID']], 
        left_on='addr', 
        right_on='StartAddr', 
        direction='backward'
    )

    # 4. 유효 범위 필터링 및 컬럼 정리
    # DataID_y가 실제 우리가 찾던 할당 ID입니다.
    if 'DataID_y' in matched.columns:
        # 매칭된 ID가 있고, 주소가 EndAddr 이내인 경우만 필터링
        final = matched[
            matched['DataID_y'].notna() & 
            (matched['addr'] <= matched['EndAddr'])
        ].copy()
        
        # 헷갈리지 않게 이름을 다시 DataID로 변경
        final = final.rename(columns={'DataID_y': 'DataID'})
    else:
        # 혹시 이름이 안 바뀌었을 경우를 대비
        final = matched[matched['DataID'].notna() & (matched['addr'] <= matched['EndAddr'])].copy()

    # 5. 최종 합계 및 캐시 라인(고유 addr) 개수 계산
    # access_count의 합계(sum)와 addr의 개수(count)를 동시에 구합니다.
    target_col = 'count' if 'count' in final.columns else 'access_count'

    result = final.groupby('DataID').agg({
        target_col: ['sum', 'count', 'mean', 'std']
    }).reset_index()

    # 컬럼명 정리 (보기 편하게 변경)
    result.columns = ['DataID', 'Total_Access_Count', 'Cache_Line_Count', 'Avg_Access_per_Line', 'Std_Access_per_Line']
    result['Std_Access_per_Line'] = result['Std_Access_per_Line'].fillna(0)

    return result


if __name__=="__main__":
    import argparse
    parser = argparse.ArgumentParser()
    parser.add_argument('--date', type=str, default='20260409-113720-mesh', help='')
    parser.add_argument('--rdma-only', type=bool, help='RDMA 접근만 분석할지 여부')
    args = parser.parse_args()

    date = args.date
    rdma_only = args.rdma_only

    trace_dir = memory_trace_dir(date)

    sub_dir = "20260501-143923-mesh"
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

        if rdma_only:
            df = df[df['rdma'] == True]
        
        df_alloc = pd.read_csv(data_alloc_file, skipinitialspace=True)

        addr_counts = df.groupby(['pc', 'addr']).size().reset_index(name='access_count')
        shared_map = df.groupby('addr')['chiplet'].nunique().reset_index(name='shared_degree')

        result = metrics_per_data_id(addr_counts, df_alloc)

        result_dir = f"{username}/mgvm/analyze/_out/data-structure/data-structure-metrics/normal"
        os.makedirs(result_dir, exist_ok=True)

        if rdma_only:
            result_dir = f"{username}/mgvm/analyze/_out/data-structure/data-structure-metrics/rdma"
            os.makedirs(result_dir, exist_ok=True)

        result.to_csv(f"{result_dir}/{benchmark}_data_structure_metrics.csv", index=False)

        # result_fig_dir = f"{result_dir}/figures"
        # os.makedirs(result_fig_dir, exist_ok=True)
        # plot_data_structures(df_alloc, benchmark, save_fig=True)