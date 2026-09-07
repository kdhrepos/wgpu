import pandas as pd
import os
from ast import literal_eval
from collections import Counter
import numpy as np
import matplotlib.pyplot as plt
import matplotlib.patches as mpatches
import seaborn as sns

from concurrent.futures import ProcessPoolExecutor, as_completed
from params import benchmarks

username = os.path.expanduser('~')


def process_benchmark(
    benchmark, 
    trace_dir, 
    result_dir, 
    is_rdma, 
    is_local,
    n_cycles=10000
):
    trace_file = f"{trace_dir}/{benchmark}_mesh_6x6_memory_trace.csv"

    if not os.path.exists(trace_file):
        return f"[Skip] {benchmark}: No trace file."

    print(f"[Start] Processing {benchmark}...")

    # 1. 데이터 로드 및 필터링
    df = pd.read_csv(trace_file, skipinitialspace=True)
    df = df.drop_duplicates()
    df = df[df['vector'] == True]

    ns = 1e-9
    df['cycle'] = df['time'] / ns
    df['cycle'] = df['cycle'].astype(int)

    if is_rdma:
        df = df[df['rdma'] == True]
    if is_local:
        df = df[df['rdma'] == False]

    if df.empty:
        return f"[Skip] {benchmark}: DataFrame is empty after filtering."

    # 2. Hop Count 벡터화 계산 (속도 최적화 핵심)
    w = 6
    s_x, s_y = df['chiplet'] % w, df['chiplet'] // w
    d_x, d_y = df['owner'] % w, df['owner'] // w
    df['hop_count'] = (d_x - s_x).abs() + (d_y - s_y).abs()

    first_appearance = df.groupby(['owner', 'pc'])['cycle'].min().reset_index()
    first_appearance.columns = ['owner', 'pc', 'first_cycle']

    df_with_start = df.merge(first_appearance, on=['owner', 'pc'], how='left')

    phase_df = df_with_start[
        (df_with_start['cycle'] >= df_with_start['first_cycle']) & 
        (df_with_start['cycle'] < (df_with_start['first_cycle'] + n_cycles))
    ].copy()

    shared_map = phase_df.groupby(['owner', 'addr'])['chiplet'].nunique().reset_index(name='shared_degree')
    phase_df = phase_df.merge(shared_map, on=['owner', 'addr'], how='left')

    pc_stats = phase_df.groupby(['owner', 'pc']).agg(
        access_count=('addr', 'count'),
        unique_addr_count=('addr', 'nunique'),
        shared_degree=('shared_degree', 'mean'),
        hop_count=('hop_count', 'sum'),
    ).rename({
        'access_count': 'access_count',
        'unique_addr_count': 'unique_addr_count',
        'shared_degree': 'shared_degree',
        'hop_count': 'hop_count',
    }).reset_index()

    pc_final_avg = pc_stats.groupby('pc').agg({
        'access_count': 'mean',
        'unique_addr_count': 'mean',
        'shared_degree': 'mean',
        'hop_count': 'mean',
    }).reset_index()

    pc_final_avg['avg_access_count'] = pc_final_avg['access_count'] / pc_final_avg['unique_addr_count']
    
    pc_final_avg.to_csv(f"{result_dir}/{benchmark}_pc_stats_per_home.csv", index=False)


if __name__ == "__main__":
    import argparse
    parser = argparse.ArgumentParser()
    parser.add_argument('--date', type=str, default='20260409-113720-mesh', help='')
    parser.add_argument('--rdma', type=bool, default=False, help='')
    parser.add_argument('--local', type=bool, default=False, help='')
    parser.add_argument('--n-cycles', type=int, default=0, help='Number of cycles to consider for each PC phase')
    args = parser.parse_args()

    date = args.date
    base_dir = f'{username}/mgvm/results/memory-traces'
    trace_dir = f"{base_dir}/{date}"

    # 디렉토리 설정 (한 번만 실행되도록 main으로 뺌)
    if args.rdma:
        result_dir = f'{username}/mgvm/analyze/_out/pc-stats/pc-stats-per-home-rdma'
        os.makedirs(result_dir, exist_ok=True)
    elif args.local:
        result_dir = f'{username}/mgvm/analyze/_out/pc-stats/pc-stats-per-home-local'
        os.makedirs(result_dir, exist_ok=True)
    else:
        result_dir = f'{username}/mgvm/analyze/_out/pc-stats/pc-stats-per-home'
        os.makedirs(result_dir, exist_ok=True)

    if args.n_cycles != 0:
        result_dir = f"{result_dir}/{args.n_cycles}"
        os.makedirs(result_dir, exist_ok=True)

    print(f"Starting multi-processing for {len(benchmarks)} benchmarks using {os.cpu_count()} cores...")

    # 병렬 처리 실행 (CPU 코어 개수만큼 워커 생성)
    with ProcessPoolExecutor(max_workers=os.cpu_count()) as executor:
        # executor.submit을 사용하면 완료된 작업부터 결과를 받아볼 수 있습니다.
        futures = {executor.submit(
            process_benchmark, 
            b, 
            trace_dir, 
            result_dir, 
            args.rdma, 
            args.local,
            args.n_cycles): b for b in benchmarks}
        
        for future in as_completed(futures):
            try:
                result = future.result()
                print(result)
            except Exception as e:
                benchmark_name = futures[future]
                print(f"[Error] Failed processing {benchmark_name}: {e}")