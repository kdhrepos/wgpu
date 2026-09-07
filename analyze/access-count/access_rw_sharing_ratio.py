import os
import sys
import pandas as pd

parent_dir = os.path.abspath(os.path.join(os.path.dirname(__file__), "../../"))

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
        return None

    print(f"[{benchmark}] Analyzing Access Count Ratio for RO/RW Shared Cachelines...")

    try:
        usecols = ['addr', 'chiplet', 'rw']
        if args.filter in ('rdma-only', 'local-only'):
            usecols += ['owner']
        elif args.filter == 'rdma-miss-only':
            usecols += ['owner', 'rdma']
        df = pd.read_csv(trace_file, skipinitialspace=True)
        df = df.drop_duplicates()
        df = df[usecols]
        df = _apply_filter(df, args.filter)

        if args.group_size != 0:
            group_size = args.group_size
            df['addr'] = df['addr'] // (64 * group_size)
        
        if df.empty:
            return None

        # Group by addr
        # count unique chiplets to determine if shared (>1)
        # count 'chiplet' to get total accesses for this cache line
        # sum 'rw' to check if any writes exist (rw=1 is write, rw=0 is read)
        addr_stats = df.groupby('addr').agg(
            access_count=('chiplet', 'count'),
            unique_chiplets=('chiplet', 'nunique'),
            write_count=('rw', 'sum')
        ).reset_index()

        total_accesses = addr_stats['access_count'].sum()
        if total_accesses == 0:
            return None

        # Classify each cache line
        addr_stats['is_shared'] = addr_stats['unique_chiplets'] > 1
        addr_stats['is_readonly'] = addr_stats['write_count'] == 0

        # Sum access counts for the 4 Combinations
        shared_ro_access = addr_stats.loc[(addr_stats['is_shared'] == True) & (addr_stats['is_readonly'] > 0), 'access_count'].sum()
        shared_rw_access = addr_stats.loc[(addr_stats['is_shared'] == True) & (addr_stats['is_readonly'] == 0), 'access_count'].sum()
        private_ro_access = addr_stats.loc[(addr_stats['is_shared'] == False) & (addr_stats['is_readonly'] > 0), 'access_count'].sum()
        private_rw_access = addr_stats.loc[(addr_stats['is_shared'] == False) & (addr_stats['is_readonly'] == 0), 'access_count'].sum()

        res_dict = {
            'benchmark': benchmark,
            'total_access_count': total_accesses,
            'shared_ro_access_count': shared_ro_access,
            'shared_rw_access_count': shared_rw_access,
            'private_ro_access_count': private_ro_access,
            'private_rw_access_count': private_rw_access,
            'shared_ro_access_ratio_%': round(shared_ro_access / total_accesses * 100, 2),
            'shared_rw_access_ratio_%': round(shared_rw_access / total_accesses * 100, 2),
            'private_ro_access_ratio_%': round(private_ro_access / total_accesses * 100, 2),
            'private_rw_access_ratio_%': round(private_rw_access / total_accesses * 100, 2)
        }

        return pd.DataFrame([res_dict])

    except Exception as e:
        print(f"[{benchmark}] Error: {e}")
        return None

if __name__ == "__main__":
    import argparse
    parser = argparse.ArgumentParser(description="Analyze Access Count Ratio for RO/RW Shared Cachelines.")
    parser.add_argument('run_id', nargs='?', default='latest',
                        help="run-id (latest, <config>, <config>/<timestamp>, ...)")
    parser.add_argument('--group-size', type=int, default=1, help='Group size for address grouping (0 means no grouping)')
    parser.add_argument('--filter', choices=_FILTER_CHOICES, default='all',
                        help='all: every L2 access; rdma-only: chiplet!=owner; local-only: chiplet==owner; rdma-miss-only: chiplet!=owner & rdma==True')
    args = parser.parse_args()

    with ProcessPoolExecutor() as executor:
        results = list(executor.map(process_benchmark, benchmarks, repeat(args)))

    results = [r for r in results if r is not None]

    if results:
        final_df = pd.concat(results, ignore_index=True)

        result_dir = f"{username}/mgvm/analyze/_out/access-count/{date}"
        os.makedirs(result_dir, exist_ok=True)

        suffix = f"_{args.filter}_{args.group_size}"
        output_path = f"{result_dir}/t_access_rw_sharing_ratio{suffix}.xlsx"

        final_df.to_excel(output_path, index=False)
        
        print("\n" + "="*80)
        print(f"{'Access Count Ratio for Read-Only / Read-Write Shared Cachelines':^80}")
        print("="*80)
        print(final_df.to_string(index=False))
        print("="*80)
        print(f"Saved results to:\n- {output_path}")
    else:
        print("No results generated.")
