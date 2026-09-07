import pandas as pd
import re
import matplotlib.pyplot as plt
import matplotlib.ticker as mtick
import os
import numpy as np
import seaborn as sns

from params import benchmarks

username = os.path.expanduser('~')


def calculate_owner_pc_saturation(df):
    # 1. (Owner, PC, 요청자) 조합별로 가장 빠른 시간 추출
    # src_chiplet: 요청을 보낸 주체 / owner: 요청을 받은 Home Node
    first_requests = df.groupby(['owner', 'pc', 'chiplet'])['time'].min().reset_index()
    
    # 2. Owner와 PC별로 시간순 정렬
    first_requests = first_requests.sort_values(by=['owner', 'pc', 'time'])
    
    # 3. 각 (Owner, PC) 그룹 내에서 유니크한 요청자(src_chiplet)의 누적 수 계산
    first_requests['cumulative_src_count'] = first_requests.groupby(['owner', 'pc']).cumcount() + 1
    
    # 4. 36번째 유니크한 요청자가 도착한 시점(Saturation) 필터링
    # 만약 본인 칩렛의 요청도 트레이스에 포함된다면 36, 제외라면 35로 설정
    saturation_points = first_requests[first_requests['cumulative_src_count'] == 36].copy()
    
    # 5. 결과 정리
    saturation_points = saturation_points.rename(columns={'time': 'saturation_time'})
    return saturation_points[['owner', 'pc', 'saturation_time']]


if __name__ == "__main__":
    base_dir=f'{username}/mgvm/results/memory-traces'
    date = "20260409-113720-mesh"
    trace_dir = f"{base_dir}/{date}"
    print(f"{trace_dir}")

    for benchmark in benchmarks:
        # if benchmark != "bfs":
            # continue

        print(f">>> Processing Benchmark: {benchmark}")

        trace_file = f"{trace_dir}/{benchmark}_mesh_6x6_memory_trace.csv"

        if not os.path.exists(trace_file):
            print(f"   Skip: No file {trace_file}")
            continue

        df = pd.read_csv(trace_file, skipinitialspace=True)
        df = df.drop_duplicates()

        # 1. 결과 저장 디렉토리 분리
        result_dir = f"{username}/mgvm/analyze/_out/pc-stats/hot-pc-metrics/unique-cachelines"
        os.makedirs(result_dir, exist_ok=True)
        
        saturation_df = calculate_owner_pc_saturation(df)
        saturated_trace = pd.merge(
            df, 
            saturation_df[['owner', 'pc']], 
            on=['owner', 'pc'], 
            how='inner'
        )

        owner_unique_addr_counts = saturated_trace.groupby('owner')['addr'].nunique().reset_index()
        owner_unique_addr_counts.to_csv(f"{result_dir}/{benchmark}_owner_unique_cachelines.csv", index=False)

        print(f">>> Done: {benchmark}")