import pandas as pd
import os
from ast import literal_eval
from collections import Counter
import numpy as np
import matplotlib.pyplot as plt
import matplotlib.patches as mpatches
import seaborn as sns

from params import benchmarks

username = os.path.expanduser('~')


if __name__=="__main__":
    import argparse
    parser = argparse.ArgumentParser()

    base_dir = f'{username}/mgvm/results/memory-traces'

    result_dir = f'{username}/mgvm/analyze/_out/pc-stats/pc-stats'
    os.makedirs(result_dir, exist_ok=True)

    for benchmark in benchmarks:
        # if benchmark != "bfs":
            # continue

        rdma_stats_file = f""

        if not os.path.exists(trace_file):
            print(f"No {trace_file}, skip")
            continue
        else:
            print(f"Processing {benchmark}")

        df = pd.read_csv(trace_file, skipinitialspace=True)
        df = df.drop_duplicates()

        if args.rdma:
            print("Filtering RDMA accesses only")
            df = df[df['rdma'] == True]

            result_dir = f'{username}/mgvm/analyze/_out/pc-stats/pc-stats-rdma'
            os.makedirs(result_dir, exist_ok=True)

        def calculate_saturation_df(df, target_degree=18):
            # 각 (Owner, PC, 요청자)별 첫 방문 시간
            first_reqs = df.groupby(['owner', 'pc', 'chiplet'])['time'].min().reset_index()
            first_reqs = first_reqs.sort_values(['owner', 'pc', 'time'])
            
            # 누적 요청자 수 계산
            first_reqs['cum_src_count'] = first_reqs.groupby(['owner', 'pc']).cumcount() + 1
            
            # 목표 수치(예: 18)에 도달한 시점 필터링
            saturations = first_reqs[first_reqs['cum_src_count'] == target_degree].copy()
            return saturations.rename(columns={'time': 'saturation_time'})

        # 2. Saturation 데이터 추출 및 PC별 평균 계산
        saturation_df = calculate_saturation_df(df, target_degree=18)
        pc_saturation_avg = saturation_df.groupby('pc')['saturation_time'].mean().reset_index()

        addr_counts = df.groupby(['pc', 'addr']).size().reset_index(name='access_count')
        shared_map = df.groupby('addr')['chiplet'].nunique().reset_index(name='shared_degree')

        stats_df = pd.merge(addr_counts, shared_map, on='addr', how='left')

        pc_stats_df = stats_df.groupby('pc').agg({
            'addr': 'nunique',
            'access_count': ['sum', 'mean', 'std', 'median', 'max', 'min'],
            'shared_degree': ['mean', 'std', 'median', 'max', 'min']
        }).reset_index()

        pc_stats_df.columns = ['_'.join(col).strip('_') for col in pc_stats_df.columns.values]

        final_pc_report = pd.merge(pc_stats_df, pc_saturation_avg, on='pc', how='left')
        final_pc_report.to_csv(f"{result_dir}/{benchmark}_pc_stats.csv", index=False)