import pandas as pd
import re
import matplotlib.pyplot as plt
import os
from ast import literal_eval
from collections import Counter
import math
import numpy as np
import seaborn as sns

from _paths import memory_trace_dir

username = os.path.expanduser('~')

num_chiplets = 36
ns = 0.000000001
chip_cols = [str(i) for i in range(36)]

benchmarks = [
    # Low-sharing Apps
    "fdtd2d",
    "jacobi1d",
    "fastwalshtransform",
    "relu",
    "fft",
    "fir",
    "sssp",
    "convolution2d",
    "color",
    "bitonicsort",
    "pagerank",
    "simpleconvolution",

    # High-sharing Apps
    'gups',    
    'syr2k',
    'spmv',
    'mis', 
    'stencil2d',
    'gemm',
    'lu',
    'matrixmultiplication',
    'mm2',
    'mm3',
    'gaussian',
    'matrixtranspose',
    'im2col',
    'vgg16',
    'bfs',
]

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

if __name__ == "__main__":
    import argparse
    parser = argparse.ArgumentParser()
    parser.add_argument('--date', type=str, default=1, help='')
    args = parser.parse_args()

    date = args.date

    trace_dir = memory_trace_dir(date)
    result_dir = f"{trace_dir}/expanded"

    if not os.path.isdir(f'{result_dir}'):
        os.makedirs(f'{result_dir}', exist_ok=True, mode=0o777)

    for benchmark in benchmarks:
        if os.path.exists(f"{result_dir}/{benchmark}_mesh_6x6_memory_trace.csv"):
            print(f"{benchmark} already exists, skip")
            continue

        trace_file = f"{trace_dir}/{benchmark}_mesh_6x6_memory_trace.csv"

        if not os.path.exists(trace_file):
            print(f"No {trace_file}, skip")
            continue

        print(f"Expanding {trace_file}")

        df = pd.read_csv(trace_file, skipinitialspace=True)
        df = df.drop_duplicates()
        access_dicts = df['access'].apply(parse_access_str)
        chips_df = pd.DataFrame(access_dicts.tolist()).reindex(columns=range(36)).fillna(0).astype(int)
        df_merged = pd.concat([df[['time', 'pc', 'addr', 'owner', 'rw']], chips_df], axis=1)

        chip_cols = list(range(36))

        # 1. access_count: 각 행(Row)에서 0~35번 칩렛 값들을 가로로 모두 더함
        df_merged['access_count'] = df_merged[chip_cols].sum(axis=1)

        # 2. shared_chiplets: 각 행(Row)에서 0보다 큰 값을 가진 칩렛 컬럼의 개수를 셈
        df_merged['shared_chiplets'] = (df_merged[chip_cols] > 0).sum(axis=1)

        df_merged.to_csv(f"{result_dir}/{benchmark}_mesh_6x6_memory_trace.csv", index=False)