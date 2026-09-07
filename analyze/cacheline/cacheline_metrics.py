import pandas as pd
import re
import matplotlib.pyplot as plt
import matplotlib.ticker as mtick
import os
import numpy as np
import seaborn as sns

from params import benchmarks
from formatter import num_chiplets, ns, chip_cols
from cacheline_metrics_plotter \
    import plot_reuse_interval, \
        plot_stack_distance
from _paths import memory_trace_dir


username = os.path.expanduser('~')

if __name__ == "__main__":
    date = "20260409-113720-mesh"
    trace_dir = memory_trace_dir(date)
    print(f"{trace_dir}")

    for benchmark in benchmarks:
        # if benchmark != "syr2k":
            # continue

        print(f">>> Processing Benchmark: {benchmark}")

        trace_file = f"{trace_dir}/{benchmark}_mesh_6x6_memory_trace.csv"

        if not os.path.exists(trace_file):
            print(f"   Skip: No file {trace_file}")
            continue

        # 2. 데이터 로드 및 전처리
        df = pd.read_csv(trace_file, skipinitialspace=True)
        df = df.drop_duplicates()
        df['cycle'] = (df['time'] * (1.0 * 1e9)).astype(int)

        result_dir = f"{username}/mgvm/analyze/_out/cacheline/cacheline-metrics"
        reuse_interval_dir = f"{result_dir}/reuse_interval"
        stack_distance_dir = f"{result_dir}/stack_distance"

        os.makedirs(result_dir, exist_ok=True)
        os.makedirs(reuse_interval_dir, exist_ok=True)
        os.makedirs(stack_distance_dir, exist_ok=True)
        
        # print(f" >>> Plotting Reuse Interval for {benchmark}...")
        # plot_reuse_interval(df, benchmark, reuse_interval_dir)

        print(f" >>> Plotting Stack Distance for {benchmark}...")
        plot_stack_distance(df, benchmark, stack_distance_dir)

        print(f">>> Done: {benchmark}")