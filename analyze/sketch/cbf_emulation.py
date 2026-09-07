import pandas as pd
import matplotlib.pyplot as plt
import os
import numpy as np
from cbf_params import params
import mmh3 # MurmurHash3 (하드웨어 해시와 유사, pip install mmh3 필요)

from _paths import memory_trace_dir

username = os.path.expanduser('~')

benchmarks = [
    # High-sharing Apps
    'gups',    
    'syr2k',
    'mm2', 
    'mm3', 
    'gemm',
    'gaussian',
    'gramschmidt',
    'vgg16',
    'lenet',
    'spmv',
    'mis',
    'stencil2d',
    'matrixmultiplication',
    'matrixtranspose',
    'im2col',
    'bfs',
    'kmeans',
    'lud',

    # Low-sharing Apps
    "fdtd2d", "jacobi1d", "fastwalshtransform", "relu", "fft", "fir", 
    "sssp", "convolution2d", "color", "bitonicsort", "pagerank", "simpleconvolution",
]

num_chiplets = 36
curr_chip_cols = [str(i) for i in range(36)]

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

def get_cluster_id(id):
    row = id // 6
    col = id % 6

    partRow = row // 3
    partCol = col // 3

    return partRow * 2 + partCol

def get_aggregated_df(df):
    for cluster_name, chiplet_ids in clusters_2x2.items():
        str_cols = [str(x) for x in chiplet_ids]
        df[cluster_name] = df[str_cols].sum(axis=1)

    for cluster_name, chiplet_ids in clusters_3x3.items():
        str_cols = [str(x) for x in chiplet_ids]
        df[cluster_name] = df[str_cols].sum(axis=1)
        
    agg_dict = {col: 'sum' for col in curr_chip_cols}
    agg_dict.update({col: 'sum' for col in clusters_2x2})
    agg_dict.update({col: 'sum' for col in clusters_3x3})
    agg_dict['access_count'] = 'sum'
    agg_dict['owner'] = 'first'

    agg_df = df.groupby('addr', as_index=False).agg(agg_dict)
    agg_df['owner_cluster_2x2'] = agg_df['owner'].map(chiplet_to_cluster_2x2)
    agg_df['owner_cluster_3x3'] = agg_df['owner'].map(chiplet_to_cluster_3x3)

    for i in range(num_chiplets):
        col = str(i)
        agg_df[col] = np.where(agg_df['owner'] == i, 0, agg_df[col])

    for col in clusters_2x2_cols:
        agg_df[col] = np.where(agg_df['owner_cluster_2x2'] == col, 0, agg_df[col])

    for col in clusters_3x3_cols:
        agg_df[col] = np.where(agg_df['owner_cluster_3x3'] == col, 0, agg_df[col])

    agg_df['shared_degree'] = (agg_df[curr_chip_cols] != 0).sum(axis=1)
    agg_df['shared_degree_2x2'] = (agg_df[list(clusters_2x2.keys())] != 0).sum(axis=1)
    agg_df['shared_degree_3x3'] = (agg_df[list(clusters_3x3.keys())] != 0).sum(axis=1)
    
    return agg_df

sub_dir = "20260329-231025-mesh"
platform = "mesh"
expanded_dir = f'{memory_trace_dir(sub_dir)}/expanded'

# 공통 하드웨어 설정
CBF_SIZE_BITS = 32
CBF_SIZE = 1024 * CBF_SIZE_BITS 
COUNTER_BITS = 8
MAX_COUNT = 255
NUM_HASHES = 3
DECAY_INTERVAL = 10000

# 결과 저장을 위한 리스트
all_results = []

