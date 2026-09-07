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

username = os.path.expanduser('~')

from params import benchmarks
from addr_metrics_plotter \
    import plot_memory_access_pattern, \
    plot_address_metrics, \
    get_pc_mapping


if __name__=="__main__":
    time_intervals = [1000, 10000, 20000, 100000]
    shared_types = ['ro_shared', 'rw_shared', 'shared']

    base_dir=f'{username}/mgvm/results/memory-traces'
    date = "20260409-113720-mesh"
    trace_dir = f"{base_dir}/{date}"

    result_dir = f"{username}/mgvm/analyze/_out/pc-stats/pc-rdma-metrics-fig-chiplet"
    os.makedirs(f"{result_dir}", exist_ok=True)

    # 2. 메인 루프
    for benchmark in benchmarks:
        # if benchmark != "bfs":
            # continue

        print(f"\n>>> Processing Benchmark: {benchmark}")
        trace_file = f"{trace_dir}/{benchmark}_mesh_6x6_memory_trace.csv"
        
        if not os.path.exists(trace_file):
            print(f"No trace of {benchmark}")
            continue

        # 데이터 로드
        df = pd.read_csv(trace_file, skipinitialspace=True)
        df = df.drop_duplicates()

        df = df[df['chiplet'] == 0]  # chiplet가 0인 행만 필터링

        # ✅ [중요] 함수 호출 전에 필요한 메트릭들을 미리 계산하여 df에 병합
        # 1. PC별/주소별 접근 횟수 (access_count) 계산
        # 'pc'별로 각 'addr'이 몇 번 나타나는지 평균을 내어 pc_stats 생성
        addr_counts = df.groupby(['pc', 'addr']).size().reset_index(name='access_count')
        pc_stats = addr_counts.groupby('pc')['access_count'].mean().to_frame()

        shared_map = df.groupby('addr')['chiplet'].nunique().reset_index(name='shared_degree')
        
        # 3. 원본 df에 메트릭 병합 (시각화 함수에서 사용하기 위함)
        stats_df = pd.merge(addr_counts, shared_map, on='addr', how='left')

        # ✅ 매핑 생성
        # 시각화할 PC 개수 설정 (전체를 다 하려면 nunique 사용)
        num_to_show = min(10, df['pc'].nunique()) 
        pc_color_map, pc_marker_map, mapped_num = get_pc_mapping(pc_stats, top_n=num_to_show)
        
        print(f"✅ Generated {mapped_num} PCs mapping for {benchmark}")

        # ✅ 시각화 함수 호출
        plot_memory_access_pattern(
            df=df,
            benchmark=benchmark, 
            result_dir=result_dir, 
            color_map=pc_color_map, 
            marker_map=pc_marker_map
        )
        
        # value="access_count" 컬럼이 이제 df에 존재하므로 정상 작동함
        plot_address_metrics(
            df=stats_df, 
            benchmark=benchmark, 
            result_dir=result_dir, 
            color_map=pc_color_map, 
            marker_map=pc_marker_map,
            value="access_count"
        )
        
        # value="shared_degree" 컬럼이 이제 df에 존재함
        plot_address_metrics(
            df=stats_df, 
            benchmark=benchmark, 
            result_dir=result_dir, 
            color_map=pc_color_map, 
            marker_map=pc_marker_map,
            value="shared_degree"
        )