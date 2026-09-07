import pandas as pd
import os
from ast import literal_eval
from collections import Counter
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


def pc_metrics_total(df, benchmark, result_dir):
    curr_chip_cols = [str(i) for i in range(36)]

    # 1. 클러스터별 access 계산 (원본 로직 유지)
    for cluster_name, chiplet_ids in clusters_2x2.items():
        str_cols = [str(x) for x in chiplet_ids]
        df[cluster_name] = df[str_cols].sum(axis=1)

    for cluster_name, chiplet_ids in clusters_3x3.items():
        str_cols = [str(x) for x in chiplet_ids]
        df[cluster_name] = df[str_cols].sum(axis=1)
    
    # PC별 전체 기준값
    pc_total_df = df.groupby('pc').agg(
        pc_total_cachelines=('addr', 'nunique'),
        pc_total_access=('access_count', 'sum')
    ).reset_index()

    # 3. addr 단위로 묶어서 Sharing 판별 (interval 제거)
    agg_dict = {col: 'sum' for col in curr_chip_cols}
    agg_dict.update({col: 'sum' for col in clusters_2x2})
    agg_dict.update({col: 'sum' for col in clusters_3x3})
    agg_dict['rw'] = 'max'
    agg_dict['owner'] = 'first'
    
    # 핵심: interval 없이 addr로만 그룹화
    addr_df = df.groupby('addr', as_index=False).agg(agg_dict)

    # 2x2, 3x3 클러스터 별 owner 설정
    addr_df['owner_cluster_2x2'] = addr_df['owner'].map(chiplet_to_cluster_2x2)
    addr_df['owner_cluster_3x3'] = addr_df['owner'].map(chiplet_to_cluster_3x3)

    # Owner 접근 마스킹 (기존 NumPy 로직 유지)
    def apply_mask(target_df, target_cols, owner_col):
        owner_vals = np.array(target_df[owner_col], dtype=str)[:, None]
        col_vals = np.array(target_cols, dtype=str)
        mask = (owner_vals == col_vals)
        target_df[target_cols] = target_df[target_cols].mask(mask, 0)
    
    apply_mask(addr_df, curr_chip_cols, 'owner')
    apply_mask(addr_df, list(clusters_2x2.keys()), 'owner_cluster_2x2')
    apply_mask(addr_df, list(clusters_3x3.keys()), 'owner_cluster_3x3')

    # Sharing 여부 및 Degree 계산
    addr_df['is_shared'] = (addr_df[curr_chip_cols] != 0).any(axis=1)
    addr_df['shared_degree'] = (addr_df[curr_chip_cols] != 0).sum(axis=1)
    addr_df['shared_degree_2x2'] = (addr_df[list(clusters_2x2.keys())] != 0).sum(axis=1)
    addr_df['shared_degree_3x3'] = (addr_df[list(clusters_3x3.keys())] != 0).sum(axis=1)
    
    addr_df['is_rw'] = addr_df['rw'] == 1
    addr_df['is_ro'] = addr_df['rw'] == 0

    # 4. 판별된 상태를 원본 데이터에 맵핑
    flags = addr_df[['addr', 'is_shared', 'is_rw', 'is_ro', 
                     'shared_degree', 'shared_degree_2x2', 'shared_degree_3x3']]
    
    # [수정] 만약 df에 이미 같은 이름의 컬럼이 있다면 삭제 후 병합 (중복 방지)
    cols_to_drop = [c for c in flags.columns if c in df.columns and c != 'addr']
    if cols_to_drop:
        df = df.drop(columns=cols_to_drop)
        
    df = df.merge(flags, on='addr', how='left')

    # [디버깅용] 여기서 에러가 난다면 컬럼이 없는 것이니 확인용 출력
    if 'is_shared' not in df.columns:
        print("Columns in df:", df.columns)
        raise KeyError("'is_shared' column missing after merge!")

    # 5. 분할 (RO Shared, RW Shared, Private)
    # 아래 라인에서 KeyError가 발생했으므로, 위 merge가 확실히 되어야 합니다.
    df_sh = df[df['is_shared'].fillna(False)].copy()
    df_ro = df[df['is_shared'].fillna(False) & df['is_ro'].fillna(False)].copy()
    df_rw = df[df['is_shared'].fillna(False) & df['is_rw'].fillna(False)].copy()
    df_pv = df[~df['is_shared'].fillna(False)].copy()

    # 6. PC 단위 집계 함수 (interval 제거 버전)
    def build_pc_group(df_sub):
        if df_sub.empty:
            return pd.DataFrame(columns=['pc', 'addr', 'access_count', 'shared_degree', 'shared_degree_2x2', 'shared_degree_3x3'])
        return df_sub.groupby(['pc', 'addr'], as_index=False).agg(
            access_count=('access_count', 'sum'),
            shared_degree=('shared_degree', 'first'),
            shared_degree_2x2=('shared_degree_2x2', 'first'),
            shared_degree_3x3=('shared_degree_3x3', 'first'),
        )

    # 7. 최종 Metric 계산 (agg_pc_metrics 로직 응용)
    def agg_total(group_df, prefix):
        if group_df.empty: return pd.DataFrame()
        res = group_df.groupby('pc').agg(
            cacheline_count=('addr', 'nunique'),
            access_count=('access_count', 'sum'),
            degree_mean=('shared_degree', 'mean'),
            degree_2x2_mean=('shared_degree_2x2', 'mean'),
            degree_3x3_mean=('shared_degree_3x3', 'mean')
        ).reset_index()
        
        # 비율 계산 (전체 대비가 아닌 해당 PC가 가진 전체 자원 대비)
        # 나중에 pc_total_df와 merge해서 계산하는게 정확합니다.
        return res.rename(columns={c: f'{prefix}_{c}' for c in res.columns if c != 'pc'})

    sh_metrics = agg_total(build_pc_group(df_sh), 'shared')
    ro_metrics = agg_total(build_pc_group(df_ro), 'ro_shared')
    rw_metrics = agg_total(build_pc_group(df_rw), 'rw_shared')
    pv_metrics = agg_total(build_pc_group(df_pv), 'private')

    # 8. 모든 결과 병합
    result_df = pc_total_df.copy()
    for m in [sh_metrics, ro_metrics, rw_metrics, pv_metrics]:
        if not m.empty:
            result_df = result_df.merge(m, on='pc', how='left')

    # 9. 비율 및 결측치 처리 (질문하신 핵심: RO, RW, PV 비율)
    for p in ['shared', 'ro_shared', 'rw_shared', 'private']:
        # PC가 접근하는 전체 캐시라인 중 해당 타입의 비율
        result_df[f'{p}_line_ratio'] = result_df[f'{p}_cacheline_count'].fillna(0) / result_df['pc_total_cachelines']
        # PC의 전체 접근 중 해당 타입에 발생한 접근 비율
        result_df[f'{p}_access_ratio'] = result_df[f'{p}_access_count'].fillna(0) / result_df['pc_total_access']
        # 나머지 결측치 0 채우기
        result_df[f'{p}_cacheline_count'] = result_df[f'{p}_cacheline_count'].fillna(0)

    # 정렬 및 저장
    # result_df = result_df.sort_values('pc_total_access', ascending=False)
    out_path = f"{result_dir}/{benchmark}/{benchmark}_pc_shared_ratio.csv"
    os.makedirs(os.path.dirname(out_path), exist_ok=True)
    result_df.to_csv(out_path, index=False)
    
    print(f"✅ 분석 완료: {out_path}")


if __name__=="__main__":
    import argparse
    parser = argparse.ArgumentParser()
    parser.add_argument('--date', type=str, default='20260221-223953', help='')
    args = parser.parse_args()

    date = args.date

    expanded_dir = f"{memory_trace_dir(date)}/expanded"

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

        pc_metrics_total(df=df, benchmark=benchmark, result_dir=f"{username}/mgvm/analyze/_out/pc/pc-shared-ratio")