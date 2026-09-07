import pandas as pd
import re
import os
import sys
import math
import numpy as np
import matplotlib.pyplot as plt
import matplotlib.patches as mpatches
import seaborn as sns
import matplotlib.lines as mlines
import matplotlib.ticker as ticker

username = os.path.expanduser('~')

# params.py lives in ../scripts (sweep config), not in this analysis dir.
sys.path.insert(0, os.path.join(os.path.dirname(os.path.abspath(__file__)), '..', 'scripts'))

from formatter import get_rdma_df, \
    get_aggregated_df, \
    num_chiplets, ns, chip_cols, \
    clusters_3x3, clusters_2x2, \
    clusters_3x3_cols, clusters_2x2_cols, \
    chiplet_to_cluster_2x2, chiplet_to_cluster_3x3

from params import benchmarks

from pc_metrics_plotter import get_pc_mapping, \
    plot_scatter \
    
from addr_metrics_plotter import plot_memory_access_pattern, \
    plot_address_metrics

from _paths import memory_trace_dir


if __name__=="__main__":
    time_intervals = [1000, 10000, 20000, 100000]
    shared_types = ['ro_shared', 'rw_shared', 'shared']

    date = "20260407-001102-mesh"
    expanded_dir = f"{memory_trace_dir(date)}/expanded"

    result_dir = f"{username}/mgvm/analyze/_out/pc/pc-rdma-metrics-fig"
    os.makedirs(f"{result_dir}", exist_ok=True)

    for benchmark in benchmarks:
        # if benchmark != "fft":
            # continue

        print(f"\n>>> Processing Benchmark: {benchmark}")

        trace_file = f"{expanded_dir}/{benchmark}_mesh_6x6_memory_trace.csv"
        if not os.path.exists(trace_file):
            print(f"No trace of {benchmark}")
            continue

        trace_df = pd.read_csv(trace_file, skipinitialspace=True)
        trace_df = trace_df.drop_duplicates()
        rdma_df = get_rdma_df(trace_df)
        agg_df = get_aggregated_df(rdma_df)

        # ✅ 글로벌 색상 및 마커 매핑 생성 (단일 벤치마크 내 절대적 일관성)
        pc_totals = agg_df.groupby('pc')['access_count'].mean()
        top_n = len(pc_totals)
        pc_num = "all" if top_n == agg_df['pc'].nunique() else f"{top_n}"
        
        # 여기서 추출한 top_pcs는 단순히 순위별로 자르기 위함이고, 매핑은 전체 unique PC에 대해 진행하는 것이 가장 안전합니다.
        # 하지만 주어진 코드를 최대한 유지하며 작성했습니다.
        top_pcs = pc_totals.sort_values(ascending=False).head(top_n).index.tolist()

        pc_color_map, pc_marker_map, pc_num = get_pc_mapping(agg_df, benchmark, top_n)
        print(f"✅ Generated {pc_num} PCs mapping for {benchmark}")

        plot_memory_access_pattern(df=rdma_df,
                                benchmark=benchmark, 
                                result_dir=result_dir, 
                                color_map=pc_color_map, 
                                marker_map=pc_marker_map)
        plot_address_metrics(df=agg_df, 
                            benchmark=benchmark, 
                            result_dir=result_dir, 
                            color_map=pc_color_map, 
                            marker_map=pc_marker_map,
                            value="access_count")
        plot_address_metrics(df=agg_df, 
                            benchmark=benchmark, 
                            result_dir=result_dir, 
                            color_map=pc_color_map, 
                            marker_map=pc_marker_map,
                            value="shared_degree")
        
        for time_interval in time_intervals:
            rdma_metrics_file = f"{username}/mgvm/analyze/_out/pc/pc-rdma-metrics/{benchmark}/{benchmark}_pc_rdma_metrics_{time_interval}.csv"

            if not os.path.exists(rdma_metrics_file):
                print(f"{benchmark} not exists") 
                continue

            df = pd.read_csv(rdma_metrics_file, skipinitialspace=True)

            df['shared_cacheline_ratio_per_pc'] = df['shared_cacheline_count'] / df['pc_total_cachelines']
            df['ro_shared_cacheline_ratio_per_pc'] = df['ro_shared_cacheline_count'] / df['pc_total_cachelines']
            df['rw_shared_cacheline_ratio_per_pc'] = df['rw_shared_cacheline_count'] / df['pc_total_cachelines']

            # ✅ 모든 plot_scatter 호출 시 color_map과 함께 marker_map도 전달
            for shared_type in shared_types:
                plot_scatter(df=df, benchmark=benchmark, result_dir=result_dir, 
                            value=f'{shared_type}_access_ratio', time_interval=time_interval,
                            top_pcs=top_pcs, pc_num=pc_num, color_map=pc_color_map, marker_map=pc_marker_map)
                
                plot_scatter(df=df, benchmark=benchmark, result_dir=result_dir, 
                            value=f'{shared_type}_access_count', time_interval=time_interval,
                            top_pcs=top_pcs, pc_num=pc_num, color_map=pc_color_map, marker_map=pc_marker_map)
                
                plot_scatter(df=df, benchmark=benchmark, result_dir=result_dir, 
                            value=f'{shared_type}_access_mean', time_interval=time_interval,
                            top_pcs=top_pcs, pc_num=pc_num, color_map=pc_color_map, marker_map=pc_marker_map)
                
                plot_scatter(df=df, benchmark=benchmark, result_dir=result_dir, 
                            value=f'{shared_type}_access_std', time_interval=time_interval,
                            top_pcs=top_pcs, pc_num=pc_num, color_map=pc_color_map, marker_map=pc_marker_map)
                
                plot_scatter(df=df, benchmark=benchmark, result_dir=result_dir, 
                            value=f'{shared_type}_degree_mean', time_interval=time_interval,
                            top_pcs=top_pcs, pc_num=pc_num, color_map=pc_color_map, marker_map=pc_marker_map)
                
                plot_scatter(df=df, benchmark=benchmark, result_dir=result_dir, 
                            value=f'{shared_type}_degree_std', time_interval=time_interval,
                            top_pcs=top_pcs, pc_num=pc_num, color_map=pc_color_map, marker_map=pc_marker_map)
                
                plot_scatter(df=df, benchmark=benchmark, result_dir=result_dir, 
                            value=f'{shared_type}_degree_2x2_mean', time_interval=time_interval,
                            top_pcs=top_pcs, pc_num=pc_num, color_map=pc_color_map, marker_map=pc_marker_map)
                
                plot_scatter(df=df, benchmark=benchmark, result_dir=result_dir, 
                            value=f'{shared_type}_degree_2x2_std', time_interval=time_interval,
                            top_pcs=top_pcs, pc_num=pc_num, color_map=pc_color_map, marker_map=pc_marker_map)
                
                plot_scatter(df=df, benchmark=benchmark, result_dir=result_dir, 
                            value=f'{shared_type}_degree_3x3_mean', time_interval=time_interval,
                            top_pcs=top_pcs, pc_num=pc_num, color_map=pc_color_map, marker_map=pc_marker_map)
                
                plot_scatter(df=df, benchmark=benchmark, result_dir=result_dir, 
                            value=f'{shared_type}_degree_3x3_std', time_interval=time_interval,
                            top_pcs=top_pcs, pc_num=pc_num, color_map=pc_color_map, marker_map=pc_marker_map)
                
                plot_scatter(df=df, benchmark=benchmark, result_dir=result_dir, 
                            value=f'{shared_type}_cacheline_count', time_interval=time_interval,
                            top_pcs=top_pcs, pc_num=pc_num, color_map=pc_color_map, marker_map=pc_marker_map)
                
                plot_scatter(df=df, benchmark=benchmark, result_dir=result_dir, 
                            value=f'{shared_type}_cacheline_ratio_per_pc', time_interval=time_interval,
                            top_pcs=top_pcs, pc_num=pc_num, color_map=pc_color_map, marker_map=pc_marker_map)