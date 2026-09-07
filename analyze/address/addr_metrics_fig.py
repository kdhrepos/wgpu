import pandas as pd
import re
import os
import math

from params import benchmarks
from formatter import num_chiplets, ns, chip_cols
from addr_metrics_plotter import plot_addr_metrics
from _paths import memory_trace_dir

username = os.path.expanduser('~')

if __name__ == "__main__":
    date = "20260409-113720-mesh"
    trace_dir = memory_trace_dir(date)
    print(f"{trace_dir}")

    for benchmark in benchmarks:
        if benchmark != "bfs":
            continue

        trace_file = f"{trace_dir}/{benchmark}_mesh_6x6_memory_trace.csv"

        if not os.path.exists(trace_file):
            print(f"   Skip: No file {trace_file}")
            continue

        print(f">>> Processing Benchmark: {benchmark}")

        df = pd.read_csv(trace_file, skipinitialspace=True)
        df = df.drop_duplicates()

        addr_metrics = df.groupby('addr').agg(
            access_count= ('addr', 'size'),
            shared_chiplets= ('chiplet', 'nunique'),
        ).rename(columns={'addr': 'count'}).reset_index()

        addr_metrics = addr_metrics.sort_values(by='addr', ascending=False)

        result_dir = f"{username}/mgvm/analyze/_out/address/addr-metrics-fig"
        os.makedirs(result_dir, exist_ok=True)

        plot_addr_metrics(df=addr_metrics, benchmark=benchmark, result_dir=result_dir)