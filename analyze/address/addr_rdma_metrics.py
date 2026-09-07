import pandas as pd
import re
import os
from ast import literal_eval
from collections import Counter
import math
import numpy as np
import matplotlib.pyplot as plt
import matplotlib.patches as mpatches
import seaborn as sns

from _paths import memory_trace_dir

username = os.path.expanduser('~')

num_chiplets = 36
ns = 0.000000001
chip_cols = [str(i) for i in range(36)]

benchmarks = [
    # Low-sharing Apps
    "fdtd2d", "jacobi1d", "fastwalshtransform", "relu", "fft", "fir", 
    "sssp", "convolution2d", "color", "bitonicsort", "pagerank", "simpleconvolution",
    # High-sharing Apps
    'gups', 'syr2k', 'spmv', 'mis', 'stencil2d', 'gemm', 'lu', 
    'matrixmultiplication', 'mm2', 'mm3', 'gaussian', 'matrixtranspose', 
    'im2col', 'vgg16', 'bfs',
]

clusters_3x3 = {
    'cluster_0_3x3': [0, 1, 2, 6, 7, 8, 12, 13, 14],
    'cluster_1_3x3': [3, 4, 5, 9, 10, 11, 15, 16, 17],
    'cluster_2_3x3': [18, 19, 20, 24, 25, 26, 30, 31, 32],
    'cluster_3_3x3': [21, 22, 23, 27, 28, 29, 33, 34, 35],
}

clusters_2x2 = {
    'cluster_0_2x2': [0, 1, 6, 7],       # 좌측 상단
    'cluster_1_2x2': [2, 3, 8, 9],       # 상단 중앙
    'cluster_2_2x2': [4, 5, 10, 11],     # 우측 상단
    'cluster_3_2x2': [12, 13, 18, 19],   # 좌측 중앙
    'cluster_4_2x2': [14, 15, 20, 21],   # 정중앙
    'cluster_5_2x2': [16, 17, 22, 23],   # 우측 중앙
    'cluster_6_2x2': [24, 25, 30, 31],   # 좌측 하단
    'cluster_7_2x2': [26, 27, 32, 33],   # 하단 중앙
    'cluster_8_2x2': [28, 29, 34, 35]    # 우측 하단
}

clusters_3x3_cols = [k for k in clusters_3x3.keys()]
clusters_2x2_cols = [k for k in clusters_2x2.keys()]

chiplet_to_cluster_2x2 = {}
for cluster_name, chiplet_ids in clusters_2x2.items():
    for chip_id in chiplet_ids:
        chiplet_to_cluster_2x2[chip_id] = cluster_name

chiplet_to_cluster_3x3 = {}
for cluster_name, chiplet_ids in clusters_3x3.items():
    for chip_id in chiplet_ids:
        chiplet_to_cluster_3x3[chip_id] = cluster_name


