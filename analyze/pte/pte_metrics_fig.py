import pandas as pd
import re
import os
import math

from params import benchmarks
from formatter import num_chiplets, ns, chip_cols
from pte_metrics_plotter import plot_pte_metrics
from _paths import pte_trace_dir


username = os.path.expanduser('~')

if __name__ == "__main__":
    date = "20260409-113720-mesh"
    trace_dir = pte_trace_dir(date)
    print(f"{trace_dir}")

    for benchmark in benchmarks:
        # if benchmark != "syr2k":
        #     continue

        trace_file = f"{trace_dir}/{benchmark}_mesh_6x6_pte_trace.csv"

        if not os.path.exists(trace_file):
            print(f"   Skip: No file {trace_file}")
            continue

        print(f">>> Processing Benchmark: {benchmark}")

        df = pd.read_csv(trace_file, skipinitialspace=True)
        df['pte'] = df['ppn'] + (df['level'] * 8)

        df = df.drop_duplicates()

        pte_metrics = df.groupby('pte').agg(
            access_count=('pte', 'size'),
            shared_chiplets=('chiplet', 'nunique'),
            level=('level', 'first'),
            owner=('owner', 'first'),
        ).rename(columns={'pte': 'count'}).reset_index()

        pte_metrics = pte_metrics.sort_values(by='pte', ascending=False)

        result_dir = f"{username}/mgvm/analyze/_out/pte/pte-metrics-fig"
        os.makedirs(result_dir, exist_ok=True)

        plot_pte_metrics(df=pte_metrics, benchmark=benchmark, result_dir=result_dir)