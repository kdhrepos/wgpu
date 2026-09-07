import os
import sys
import pandas as pd
import numpy as np
import numpy as np
import matplotlib.pyplot as plt
import matplotlib.patches as mpatches
import seaborn as sns

parent_dir = os.path.abspath(os.path.join(os.path.dirname(__file__), "../../"))

# 검색 경로에 추가
if parent_dir not in sys.path:
    sys.path.append(parent_dir)

from ast import literal_eval
from collections import Counter
from concurrent.futures import ProcessPoolExecutor, as_completed
from params import benchmarks

username = os.path.expanduser('~')
# run_id 결정: argv 첫 positional, 없으면 'latest'
_run_id = next((a for a in sys.argv[1:] if not a.startswith('-')), 'latest')
from _paths import run_root as _run_root, memory_trace_dir as _mtd, run_tag as _run_tag
date = _run_tag(_run_id)
trace_dir = str(_mtd(_run_id))

xDim=4
yDim=4

def process_benchmark(
    benchmark, 
    trace_dir, 
    result_dir, 
    is_rdma, 
    is_local,
):
    trace_file = f"{trace_dir}/{benchmark}_mesh_{xDim}x{yDim}_memory_trace.csv"

    if not os.path.exists(trace_file):
        return f"[Skip] {benchmark}: No trace file."

    print(f"[Start] Processing {benchmark}...")

    # 1. 데이터 로드 및 필터링
    df = pd.read_csv(trace_file, skipinitialspace=True)
    df = df.drop_duplicates()
    df = df[df['vector'] == True]

    if is_rdma:
        df = df[df['rdma'] == True]
    if is_local:
        df = df[df['rdma'] == False]

    if df.empty:
        return f"[Skip] {benchmark}: DataFrame is empty after filtering."

    # 2. Hop Count 벡터화 계산 (속도 최적화 핵심)
    w = xDim
    s_x, s_y = df['chiplet'] % w, df['chiplet'] // w
    d_x, d_y = df['owner'] % w, df['owner'] // w
    df['hop_count'] = (d_x - s_x).abs() + (d_y - s_y).abs()

    # 3. Saturation DataFrame 계산
    def calculate_saturation_df(temp_df, target_degree=18):
        first_reqs = temp_df.groupby(['owner', 'pc', 'chiplet'])['time'].min().reset_index()
        first_reqs = first_reqs.sort_values(['owner', 'pc', 'time'])
        first_reqs['cum_src_count'] = first_reqs.groupby(['owner', 'pc']).cumcount() + 1
        saturations = first_reqs[first_reqs['cum_src_count'] == target_degree].copy()
        return saturations.rename(columns={'time': 'saturation_time'})

    saturation_df = calculate_saturation_df(df, target_degree=18)
    pc_saturation_avg = saturation_df.groupby('pc')['saturation_time'].mean().reset_index()

    # 4. PC별 통계 계산
    pc_hop_avg = df.groupby('pc')['hop_count'].mean().reset_index(name='hop_count_mean')
    addr_counts = df.groupby(['pc', 'addr']).size().reset_index(name='access_count')
    shared_map = df.groupby('addr')['chiplet'].nunique().reset_index(name='shared_degree')

    stats_df = pd.merge(addr_counts, shared_map, on='addr', how='left')

    pc_stats_df = stats_df.groupby('pc').agg({
        'addr': 'nunique',
        'access_count': ['sum', 'mean', 'std', 'median', 'max', 'min'],
        'shared_degree': ['mean', 'std', 'median', 'max', 'min']
    }).reset_index()

    pc_stats_df.columns = ['_'.join(col).strip('_') for col in pc_stats_df.columns.values]

    # 5. 최종 병합 및 저장
    final_pc_report = pd.merge(pc_stats_df, pc_saturation_avg, on='pc', how='left')
    final_pc_report = pd.merge(final_pc_report, pc_hop_avg, on='pc', how='left')

    output_path = f"{result_dir}/{benchmark}_pc_stats.csv"
    final_pc_report.to_csv(output_path, index=False)
    
    return f"[Done] {benchmark} saved to {output_path}"


if __name__ == "__main__":
    import argparse
    parser = argparse.ArgumentParser()
    parser.add_argument('run_id', nargs='?', default='latest',
                        help="run-id (latest, <config>, <config>/<timestamp>, ...)")
    parser.add_argument('--rdma', type=bool, default=False, help='')
    parser.add_argument('--local', type=bool, default=False, help='')
    parser.add_argument('--read-only', type=bool, default=False, help='')
    args = parser.parse_args()


    # 디렉토리 설정 (한 번만 실행되도록 main으로 뺌)
    if args.rdma:
        result_dir = f'{username}/mgvm/analyze/_out/pc-stats/pc-stats-rdma'
    elif args.local:
        result_dir = f'{username}/mgvm/analyze/_out/pc-stats/pc-stats-local'
    else:
        result_dir = f'{username}/mgvm/analyze/_out/pc-stats/pc-stats'
        
    os.makedirs(result_dir, exist_ok=True)

    print(f"Starting multi-processing for {len(benchmarks)} benchmarks using {os.cpu_count()} cores...")

    # 병렬 처리 실행 (CPU 코어 개수만큼 워커 생성)
    with ProcessPoolExecutor(max_workers=os.cpu_count()) as executor:
        # executor.submit을 사용하면 완료된 작업부터 결과를 받아볼 수 있습니다.
        futures = {executor.submit(process_benchmark, b, trace_dir, result_dir, args.rdma, args.local): b for b in benchmarks}
        
        for future in as_completed(futures):
            try:
                result = future.result()
                print(result)
            except Exception as e:
                benchmark_name = futures[future]
                print(f"[Error] Failed processing {benchmark_name}: {e}")