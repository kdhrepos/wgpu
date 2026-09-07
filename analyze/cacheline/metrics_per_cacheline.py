import pandas as pd
import re
import matplotlib.pyplot as plt
import os
from ast import literal_eval
from collections import Counter
import math
import numpy as np

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

chip_cols = list(range(36))
agg_rules = {col: 'sum' for col in chip_cols}


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


def shared_data_across_time(df):
    chip_cols = list(range(36))
    agg_rules = {col: 'sum' for col in chip_cols}
    
    grouped = df.groupby(['period_bin', 'addr'], observed=True).agg(agg_rules)
    grouped['chiplets'] = (grouped[chip_cols] != 0).sum(axis=1)
    grouped['count'] = grouped[chip_cols].sum(axis=1)
    grouped = grouped.reset_index()

    shared = grouped[grouped['chiplets'] > 1]

    total_metrics = grouped.groupby('period_bin', observed=True).agg(
        total_addr=('addr', 'nunique'), 
        total_count=('count', 'sum')    
    )

    shared_metrics = shared.groupby('period_bin', observed=True).agg(
        shared_addr=('addr', 'nunique'),
        shared_count=('count', 'sum'),
    )

    final_res = total_metrics.join(shared_metrics).fillna(0)
    final_res['shared_addr_ratio'] = final_res['shared_addr'] / final_res['total_addr']
    final_res['shared_count_ratio'] = final_res['shared_count'] / final_res['total_count']

    results_df = final_res.reset_index().rename(columns={'period_bin': 'start'})
    results_df['start'] = results_df['start'].astype(float)
    results_df['end'] = results_df['start'] + term_cycle
    results_df = results_df[['start', 'end', 'shared_addr_ratio', 'shared_count_ratio']]

    result_dir=f'{run_root(date)}/shared-across-time'
    if not os.path.isdir(f'{result_dir}'):
        os.makedirs(f'{result_dir}', exist_ok=True, mode=0o777)

    print(f"Save results to {result_dir}/{benchmark}_mesh_6x6_access_count_per_time.csv")

    results_df.to_csv(f"{result_dir}/{benchmark}_mesh_6x6_access_count_per_time.csv", index=False)


def total_shared_data(df):
    chip_cols = list(range(36))
    agg_rules = {col: 'sum' for col in chip_cols}

    grouped = df.groupby(['addr'], observed=True).agg(agg_rules)
    grouped['chiplets'] = (grouped[chip_cols] != 0).sum(axis=1)
    grouped['count'] = grouped[chip_cols].sum(axis=1)
    grouped = grouped.reset_index()

    shared = grouped[grouped['chiplets'] > 1]

    total_addrs= len(grouped)
    shared_addrs = len(shared)

    total_counts = grouped['count'].sum()
    shared_counts = shared['count'].sum()

    result_dir = f'{run_root(date)}/total-shared-data'
    if not os.path.isdir(f'{result_dir}'):
        os.makedirs(f'{result_dir}', exist_ok=True, mode=0o777)

    result_filename = f'{result_dir}/{benchmark}_mesh_6x6_total_shared_data.txt'
    print(f"Save results to {result_filename}")

    with open(result_filename, 'w', encoding='utf-8') as f:
        f.write(f"total addresses: {total_addrs}\n")
        f.write(f"shared addresses: {shared_addrs}\n")

        f.write(f"total counts: {total_counts}\n")
        f.write(f"shared counts: {shared_counts}\n")

        f.write(f"shared address ratio: {shared_addrs/total_addrs}\n")
        f.write(f"shared count ratio: {shared_counts/total_counts}\n")
        

