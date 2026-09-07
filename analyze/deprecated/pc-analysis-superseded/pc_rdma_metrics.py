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

username = os.path.expanduser('~')

num_chiplets = 36
ns = 0.000000001
chip_cols = [str(i) for i in range(36)]

benchmarks = [
    'gpt3_gemm',
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


def agg_pc_metrics(group_df, kind_prefix, interval_total_cachelines_df, interval_total_df):
    """
    interval × pc metric 집계 (기준값 전달받음)
    기존 shared_chiplets 대신 새로 계산한 shared_degree 사용
    """
    if group_df.empty:
        cols = [f'{kind_prefix}_{col}' for col in 
               ['cacheline_count', 'access_count', 'access_mean', 'access_std', 
                'cacheline_ratio', 'access_ratio', 'degree_mean', 'degree_std',
                'degree_2x2_mean', 'degree_2x2_std',  # 2x2 추가
                'degree_3x3_mean', 'degree_3x3_std'   # 3x3 추가
                ]]
        return pd.DataFrame(columns=['interval', 'pc'] + cols)

    g = (
        group_df.groupby(['interval', 'pc'])
        .agg(
            cacheline_count=('addr', 'nunique'),
            access_count=('access_count', 'sum'),
            access_mean=('access_count', 'mean'),
            access_std=('access_count', 'std'),
            shared_degree_mean=('shared_degree', 'mean'),  # 수정됨
            shared_degree_std=('shared_degree', 'std'),    # 수정됨
            # 2x2, 3x3 클러스터 metric 추가
            shared_degree_2x2_mean=('shared_degree_2x2', 'mean'),
            shared_degree_2x2_std=('shared_degree_2x2', 'std'),
            shared_degree_3x3_mean=('shared_degree_3x3', 'mean'),
            shared_degree_3x3_std=('shared_degree_3x3', 'std'),
        )
        .reset_index()
    )

    # 안전한 merge + division by zero 방지
    g = g.merge(interval_total_cachelines_df, on='interval', how='left')
    g[f'{kind_prefix}_cacheline_ratio'] = (
        g['cacheline_count'] / g['interval_total_cachelines'].fillna(1)
    )

    g = g.merge(interval_total_df, on='interval', how='left')
    g[f'{kind_prefix}_access_ratio'] = (
        g['access_count'] / g['interval_total_access'].fillna(1)
    )

    # prefix 적용
    rename_dict = {
        'cacheline_count': f'{kind_prefix}_cacheline_count',
        'access_count': f'{kind_prefix}_access_count',
        'access_mean': f'{kind_prefix}_access_mean',
        'access_std': f'{kind_prefix}_access_std',
        'shared_degree_mean': f'{kind_prefix}_degree_mean',
        'shared_degree_std': f'{kind_prefix}_degree_std',
        'shared_degree_2x2_mean': f'{kind_prefix}_degree_2x2_mean',
        'shared_degree_2x2_std': f'{kind_prefix}_degree_2x2_std',
        'shared_degree_3x3_mean': f'{kind_prefix}_degree_3x3_mean',
        'shared_degree_3x3_std': f'{kind_prefix}_degree_3x3_std',
    }
    g = g.rename(columns=rename_dict)
    
    # 💡 수정 1: 병합 시 중복(_x, _y) 생성을 막기 위해 빌려온 분모 컬럼 삭제 후 반환
    g = g.drop(columns=['interval_total_cachelines', 'interval_total_access'], errors='ignore')
    
    return g


def pc_metrics_interval(df, benchmark, result_dir, time_interval=10000, ns=1e-9):
    curr_chip_cols = [str(i) for i in range(36)]

    # 1. 시간 정규화
    t_min = df['time'].min()
    df['norm_time'] = (df['time'] - t_min) / ns
    df['interval'] = (df['norm_time'] // time_interval) * time_interval

    # 2. interval별 전체 기준값들 (분모용)
    interval_total_cachelines_df = df.groupby('interval', as_index=False).agg(
        interval_total_cachelines=('addr', 'nunique')
    )
    interval_total_df = df.groupby('interval', as_index=False).agg(
        interval_total_access=('access_count', 'sum')
    )
    pc_interval_total_df = df.groupby(['interval', 'pc'], as_index=False).agg(
        pc_total_cachelines=('addr', 'nunique'),
        pc_total_access=('access_count', 'sum')
    )

    # 2x2, 3x3 클러스터 별 metric 추가
    for cluster_name, chiplet_ids in clusters_2x2.items():
        str_cols = [str(x) for x in chiplet_ids]
        df[cluster_name] = df[str_cols].sum(axis=1)

    for cluster_name, chiplet_ids in clusters_3x3.items():
        str_cols = [str(x) for x in chiplet_ids]
        df[cluster_name] = df[str_cols].sum(axis=1)

    # ---------------------------------------------------------
    # 3. 👇 핵심 수정 로직: interval × addr 단위로 묶어서 Sharing 판별
    # ---------------------------------------------------------
    agg_dict = {col: 'sum' for col in curr_chip_cols}
    agg_dict.update({col: 'sum' for col in clusters_2x2})
    agg_dict.update({col: 'sum' for col in clusters_3x3})
    agg_dict['rw'] = 'max'     # 1이 하나라도 있으면 RW 캐시라인
    agg_dict['owner'] = 'first' # owner는 동일하므로 하나만 취함
    
    interval_addr_df = df.groupby(['interval', 'addr'], as_index=False).agg(agg_dict)

    # 2x2, 3x3 클러스터 별 owner 설정
    interval_addr_df['owner_cluster_2x2'] = interval_addr_df['owner'].map(chiplet_to_cluster_2x2)
    interval_addr_df['owner_cluster_3x3'] = interval_addr_df['owner'].map(chiplet_to_cluster_3x3)

    for i in range(num_chiplets):
        col = str(i)
        interval_addr_df[col] = np.where(interval_addr_df['owner'] == i, 0, interval_addr_df[col])

    for col in clusters_2x2_cols:
        interval_addr_df[col] = np.where(interval_addr_df['owner_cluster_2x2'] == col, 0, interval_addr_df[col])

    for col in clusters_3x3_cols:
        interval_addr_df[col] = np.where(interval_addr_df['owner_cluster_3x3'] == col, 0, interval_addr_df[col])

    # Sharing 여부 및 Sharing Degree(공유된 칩렛 개수) 계산
    interval_addr_df['is_shared'] = (interval_addr_df[curr_chip_cols] != 0).any(axis=1)
    interval_addr_df['shared_degree'] = (interval_addr_df[curr_chip_cols] != 0).sum(axis=1)
    interval_addr_df['shared_degree_2x2'] = (interval_addr_df[clusters_2x2_cols] != 0).sum(axis=1)
    interval_addr_df['shared_degree_3x3'] = (interval_addr_df[clusters_3x3_cols] != 0).sum(axis=1)
    
    # interval 기준 RO/RW 판별
    interval_addr_df['is_rw_interval'] = interval_addr_df['rw'] == 1
    interval_addr_df['is_ro_interval'] = interval_addr_df['rw'] == 0

    # ---------------------------------------------------------
    # 4. 판별된 상태(Flag)를 원본 데이터에 맵핑
    # ---------------------------------------------------------
    flags = interval_addr_df[['interval', 'addr', 
                              'is_shared', 'is_rw_interval', 'is_ro_interval', 
                              'shared_degree', 'shared_degree_2x2', 'shared_degree_3x3']]
    df = df.merge(flags, on=['interval', 'addr'], how='left')

    # ---------------------------------------------------------
    # 5. RO Shared, RW Shared, Private 행 필터링 (4분할)
    # ---------------------------------------------------------
    df_sh = df[df['is_shared']].copy()
    df_ro = df[df['is_shared'] & df['is_ro_interval']].copy()
    df_rw = df[df['is_shared'] & df['is_rw_interval']].copy()
    df_pv = df[~df['is_shared']].copy()  # ✨ Private 데이터 분할

    # ---------------------------------------------------------
    # 6. interval × pc × addr 집계
    # ---------------------------------------------------------
    def build_group(df_sub):
        if df_sub.empty:
            # 빈 DataFrame일 때도 2x2, 3x3 컬럼 포함
            return pd.DataFrame(columns=['interval', 'pc', 'addr', 'access_count', 
                                         'shared_degree', 'shared_degree_2x2', 'shared_degree_3x3'])
        return df_sub.groupby(['interval', 'pc', 'addr'], as_index=False).agg(
            access_count=('access_count', 'sum'),
            shared_degree=('shared_degree', 'first'),
            # 2x2, 3x3 값 포함해서 넘겨주기
            shared_degree_2x2=('shared_degree_2x2', 'first'),
            shared_degree_3x3=('shared_degree_3x3', 'first'),
        )

    sh_group = build_group(df_sh)
    ro_group = build_group(df_ro)
    rw_group = build_group(df_rw)
    pv_group = build_group(df_pv)

    # ---------------------------------------------------------
    # 7. PC 단위 최종 병합 (Shared, RO, RW, Private 모두)
    # ---------------------------------------------------------
    sh_metrics = agg_pc_metrics(sh_group, 'shared', interval_total_cachelines_df, interval_total_df)
    ro_metrics = agg_pc_metrics(ro_group, 'ro_shared', interval_total_cachelines_df, interval_total_df)
    rw_metrics = agg_pc_metrics(rw_group, 'rw_shared', interval_total_cachelines_df, interval_total_df)
    pv_metrics = agg_pc_metrics(pv_group, 'private', interval_total_cachelines_df, interval_total_df)

    # 수정 2: 병합 시 빈 데이터프레임 처리 강건화 및 누락 없이 outer merge
    result_df = pd.merge(sh_metrics, ro_metrics, on=['interval', 'pc'], how='outer')
    result_df = pd.merge(result_df, rw_metrics, on=['interval', 'pc'], how='outer')
    result_df = pd.merge(result_df, pv_metrics, on=['interval', 'pc'], how='outer')
    result_df = pd.merge(result_df, pc_interval_total_df, on=['interval', 'pc'], how='left')
    
    # 수정 3: 최종 결과에 분모 컬럼들 1번만 명시적 병합 (_x, _y 방지)
    result_df = pd.merge(result_df, interval_total_cachelines_df, on='interval', how='left')
    result_df = pd.merge(result_df, interval_total_df, on='interval', how='left')

    # 누락 컬럼 0으로 채우기 (shared 추가)
    for prefix in ['shared', 'ro_shared', 'rw_shared', 'private']:
        for col in ['cacheline_count', 'access_count', 'access_mean', 'access_std',
                    'cacheline_ratio', 'access_ratio', 
                    'shared_degree_mean', 'shared_degree_std',
                    'shared_degree_2x2_mean', 'shared_degree_2x2_std',
                    'shared_degree_3x3_mean', 'shared_degree_3x3_std']:
            full_col = f'{prefix}_{col}'
            if full_col not in result_df.columns:
                result_df[full_col] = 0.0
            else:
                result_df[full_col] = result_df[full_col].fillna(0.0)

    # 정렬 (interval 우선, 그 다음 접근 횟수 높은 순)
    sort_cols = ['interval']
    for col in ['shared_access_count', 'ro_shared_access_count']: # 💡 수정 4: 가장 큰 값인 shared 기준 정렬
        if col in result_df.columns:
            sort_cols.append(col)
    
    result_df = result_df.sort_values(sort_cols, ascending=[True] + [False]*(len(sort_cols)-1))

    # 저장
    out_path = f"{result_dir}/{benchmark}/{benchmark}_pc_rdma_metrics_{time_interval}.csv"
    os.makedirs(f"{result_dir}/{benchmark}", exist_ok=True)
    
    result_df.to_csv(out_path, index=False)
    print(f"✅ Saved: {out_path}")


if __name__=="__main__":
    import argparse
    parser = argparse.ArgumentParser()
    parser.add_argument('--date', type=str, default='20260407-001102-mesh', help='')
    args = parser.parse_args()

    date = args.date

    base_dir = f'{username}/mgvm/results/memory-traces'
    expanded_dir = f"{base_dir}/{date}/expanded"

    result_dir = f"./pc-rdma-metrics"
    os.makedirs(f"{result_dir}", exist_ok=True)

    time_intervals = [1000, 10000, 20000, 100000]

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

        for time_interval in time_intervals:
            pc_metrics_interval(df=df.copy(), benchmark=benchmark,
                                result_dir=result_dir, time_interval=time_interval)