def addr_metrics_interval(df, benchmark, result_dir, time_interval=10000, ns=1e-9):
    curr_chip_cols = [str(i) for i in range(36)]

    # 1. 시간 정규화 및 interval 생성
    t_min = df['time'].min()
    df['norm_time'] = (df['time'] - t_min) / ns
    df['interval'] = (df['norm_time'] // time_interval) * time_interval

    # 2. interval별 전체 기준값 (분모)
    interval_total_cachelines_df = df.groupby('interval', as_index=False).agg(
        interval_total_cachelines=('addr', 'nunique')
    )
    interval_total_df = df.groupby('interval', as_index=False).agg(
        interval_total_access=('access_count', 'sum')
    )

    # 3. 2x2, 3x3 클러스터 컬럼 생성 (기존과 동일)
    for cluster_name, chiplet_ids in clusters_2x2.items():
        str_cols = [str(x) for x in chiplet_ids]
        df[cluster_name] = df[str_cols].sum(axis=1)

    for cluster_name, chiplet_ids in clusters_3x3.items():
        str_cols = [str(x) for x in chiplet_ids]
        df[cluster_name] = df[str_cols].sum(axis=1)

    # 4. interval × addr 집계 (sharing 판별용) – 기존 interval_addr_df 그대로 재사용
    agg_dict = {col: 'sum' for col in curr_chip_cols}
    agg_dict.update({col: 'sum' for col in clusters_2x2})
    agg_dict.update({col: 'sum' for col in clusters_3x3})
    agg_dict['rw'] = 'max'
    agg_dict['owner'] = 'first'

    interval_addr_df = df.groupby(['interval', 'addr'], as_index=False).agg(agg_dict)

    # owner 기반 마스킹 (기존과 동일)
    interval_addr_df['owner_cluster_2x2'] = interval_addr_df['owner'].map(chiplet_to_cluster_2x2)
    interval_addr_df['owner_cluster_3x3'] = interval_addr_df['owner'].map(chiplet_to_cluster_3x3)

    owner_vals = np.array(interval_addr_df['owner'], dtype=str)[:, None]
    col_vals = np.array(curr_chip_cols, dtype=str)
    is_owner = owner_vals == col_vals
    interval_addr_df[curr_chip_cols] = interval_addr_df[curr_chip_cols].mask(is_owner, 0)

    owner_vals_2x2 = np.array(interval_addr_df['owner_cluster_2x2'], dtype=str)[:, None]
    col_vals_2x2 = np.array(clusters_2x2_cols, dtype=str)
    is_owner_2x2 = owner_vals_2x2 == col_vals_2x2
    interval_addr_df[clusters_2x2_cols] = interval_addr_df[clusters_2x2_cols].mask(is_owner_2x2, 0)

    owner_vals_3x3 = np.array(interval_addr_df['owner_cluster_3x3'], dtype=str)[:, None]
    col_vals_3x3 = np.array(clusters_3x3_cols, dtype=str)
    is_owner_3x3 = owner_vals_3x3 == col_vals_3x3
    interval_addr_df[clusters_3x3_cols] = interval_addr_df[clusters_3x3_cols].mask(is_owner_3x3, 0)

    # sharing flag 및 degree 계산 (addr 기준)
    interval_addr_df['is_shared'] = (interval_addr_df[curr_chip_cols] != 0).any(axis=1)
    interval_addr_df['shared_degree'] = (interval_addr_df[curr_chip_cols] != 0).sum(axis=1)
    interval_addr_df['shared_degree_2x2'] = (interval_addr_df[clusters_2x2_cols] != 0).sum(axis=1)
    interval_addr_df['shared_degree_3x3'] = (interval_addr_df[clusters_3x3_cols] != 0).sum(axis=1)

    interval_addr_df['is_rw_interval'] = interval_addr_df['rw'] == 1
    interval_addr_df['is_ro_interval'] = interval_addr_df['rw'] == 0

    # 5. interval × addr 기준 metric 집계
    #    여기서는 PC를 완전히 무시하고, addr 레벨 집계만 생성
    #    access_count: 해당 interval에서 이 addr로의 총 접근 수
    addr_access_df = df.groupby(['interval', 'addr'], as_index=False).agg(
        access_count=('access_count', 'sum'),
    )

    # sharing/degree 정보 merge
    addr_metrics = addr_access_df.merge(
        interval_addr_df[[
            'interval', 'addr',
            'is_shared', 'is_rw_interval', 'is_ro_interval',
            'shared_degree', 'shared_degree_2x2', 'shared_degree_3x3'
        ]],
        on=['interval', 'addr'],
        how='left'
    )

    # 6. interval 전체 대비 비율 계산
    addr_metrics = addr_metrics.merge(interval_total_cachelines_df, on='interval', how='left')
    addr_metrics = addr_metrics.merge(interval_total_df, on='interval', how='left')

    # 캐시라인 비율: 이 interval에서 전체 캐시라인 수 대비 (그냥 1개 캐시라인이므로 1 / total)
    addr_metrics['cacheline_ratio'] = 1.0 / addr_metrics['interval_total_cachelines'].replace(0, np.nan)
    # 접근 비율: interval 전체 access 대비 이 addr access 비율
    addr_metrics['access_ratio'] = addr_metrics['access_count'] / addr_metrics['interval_total_access'].replace(0, np.nan)

    # NaN → 0
    addr_metrics['cacheline_ratio'] = addr_metrics['cacheline_ratio'].fillna(0.0)
    addr_metrics['access_ratio'] = addr_metrics['access_ratio'].fillna(0.0)

    # 7. 정렬 (interval, access_count 내림차순)
    addr_metrics = addr_metrics.sort_values(['interval', 'access_count'], ascending=[True, False])

    # 8. 저장
    out_dir = f"{result_dir}/{benchmark}/{time_interval}"
    os.makedirs(f"{out_dir}", exist_ok=True)

    out_path = f"{out_dir}/{benchmark}_addr_rdma_metrics.csv"
    addr_metrics.to_csv(out_path, index=False)
    print(f"✅ Saved: {out_path}")


if __name__=="__main__":
    import argparse
    parser = argparse.ArgumentParser()
    parser.add_argument('--date', type=str, default='20260221-223953', help='')
    args = parser.parse_args()

    date = args.date

    expanded_dir = f"{memory_trace_dir(date)}/expanded"

    result_dir = f"{username}/mgvm/analyze/_out/address/addr-rdma-metrics"
    os.makedirs(f"{result_dir}", exist_ok=True)

    time_intervals = [1000, 5000, 10000, 20000, 50000, 100000]

    for benchmark in benchmarks:
        # if benchmark != "bfs":
            # continue

        trace_file = f"{expanded_dir}/{benchmark}_mesh_6x6_memory_trace.csv"

        if not os.path.exists(trace_file):
            print(f"No {trace_file}, skip")
            continue
        else:
            print(f"Processing {benchmark}")

        df = pd.read_csv(trace_file, skipinitialspace=True)
        df = df.drop_duplicates()

        for time_interval in time_intervals:
            addr_metrics_interval(df=df.copy(), benchmark=benchmark,
                                result_dir=result_dir, time_interval=time_interval)