import numpy as np

num_chiplets = 36
ns = 0.000000001
chip_cols = [str(i) for i in range(36)]

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


def get_rdma_df(df):
    rdma_df = df.copy()
    for i in range(num_chiplets):
        col = str(i)
        rdma_df[col] = np.where(rdma_df['owner'] == i, 0, rdma_df[col])
    rdma_df['is_rdma'] = rdma_df[chip_cols].any(axis=1)
    return rdma_df[rdma_df['is_rdma']]


def get_aggregated_df(df):
    import numpy as np

    for cluster_name, chiplet_ids in clusters_2x2.items():
        str_cols = [str(x) for x in chiplet_ids]
        df[cluster_name] = df[str_cols].sum(axis=1)

    for cluster_name, chiplet_ids in clusters_3x3.items():
        str_cols = [str(x) for x in chiplet_ids]
        df[cluster_name] = df[str_cols].sum(axis=1)
        
    agg_dict = {col: 'sum' for col in chip_cols}
    agg_dict.update({col: 'sum' for col in clusters_2x2})
    agg_dict.update({col: 'sum' for col in clusters_3x3})
    agg_dict['access_count'] = 'sum'
    agg_dict['owner'] = 'first'
    agg_dict['pc'] = 'first'

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

    agg_df['shared_degree'] = (agg_df[chip_cols] != 0).sum(axis=1)
    agg_df['shared_degree_2x2'] = (agg_df[list(clusters_2x2.keys())] != 0).sum(axis=1)
    agg_df['shared_degree_3x3'] = (agg_df[list(clusters_3x3.keys())] != 0).sum(axis=1)
    
    return agg_df


def get_pc_mapping(df, benchmark, top_n=10):
    import numpy as np
    import seaborn as sns
    
    np.random.seed(42)
    
    pc_totals = df.groupby('pc')['ro_shared_access_mean'].mean()
    top_pcs = pc_totals.sort_values(ascending=False).head(top_n).index.tolist()
    
    global_colors = sns.color_palette("tab20", len(top_pcs))
    # 🎯 matplotlib이 바로 인식하는 형태로 변환
    pc_color_map = {pc: global_colors[i] for i, pc in enumerate(top_pcs)}  # list[float] 형태
    MARKER_LIST = ['o', 's', '^', 'D', 'v', '<', '>', 'p', '*', 'h']
    pc_marker_map = {pc: MARKER_LIST[i % len(MARKER_LIST)] for i, pc in enumerate(top_pcs)}
    
    return pc_color_map, pc_marker_map, len(top_pcs)