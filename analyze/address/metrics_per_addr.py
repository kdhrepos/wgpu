import pandas as pd
import sys
import re
import matplotlib.pyplot as plt
import matplotlib.ticker as mtick
import os
import numpy as np
import seaborn as sns

from concurrent.futures import ProcessPoolExecutor
from itertools import repeat

from params import benchmarks
from formatter import num_chiplets, ns, chip_cols
from metrics_per_addr_plotter \
    import plot_access_count_per_addrs, \
        plot_temporal_access_and_footprint, \
        plot_memory_access_trace, \
        plot_temporal_mean_std_analysis, \
        plot_access_count_with_shared_degree, \
        plot_temporal_access_and_footprint_per_chiplet, \
        plot_access_count_per_pages \

from _paths import memory_trace_dir

username = os.path.expanduser('~')
# run_id 결정: argv 첫 positional, 없으면 'latest'
_run_id = next((a for a in sys.argv[1:] if not a.startswith('-')), 'latest')
from _paths import run_root as _run_root, memory_trace_dir as _mtd, run_tag as _run_tag
date = _run_tag(_run_id)
trace_dir = str(_mtd(_run_id))

xDim=6
yDim=6


def process_benchmark(benchmark):
    print(f">>> Processing Benchmark: {benchmark}")
    trace_file = f"{trace_dir}/{benchmark}_mesh_{xDim}x{yDim}_memory_trace.csv"

    if not os.path.exists(trace_file):
        print(f"   Skip: No file {trace_file}")
        return {'benchmark': benchmark, 'status': 'Skip (No File)'}

    try:
        # 1. 결과 저장 디렉토리 분리 (./date 내 하위 디렉토리 생성)
        result_dir_addr = f"{username}/mgvm/analyze/_out/address/{date}/plots/access-distribution-addr"
        result_dir_temporal = f"{username}/mgvm/analyze/_out/address/{date}/plots/temporal-analysis"
        result_dir_temporal_per_chiplet = f"{username}/mgvm/analyze/_out/address/{date}/plots/temporal-analysis-per-chiplet"
        result_dir_trace = f"{username}/mgvm/analyze/_out/address/{date}/plots/access-trace"
        result_dir_mean_std = f"{username}/mgvm/analyze/_out/address/{date}/plots/mean-std-analysis"
        result_dir_shared_degree = f"{username}/mgvm/analyze/_out/address/{date}/plots/shared-degree-analysis"
        result_dir_pages = f"{username}/mgvm/analyze/_out/address/{date}/plots/access-distribution-pages"

        os.makedirs(result_dir_addr, exist_ok=True)
        os.makedirs(result_dir_temporal, exist_ok=True)
        os.makedirs(result_dir_temporal_per_chiplet, exist_ok=True)
        os.makedirs(result_dir_trace, exist_ok=True)
        os.makedirs(result_dir_mean_std, exist_ok=True)
        os.makedirs(result_dir_shared_degree, exist_ok=True)
        os.makedirs(result_dir_pages, exist_ok=True)

        # 2. 데이터 로드 및 전처리
        df = pd.read_csv(trace_file, skipinitialspace=True)
        df = df.drop_duplicates()

        df = df[df['rdma']==True]
        
        # 3. Plot 실행 및 저장
        print(f"[{benchmark}] - Generating Access Distribution Plot...")
        plot_access_count_per_addrs(df, benchmark, result_dir_addr, save_fig=True)

        print(f"[{benchmark}] - Generating Temporal Analysis Plot...")
        plot_temporal_access_and_footprint(df, benchmark, result_dir_temporal, save_fig=True)

        print(f"[{benchmark}] - Generating Temporal Analysis per Chiplet Plot...")
        plot_temporal_access_and_footprint_per_chiplet(
            df, 
            benchmark, 
            result_dir_temporal_per_chiplet, 
            page_level=True,
            save_fig=True)

        # print(f"[{benchmark}] - Generating Memory Access Trace Plot...")
        # # NOTE: agg_df가 코드에 정의되어 있지 않아 에러가 발생할 수 있습니다.
        # # 필요 시 외부 생성 혹은 해당 부분을 주석처리/수정해야 할 수 있습니다. (원본 코드 그대로 유지함)
        # plot_memory_access_trace(df, df, benchmark, result_dir_trace, save_fig=True) # df를 임시로 넘김. 원본: agg_df

        # print(f"[{benchmark}] - Generating Temporal Mean/Std Dev Analysis Plot...")
        # plot_temporal_mean_std_analysis(df, benchmark, result_dir_mean_std, save_fig=True)

        print(f"[{benchmark}] - Generating Access Count vs Shared Degree Plot...")
        plot_access_count_with_shared_degree(df, benchmark, result_dir_shared_degree, save_fig=True)

        print(f"[{benchmark}] - Generating Access Count per Page Plot...")
        plot_access_count_per_pages(df, benchmark, result_dir_pages, save_fig=True)

        print(f">>> Done: {benchmark}")
        return {'benchmark': benchmark, 'status': 'Success'}
    
    except Exception as e:
        print(f"[{benchmark}] Error: {e}")
        return {'benchmark': benchmark, 'status': f'Error: {e}'}


if __name__ == "__main__":

    import argparse as _ap
    _p = _ap.ArgumentParser(add_help=False)
    _p.add_argument('run_id', nargs='?', default='latest')
    _p.parse_known_args()

    # 병렬 처리 실행
    with ProcessPoolExecutor(max_workers=8) as executor:
        results = list(executor.map(process_benchmark, benchmarks))

    # # 결과를 DataFrame으로 변환 및 xlsx 에 저장
    # results_df = pd.DataFrame(results)
    
    # os.makedirs(f"{username}/mgvm/analyze/_out/address/{date}", exist_ok=True)
    # out_file = f"{username}/mgvm/analyze/_out/address/{date}/metrics_per_addr_summary.xlsx"
    # results_df.to_excel(out_file, index=False)
    
    # print("\n" + "="*80)
    # print("Execution Summary")
    # print("="*80)
    # print(results_df.to_string(index=False))
    # print("="*80)
    # print(f"Summary saved to: {out_file}")