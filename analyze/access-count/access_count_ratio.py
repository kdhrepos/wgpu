import os
import sys
import pandas as pd
import numpy as np

parent_dir = os.path.abspath(os.path.join(os.path.dirname(__file__), "../../"))

# 검색 경로에 추가
if parent_dir not in sys.path:
    sys.path.append(parent_dir)

from concurrent.futures import ProcessPoolExecutor
from itertools import repeat
from params import benchmarks

username = os.path.expanduser('~')
# run_id 결정: argv 첫 positional, 없으면 'latest'
_run_id = next((a for a in sys.argv[1:] if not a.startswith('-')), 'latest')
from _paths import run_root as _run_root, memory_trace_dir as _mtd, run_tag as _run_tag
date = _run_tag(_run_id)
trace_dir = str(_mtd(_run_id))

xDim = 4
yDim = 4

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

    try:
        usecols = ['addr']
        if args.filter in ('rdma-only', 'local-only'):
            usecols += ['chiplet', 'owner']
        elif args.filter == 'rdma-miss-only':
            usecols += ['chiplet', 'owner', 'rdma']
        df = pd.read_csv(trace_file, skipinitialspace=True)
        df = df.drop_duplicates()
        df = df[usecols]
    except Exception as e:
        print(f"Error reading {benchmark}: {e}")
        return None

    df = _apply_filter(df, args.filter)

    if df.empty:
        return None

    # 주소별 접근 횟수 집계
    access_counts = df.groupby('addr').size().reset_index(name='count')

    # 2. 구간 설정 수정: 1회 접근을 독립 구간으로 설정
    bins = [1, 2, 4, 8, 16, 32, 64, 128, 256, np.inf]
    labels = ['1', '2-4', '4-8', '8-16', '16-32', '32-64', '64-128', '128-256', '256+']

    # 3. 구간 분류 및 비율 계산
    access_counts['range'] = pd.cut(access_counts['count'], bins=bins, labels=labels, right=False)
    
    # addr ratio (%)
    addr_ratio = access_counts.groupby('range', observed=False).size()
    addr_ratio = (addr_ratio / addr_ratio.sum()) * 100
    
    # access ratio (%)
    access_ratio = access_counts.groupby('range', observed=False)['count'].sum()
    access_ratio = (access_ratio / access_ratio.sum()) * 100

    # 결과 사전 생성
    res = {
        'benchmark': benchmark,
        'filter': args.filter,
        'total_accesses': len(df),
        'unique_cache_lines': len(access_counts)
    }
    
    for label in labels:
        res[f'addr_{label}'] = addr_ratio.get(label, 0)
    for label in labels:
        res[f'access_{label}'] = access_ratio.get(label, 0)

    return res

if __name__ == "__main__":
    import argparse
    parser = argparse.ArgumentParser(description="Access count distribution per cacheline.")
    parser.add_argument('run_id', nargs='?', default='latest')
    parser.add_argument('--filter', choices=_FILTER_CHOICES, default='rdma-miss-only',
                        help='all: every L2 access; rdma-only: chiplet!=owner; local-only: chiplet==owner; rdma-miss-only: chiplet!=owner & rdma==True')
    args = parser.parse_args()

    print(f"Target Directory: {trace_dir}")

    with ProcessPoolExecutor() as executor:
        results = list(executor.map(process_benchmark, benchmarks, repeat(args)))

    results = [r for r in results if r is not None]

    if results:
        results_df = pd.DataFrame(results)

        # 컬럼 순서 고정 (정보성 지표 + 설정한 라벨 순서)
        info_cols = ['benchmark', 'filter', 'total_accesses', 'unique_cache_lines']
        range_labels = ['1', '2-4', '4-8', '8-16', '16-32', '32-64', '64-128', '128-256', '256+']
        
        addr_cols = [f'addr_{label}' for label in range_labels]
        access_cols = [f'access_{label}' for label in range_labels]
        
        results_df = results_df[info_cols + addr_cols + access_cols]

        result_dir = f"{username}/mgvm/analyze/_out/access-count/{date}"
        os.makedirs(result_dir, exist_ok=True)
        
        output_path = f"{result_dir}/top_access_count_ratio_{args.filter}_{xDim}x{yDim}.xlsx"
        results_df.to_excel(output_path, index=False)
        
        print("\n--- Integrated Analysis Summary (Including Single-access) ---")
        print(results_df.to_string(index=False))
    else:
        print("No valid results found.")