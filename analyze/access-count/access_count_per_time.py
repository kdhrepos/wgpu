import pandas as pd
import re
import matplotlib.pyplot as plt
import os
from ast import literal_eval
from collections import Counter
import math

from _paths import memory_trace_dir, run_root

username = os.path.expanduser('~')

benchmarks = [
    # High-sharing Apps
    'gups',    
    'syr2k',
    'spmv',
    'mis', # Deadlock
    'stencil2d',
    'gemm',
    'lu',
    'matrixmultiplication',
    'mm2',
    'mm3',
    'gaussian',
    'matrixtranspose', # Error

    # Low-sharing Apps
    'fdtd2d',
    'fastwalshtransform',
    'jacobi1d',
    'relu',
    'pagerank',
    'bfs',
    'color',
    'convolution2d',
    'sssp',
    'fft',
    'fir',
    'bitonicsort',
]

num_chiplets = 36
ns = 0.000000001

import argparse
parser = argparse.ArgumentParser()
parser.add_argument('--date', type=str, default=1, help='')
parser.add_argument('--term-cycle', type=int, default=10000, help='')
args = parser.parse_args()

date = args.date
term_cycle = args.term_cycle

def parse_access_str(access_str):
    if pd.isna(access_str):
        return {}
    
    result = {}
    for item in access_str.split('/'):
        try:
            chip_id, count = item.strip().split(':')
            result[int(chip_id)] = int(count)
        except ValueError:
            continue
    return result


for benchmark in benchmarks:
    trace_file = f"{memory_trace_dir(date)}/{benchmark}_mesh_6x6_memory_trace.csv"

    print(f"{benchmark} at {trace_file}")

    if not os.path.exists(trace_file):
        print(f"There is no {trace_file}")
        continue

    df = pd.read_csv(trace_file)
    df = df.drop_duplicates()

    access_dicts = df['access'].apply(parse_access_str)
    chips_df = pd.DataFrame(access_dicts.tolist())
    chips_df = chips_df.reindex(columns=range(36)).fillna(0).astype(int)

    df = pd.concat([df[['time', 'addr', 'pc']], chips_df], axis=1)

    chip_cols = list(range(36))

    t_min = df['time'].min()
    df['time'] = (df['time'] - t_min) / ns
    max_time = df['time'].max()

    bins = np.arange(0, max_time + term_cycle, term_cycle)
    df['period_bin'] = pd.cut(df['time'], bins=bins, labels=bins[:-1], include_lowest=True)

    agg_rules = {col: 'sum' for col in chip_cols}

    grouped = df.groupby(['period_bin', 'addr'], observed=True).agg(agg_rules)
    grouped['chiplets'] = (grouped[chip_cols] != 0).sum(axis=1)
    grouped['count'] = grouped[chip_cols].sum(axis=1)
    grouped = grouped.reset_index()

    shared = grouped[grouped['chiplets'] > 1]

    # 구간별 통계 산출
    total_metrics = grouped.groupby('period_bin', observed=True).agg(
        total_addr=('addr', 'nunique'), # 구간 내 주소 개수
        total_count=('count', 'sum')   # 구간 내 전체 count 합
    )

    shared_metrics = shared.groupby('period_bin', observed=True).agg(
        shared_addr=('addr', 'nunique'),
        shared_count=('count', 'sum'),
    )

    # 결과 병합
    final_res = total_metrics.join(shared_metrics).fillna(0)
    final_res['shared_addr_ratio'] = final_res['shared_addr'] / final_res['total_addr']
    final_res['shared_count_ratio'] = final_res['shared_count'] / final_res['total_count']

    results_df = final_res.reset_index().rename(columns={'period_bin': 'start'})
    results_df['start'] = results_df['start'].astype(float)
    results_df['end'] = results_df['start'] + term_cycle
    results_df = results_df[['start', 'end', 'shared_addr_ratio', 'shared_count_ratio']]

    result_dir=f'{run_root(date)}/data'
    if not os.path.isdir(f'{result_dir}'):
        os.makedirs(f'{result_dir}', exist_ok=True, mode=0o777)

    print(f"Save results to {result_dir}/{benchmark}_mesh_6x6_access_count_per_time.csv")
    
    results_df.to_csv(f"{result_dir}/{benchmark}_mesh_6x6_access_count_per_time.csv", index=False)