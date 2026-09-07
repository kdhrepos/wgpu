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

if __name__ == "__main__":
    import argparse
    parser = argparse.ArgumentParser()
    parser.add_argument('--date', type=str, default='20260221-223953', help='')
    args = parser.parse_args()

    date = args.date
    trace_dir = memory_trace_dir(date)
    expanded_dir = f"{trace_dir}/expanded"

    # RO 구간 설정
    bins = [0, 9, 18, 27, 36]
    labels = ['2~9', '10~18', '19~27', '28~36']

    # 결과 수집용 리스트 (하나로 통합)
    results = []

    for benchmark in benchmarks:
        trace_file = f"{expanded_dir}/{benchmark}_mesh_6x6_memory_trace.csv"

        if not os.path.exists(trace_file):
            print(f"Skip: No {trace_file}")
            continue

        print(f"Processing {benchmark}...")
        df = pd.read_csv(trace_file, skipinitialspace=True)
        df = df.drop_duplicates()

        # ===== addr별 집계 =====
        agg_dict = {col: 'sum' for col in chip_cols}
        agg_dict['rw'] = 'max'
        agg_dict['access_count'] = 'sum'
        agg_dict['owner'] = 'first'
        grouped = df.groupby('addr').agg(agg_dict).reset_index()

        # 1. 0~35 컬럼명만 안전하게 추출
        valid_chip_strs = {str(i) for i in range(36)}
        chip_cols = [col for col in grouped.columns if str(col) in valid_chip_strs]

        # 2. owner 값을 "순수 NumPy 문자열 배열"로 변환 후 2차원(N, 1)으로 세움
        owner_vals = np.array(grouped['owner'], dtype=str)[:, None]  # Shape: (307172, 1)

        # 3. 컬럼명 배열도 "순수 NumPy 문자열 배열"로 변환
        col_vals = np.array(chip_cols, dtype=str)  # Shape: (36,)

        # 4. 브로드캐스팅 비교 (Pandas 간섭 없이 순수 NumPy C엔진으로 초고속 연산됨)
        is_owner = owner_vals == col_vals  # Shape: (307172, 36) 의 True/False 마스크 생성

        # 5. 마스크 적용하여 값 0으로 치환
        grouped[chip_cols] = grouped[chip_cols].mask(is_owner, 0)

        # 6. shared, private group 분리
        is_shared_mask = (grouped[chip_cols] != 0).any(axis=1)
        shared_group = grouped[is_shared_mask]
        private_group = grouped[~is_shared_mask]  # Not 연산자로 간단하게 추출

        total_cachelines = grouped['addr'].nunique()
        total_access_count = grouped['access_count'].sum()

        ro_shared_cnt = len(shared_group[shared_group['rw'] == 0])
        rw_shared_cnt = len(shared_group[shared_group['rw'] == 1])
        private_cnt = len(private_group['addr'])

        ro_shared_acc = shared_group[shared_group['rw'] == 0]['access_count'].sum()
        rw_shared_acc = shared_group[shared_group['rw'] == 1]['access_count'].sum()
        private_acc = private_group['access_count'].sum()

        ro_shared_df = shared_group[shared_group['rw'] == 0]
        ro_shared_degrees = (ro_shared_df[chip_cols] != 0).sum(axis=1)
        # bins = [1, 9, 18, 27, 36], labels = ['2~9', '10~18', '19~27', '28~36']
        ro_cuts = pd.cut(ro_shared_degrees, bins=bins, labels=labels, right=True)
        # 구간별 개수를 비율로 변환한 딕셔너리 생성
        ro_bin_cnts = ro_cuts.value_counts().to_dict()
        # 주의: 분모가 total_cachelines 인지 ro_shared_cnt 인지에 따라 나눗셈 대상을 수정하세요
        ro_bin_cnt_ratios = {lbl: cnt / total_cachelines for lbl, cnt in ro_bin_cnts.items()}

        rw_shared_df = shared_group[shared_group['rw'] == 1]
        rw_shared_degrees = (rw_shared_df[chip_cols] != 0).sum(axis=1)
        # bins = [1, 9, 18, 27, 36], labels = ['2~9', '10~18', '19~27', '28~36']
        rw_cuts = pd.cut(rw_shared_degrees, bins=bins, labels=labels, right=True)
        # 구간별 개수를 비율로 변환한 딕셔너리 생성
        rw_bin_cnts = rw_cuts.value_counts().to_dict()
        # 주의: 분모가 total_cachelines 인지 rw_shared_cnt 인지에 따라 나눗셈 대상을 수정하세요
        rw_bin_cnt_ratios = {lbl: cnt / total_cachelines for lbl, cnt in rw_bin_cnts.items()}

        # ===== 결과 저장 (RO/RW 모두) =====
        result_row = {
            'benchmark': benchmark,
            # Cacheline Count
            'ro_shared_cnt': ro_shared_cnt, 
            'rw_shared_cnt': rw_shared_cnt,
            'private_cnt': private_cnt,
            'ro_shared_cnt_ratio': ro_shared_cnt / total_cachelines,
            'rw_shared_cnt_ratio': rw_shared_cnt / total_cachelines,
            'private_cnt_ratio': private_cnt / total_cachelines, 
            # Cacheline Access Count
            'ro_shared_acc_count': ro_shared_acc, 
            'rw_shared_acc_count': rw_shared_acc,
            'private_acc_count': private_acc,
            'ro_shared_acc_ratio': ro_shared_acc / total_access_count,
            'rw_shared_acc_ratio': rw_shared_acc / total_access_count,
            'private_acc_ratio': private_acc / total_access_count,
            **{f'ro_{lbl}_cnt_ratio': ro_bin_cnt_ratios.get(lbl, 0.0) for lbl in labels},
            **{f'rw_{lbl}_cnt_ratio': rw_bin_cnt_ratios.get(lbl, 0.0) for lbl in labels},
            # Total Numbers
            'total_cachelines': total_cachelines,
            'total_access_count': total_access_count,
        }
        results.append(result_row)
        result_df = pd.DataFrame(results)
        output_dir = f"{username}/mgvm/analyze/_out/cacheline/sharing-degree"
        os.makedirs(output_dir, exist_ok=True)
        result_df.to_csv(f"{output_dir}/sharing_degree_full.csv", index=False, float_format='%.12f')
    
    print("✅ Saved to sharing_degree_full.csv, totally")