for benchmark in benchmarks:
    # if benchmark != "syr2k":
        # continue

    print(f"\n>>> Processing Benchmark: {benchmark}")

    p = params.get(benchmark)
    trace_file = f"{expanded_dir}/{benchmark}_{platform}_6x6_memory_trace.csv"

    if not os.path.exists(trace_file):
        print(f"  Skip: No file {trace_file}")
        continue

    df = pd.read_csv(trace_file, skipinitialspace=True)
    df = df.drop_duplicates()          # full-row 완전-중복 제거

    rdma_df = df.copy()
    for i in range(num_chiplets):
        col = str(i)
        rdma_df[col] = np.where(rdma_df['owner'] == i, 0, rdma_df[col])

    df['is_rdma'] = rdma_df[curr_chip_cols].any(axis=1)

    vals = df[curr_chip_cols].values
    mask = vals != 0

    col_names = np.array(curr_chip_cols)

    df['requestors'] = [col_names[m].tolist() for m in mask]
    rdma_df = df[df['is_rdma'] == True]

    agg_df = get_aggregated_df(df)
    
    # 3. CBF 초기화 및 에뮬레이션 (작성하신 코드 핵심 로직)
    target_cbfs = np.zeros((num_chiplets, CBF_SIZE), dtype=np.uint8)
    last_decay_times = np.full(num_chiplets, rdma_df['time'].iloc[0])

    cbf_predicted_global = set()
    cbf_predicted_hot = set()
    cbf_predicted_warm = set()

    for _, row in rdma_df.iterrows():
        curr_time = row['time']
        owner = int(row['owner'])
        src_list = row['requestors'] if isinstance(row['requestors'], list) else [int(row['requestors'])]
        addr = int(row['addr'])
        key_bytes = addr.to_bytes(8, 'little')

        # Decay 로직
        for chip_id in range(num_chiplets):
            if curr_time - last_decay_times[chip_id] >= DECAY_INTERVAL:
                target_cbfs[chip_id] >>= 1
                last_decay_times[chip_id] = curr_time

        # Update
        for src in src_list:
            cls_id = get_cluster_id(int(src))
            h_idx = mmh3.hash(key_bytes, cls_id) % CBF_SIZE

            if target_cbfs[owner, h_idx] < MAX_COUNT:
                target_cbfs[owner, h_idx] += 1
        
        # Identification (HOT_THRESHOLD/WARM_THRESHOLD는 8-bit 카운터 포화 기준인 255/63 등 고정값 사용 권장)
        # 벤치마크 스케일과 상관없이 '카운터가 꽉 찼는가'가 중요하기 때문
        global_count = 0
        hot_count = 0
        warm_count = 0
        for cls_id in range(NUM_HASHES):
            h_idx = mmh3.hash(key_bytes, cls_id) % CBF_SIZE
            if target_cbfs[owner, h_idx] >= 255: # Global threshold (Saturation)
                global_count += 1
            if target_cbfs[owner, h_idx] >= 127: # Hot threshold
                hot_count += 1
            elif target_cbfs[owner, h_idx] >= 63: # Warm threshold
                warm_count += 1

        if global_count >= 2: cbf_predicted_global.add(addr)
        elif hot_count >= 2: cbf_predicted_hot.add(addr)
        elif warm_count >= 2: cbf_predicted_warm.add(addr)

    # 5. 리스트에 저장
    all_results.append({
        "Benchmark": benchmark,
        "Pred_Global_Cachelines": len(cbf_predicted_global),
        "Pred_Hot_Cachelines": len(cbf_predicted_hot),
        "Pred_Warm_Cachelines": len(cbf_predicted_warm),

        "Pred_Glob_Acc_Mean": agg_df[agg_df['addr'].isin(cbf_predicted_global)]['access_count'].mean() if cbf_predicted_global else 0,
        "Pred_Hot_Acc_Mean": agg_df[agg_df['addr'].isin(cbf_predicted_hot)]['access_count'].mean() if cbf_predicted_hot else 0,
        "Pred_Warm_Acc_Mean": agg_df[agg_df['addr'].isin(cbf_predicted_warm)]['access_count'].mean() if cbf_predicted_warm else 0,

        "Pred_Glob_Acc_Std": agg_df[agg_df['addr'].isin(cbf_predicted_global)]['access_count'].std() if cbf_predicted_global else 0,
        "Pred_Hot_Acc_Std": agg_df[agg_df['addr'].isin(cbf_predicted_hot)]['access_count'].std() if cbf_predicted_hot else 0,
        "Pred_Warm_Acc_Std": agg_df[agg_df['addr'].isin(cbf_predicted_warm)]['access_count'].std() if cbf_predicted_warm else 0,

        "Pred_Glob_Shr_Mean": agg_df[agg_df['addr'].isin(cbf_predicted_global)]['shared_degree'].mean() if cbf_predicted_global else 0,
        "Pred_Hot_Shr_Mean": agg_df[agg_df['addr'].isin(cbf_predicted_hot)]['shared_degree'].mean() if cbf_predicted_hot else 0,
        "Pred_Warm_Shr_Mean": agg_df[agg_df['addr'].isin(cbf_predicted_warm)]['shared_degree'].mean() if cbf_predicted_warm else 0,

        "Pred_Glob_Shr_Std": agg_df[agg_df['addr'].isin(cbf_predicted_global)]['shared_degree'].std() if cbf_predicted_global else 0,
        "Pred_Hot_Shr_Std": agg_df[agg_df['addr'].isin(cbf_predicted_hot)]['shared_degree'].std() if cbf_predicted_hot else 0,
        "Pred_Warm_Shr_Std": agg_df[agg_df['addr'].isin(cbf_predicted_warm)]['shared_degree'].std() if cbf_predicted_warm else 0,
    })

    # [핵심 수정] 매 벤치마크 루프가 끝날 때마다 즉시 파일로 저장
    # 이렇게 하면 시뮬레이션 도중 중단되어도 지금까지의 결과는 보존됩니다.
    current_results_df = pd.DataFrame(all_results)
    current_results_df.to_csv(f"cbf_simulation_summary_{CBF_SIZE_BITS}.csv", index=False)
    print(f"[✓] Benchmark '{benchmark}' completed and saved to CSV.")

print(f"\n[!] All simulations finished. Final results are in 'cbf_simulation_summary_{CBF_SIZE_BITS}.csv'.")