def shared_data_across_time_with_num_chiplets(df):
    chip_cols = list(range(36))
    agg_rules = {col: 'sum' for col in chip_cols}
    
    grouped = df.groupby(['period_bin', 'addr'], observed=True).agg(agg_rules)
    grouped['chiplets'] = (grouped[chip_cols] != 0).sum(axis=1)
    grouped['count'] = grouped[chip_cols].sum(axis=1)
    grouped = grouped.reset_index()

    # 공유 주소(count > 1) 필터링
    shared1 = grouped[(grouped['chiplets'] > 1) & (grouped['chiplets'] <= 9)]
    shared2 = grouped[(grouped['chiplets'] > 9) & (grouped['chiplets'] <= 18)]
    shared3 = grouped[(grouped['chiplets'] > 18) & (grouped['chiplets'] <= 27)]
    shared4 = grouped[(grouped['chiplets'] > 27) & (grouped['chiplets'] <= 36)]

    # 구간별 통계 산출
    total_metrics = grouped.groupby('period_bin', observed=True).agg(
        total_addr=('addr', 'nunique'), # 구간 내 주소 개수
        total_count=('count', 'sum')   # 구간 내 전체 count 합
    )

    shared_metrics1 = shared1.groupby('period_bin', observed=True).agg(
        shared_addr=('addr', 'nunique'),
        shared_count=('count', 'sum'),
    )
    shared_metrics2 = shared2.groupby('period_bin', observed=True).agg(
        shared_addr=('addr', 'nunique'),
        shared_count=('count', 'sum'),
    )
    shared_metrics3 = shared3.groupby('period_bin', observed=True).agg(
        shared_addr=('addr', 'nunique'),
        shared_count=('count', 'sum'),
    )
    shared_metrics4 = shared4.groupby('period_bin', observed=True).agg(
        shared_addr=('addr', 'nunique'),
        shared_count=('count', 'sum'),
    )

    # 결과 병합
    final_res = total_metrics \
        .join(shared_metrics1, rsuffix='_1') \
        .join(shared_metrics2, rsuffix='_2') \
        .join(shared_metrics3, rsuffix='_3') \
        .join(shared_metrics4, rsuffix='_4') \
        .fillna(0)

    final_res['shared_addr_ratio'] = final_res['shared_addr'] / final_res['total_addr']
    final_res['shared_count_ratio'] = final_res['shared_count'] / final_res['total_count']

    # 각 범위별 비율 계산
    final_res['shared_addr_ratio_1'] = final_res['shared_addr'] / final_res['total_addr']
    final_res['shared_count_ratio_1'] = final_res['shared_count'] / final_res['total_count']

    final_res['shared_addr_ratio_2'] = final_res['shared_addr_2'] / final_res['total_addr']
    final_res['shared_count_ratio_2'] = final_res['shared_count_2'] / final_res['total_count']

    final_res['shared_addr_ratio_3'] = final_res['shared_addr_3'] / final_res['total_addr']
    final_res['shared_count_ratio_3'] = final_res['shared_count_3'] / final_res['total_count']

    final_res['shared_addr_ratio_4'] = final_res['shared_addr_4'] / final_res['total_addr']
    final_res['shared_count_ratio_4'] = final_res['shared_count_4'] / final_res['total_count']

    # 결과 정리
    results_df = final_res.reset_index().rename(columns={'period_bin': 'start'})
    results_df['start'] = results_df['start'].astype(float)
    results_df['end'] = results_df['start'] + term_cycle

    # 필요한 컬럼만 선택 (4개 범위 모두 포함)
    results_df = results_df[['start', 'end', 
                            'shared_addr_ratio_1', 'shared_addr_ratio_2', 
                            'shared_addr_ratio_3', 'shared_addr_ratio_4', 
                            'shared_count_ratio_1', 'shared_count_ratio_2',
                            'shared_count_ratio_3', 'shared_count_ratio_4']]
    
    result_dir=f'{run_root(date)}/shared-across-time-with-num-chiplets'
    if not os.path.isdir(f'{result_dir}'):
        os.makedirs(f'{result_dir}', exist_ok=True, mode=0o777)

    print(f"Save results to {result_dir}/{benchmark}_mesh_6x6_access_count_per_time_chiplets.csv")

    results_df.to_csv(f"{result_dir}/{benchmark}_mesh_6x6_access_count_per_time_chiplets.csv", index=False)


def metrics_per_pc(df):
    chip_cols = list(range(36))
    agg_rules = {col: 'sum' for col in chip_cols}

    grouped = df.groupby(['pc', 'addr'], observed=True).agg(agg_rules)
    grouped['chiplets'] = (grouped[chip_cols] != 0).sum(axis=1)
    grouped['count'] = grouped[chip_cols].sum(axis=1)
    grouped = grouped.reset_index()

    a = grouped.groupby('pc')['chiplets'].mean().reset_index()
    b = grouped.groupby('pc')['addr'].nunique().reset_index()
    c = grouped.groupby('pc')['count'].sum().reset_index()
    c['count'] /= grouped['count'].sum()
    m = pd.merge(a, b, on='pc')
    m = pd.merge(m, c, on='pc')

    result_dir=f'{run_root(date)}/metrics-per-pc'
    if not os.path.isdir(f'{result_dir}'):
        os.makedirs(f'{result_dir}', exist_ok=True, mode=0o777)

    print(f"Save results to {result_dir}/{benchmark}_mesh_6x6_metrics_per_pc.csv")

    m.to_csv(f"{result_dir}/{benchmark}_mesh_6x6_metrics_per_pc.csv", index=False)


if __name__=="__main__":
    num_chiplets = 36
    ns = 0.000000001

    import argparse
    parser = argparse.ArgumentParser()
    parser.add_argument('--date', type=str, default=1, help='')
    parser.add_argument('--term-cycle', type=int, default=10000, help='')
    args = parser.parse_args()

    date = args.date
    term_cycle = args.term_cycle

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

        t_min = df['time'].min()
        df['time'] = (df['time'] - t_min) / ns
        max_time = df['time'].max()

        bins = np.arange(0, max_time + term_cycle, term_cycle)
        df['period_bin'] = pd.cut(df['time'], bins=bins, labels=bins[:-1], include_lowest=True)

        # shared_data_across_time(df)
        # total_shared_data(df)
        # shared_data_across_time_with_num_chiplets(df)
        metrics_per_pc(df)