import os
import sys
import pandas as pd
import numpy as np

parent_dir = os.path.abspath(os.path.join(os.path.dirname(__file__), "../../"))
if parent_dir not in sys.path:
    sys.path.append(parent_dir)

scripts_dir = os.path.abspath(os.path.join(os.path.dirname(__file__), "../../../scripts"))
if scripts_dir not in sys.path:
    sys.path.append(scripts_dir)

from concurrent.futures import ProcessPoolExecutor
from itertools import repeat
from params import benchmarks

username = os.path.expanduser("~")

# run_id 결정: argv 첫 positional, 없으면 'latest'
_run_id = next((a for a in sys.argv[1:] if not a.startswith('-')), 'latest')
from _paths import run_root as _run_root, memory_trace_dir as _mtd, run_tag as _run_tag
date = _run_tag(_run_id)
trace_dir = str(_mtd(_run_id))

xDim = 6
yDim = 6
num_chiplets = xDim * yDim

_FILTER_CHOICES = ["all", "rdma-only", "local-only", "rdma-miss-only"]

def _apply_filter(df, f):
    if f == "rdma-only":
        return df[df["chiplet"] != df["owner"]]
    if f == "local-only":
        return df[df["chiplet"] == df["owner"]]
    if f == "rdma-miss-only":
        return df[(df["chiplet"] != df["owner"]) & (df["rdma"] == True)]
    return df


def process_benchmark(benchmark, args):
    trace_file = f"{trace_dir}/{benchmark}_mesh_{xDim}x{yDim}_memory_trace.csv"

    if not os.path.exists(trace_file):
        print(f"No {trace_file}, skip")
        return None

    print(f"Processing {benchmark}...")

    usecols = ["chiplet", "owner"]
    if args.filter == "rdma-miss-only":
        usecols += ["rdma"]
    df = pd.read_csv(trace_file, skipinitialspace=True)
    df = df.drop_duplicates()
    df = df[usecols]
    df = _apply_filter(df, args.filter)

    total_access_count = len(df)
    if total_access_count == 0:
        return None

    # local / remote 기준: owner == chiplet 이면 local, 아니면 remote
    local_access_count = (df["chiplet"] == df["owner"]).sum()
    remote_access_count = total_access_count - local_access_count

    return {
        "benchmark": benchmark,
        "filter": args.filter,
        "total_access_count": total_access_count,
        "local_access_count": int(local_access_count),
        "remote_access_count": int(remote_access_count),
        "local_ratio": local_access_count / total_access_count,
        "remote_ratio": remote_access_count / total_access_count,
    }


if __name__ == "__main__":
    import argparse
    parser = argparse.ArgumentParser(description="Local vs remote L2 access ratio.")
    parser.add_argument('run_id', nargs='?', default='latest')
    parser.add_argument('--filter', choices=_FILTER_CHOICES, default='all',
                        help='all: every L2 access; rdma-only: chiplet!=owner; local-only: chiplet==owner; rdma-miss-only: chiplet!=owner & rdma==True')
    args = parser.parse_args()

    print(f"Target Directory: {trace_dir}")

    results = []
    with ProcessPoolExecutor() as executor:
        for result in executor.map(process_benchmark, benchmarks, repeat(args)):
            if result is not None:
                results.append(result)

    results_df = pd.DataFrame(results).sort_values("benchmark")

    result_dir = f"{username}/mgvm/analyze/_out/cacheline/{date}"
    os.makedirs(result_dir, exist_ok=True)

    output_path = f"{result_dir}/local_remote_access_ratio_{args.filter}.xlsx"

    with pd.ExcelWriter(output_path, engine="openpyxl") as writer:
        results_df.to_excel(writer, sheet_name="summary", index=False)

    print("\n--- Analysis Summary ---")
    print(results_df)
    print(f"\nSummary saved to: {output_path}")