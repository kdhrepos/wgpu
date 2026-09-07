import os
import sys
import pandas as pd

parent_dir = os.path.abspath(os.path.join(os.path.dirname(__file__), "../../"))
if parent_dir not in sys.path:
    sys.path.append(parent_dir)

scripts_dir = os.path.abspath(os.path.join(os.path.dirname(__file__), "../../../scripts"))
if scripts_dir not in sys.path:
    sys.path.append(scripts_dir)

from concurrent.futures import ProcessPoolExecutor
from itertools import repeat
from params import benchmarks

username = os.path.expanduser('~')

# --- run_id 결정: argv 첫 positional, 없으면 'latest' (Linux fork 전에 trace_dir 픽스) ---
_run_id = next((a for a in sys.argv[1:] if not a.startswith('-')), 'latest')
from _paths import run_root as _run_root, memory_trace_dir as _mtd, run_tag as _run_tag
date = _run_tag(_run_id)
trace_dir = str(_mtd(_run_id))
def process_benchmark(benchmark, args):
    trace_file = f"{trace_dir}/{benchmark}_mesh_{xDim}x{yDim}_memory_trace.csv"

    if not os.path.exists(trace_file):
        return None

    group_col = args.group_by
    print(f"[{benchmark}] Analyzing PC stats for RDMA per {group_col}...")

    try:
        df = pd.read_csv(trace_file, skipinitialspace=True)
        df = df.drop_duplicates()
        df = df[['pc', 'addr', 'chiplet', 'rdma', 'owner', 'rw', 'type']]

        df = df[(df['rdma'] == True)].copy()

        if df.empty:
            return None

        if args.group_size != 0:
            group_size = args.group_size
            df['addr'] = df['addr'] // (64 * group_size)

        src_x, src_y = df['chiplet'] % xDim, df['chiplet'] // xDim
        dst_x, dst_y = df['owner']   % xDim, df['owner']   // xDim
        df['hop_count'] = (src_x - dst_x).abs() + (src_y - dst_y).abs()

        addr_chiplets = df.groupby(['pc', 'addr'])['chiplet'].nunique().reset_index(name='chiplet_count')
        avg_chiplets_per_addr = addr_chiplets.groupby('pc')['chiplet_count'].mean().reset_index(name='avg_requestor_chiplets')

        # group_col(owner=홈 노드 / chiplet=요청자)와 'pc'별로 그룹화하여 통계 추출
        stats = df.groupby([group_col, 'pc']).agg(
            access_count=('addr', 'count'),
            unique_addrs=('addr', 'nunique'),
            read_count=('rw', lambda x: (x == 0).sum()),
            write_count=('rw', lambda x: (x == 1).sum()),
            avg_hop=('hop_count', 'mean'),
        ).reset_index()
        stats['access_per_addr'] = stats['access_count'] / stats['unique_addrs']

        pc_agg = stats.groupby('pc').agg(
            avg_unique_addrs=('unique_addrs', 'mean'),
            avg_read_count=('read_count', 'mean'),
            avg_write_count=('write_count', 'mean'),
            avg_access_per_addr=('access_per_addr', 'mean'),
            avg_hop_count=('avg_hop', 'mean'),
            total_access_count=('access_count', 'sum')
        ).reset_index()

        pc_agg = pd.merge(pc_agg, avg_chiplets_per_addr, on='pc')

        total_unique_addrs_per_pc = df.groupby('pc')['addr'].nunique().reset_index(name='total_unique_addrs')
        pc_agg = pd.merge(pc_agg, total_unique_addrs_per_pc, on='pc')

        rw_sum = pc_agg['avg_read_count'] + pc_agg['avg_write_count']
        pc_agg['read_ratio_%']  = (pc_agg['avg_read_count']  / rw_sum * 100).round(2).fillna(0)
        pc_agg['write_ratio_%'] = (pc_agg['avg_write_count'] / rw_sum * 100).round(2).fillna(0)
        pc_agg['avg_access_per_addr'] = pc_agg['avg_access_per_addr'].round(2)
        pc_agg['avg_hop_count'] = pc_agg['avg_hop_count'].round(2)

        pc_agg = pc_agg.rename(columns={
            'avg_unique_addrs':    f'avg_unique_addrs_per_{group_col}',
            'avg_access_per_addr': f'avg_access_per_addr_per_{group_col}',
            'avg_hop_count':       f'avg_hop_count_per_{group_col}',
        })

        pc_agg.insert(0, 'benchmark', benchmark)

        column_order = [
            'benchmark', 'pc',
            'total_access_count', 'total_unique_addrs',
            f'avg_unique_addrs_per_{group_col}',
            f'avg_access_per_addr_per_{group_col}',
            'avg_requestor_chiplets', f'avg_hop_count_per_{group_col}',
            'read_ratio_%', 'write_ratio_%',
        ]
        pc_agg = pc_agg[column_order]

        pc_agg = pc_agg.sort_values(by='total_access_count', ascending=False)

        return pc_agg

    except Exception as e:
        print(f"[{benchmark}] Error: {e}")
        return None


if __name__ == "__main__":
    import argparse
    parser = argparse.ArgumentParser(description="Analyze PC stats for RDMA per Owner or Chiplet.")
    parser.add_argument('run_id', nargs='?', default='latest',
                        help="run-id (latest, <config>, <config>/<timestamp>, ...)")
    parser.add_argument('--group-size', type=int, default=1, help='Group size for address grouping')
    parser.add_argument('--group-by', choices=['owner', 'chiplet'], default='owner',
                        help="grouping key for per-PC aggregation: 'owner' (data home node, default) or 'chiplet' (requester)")
    args = parser.parse_args()

    with ProcessPoolExecutor() as executor:
        results = list(executor.map(process_benchmark, benchmarks, repeat(args)))

    results = [r for r in results if r is not None]

    if results:
        final_df = pd.concat(results, ignore_index=True)

        result_dir = f"{username}/mgvm/analyze/_out/pc-stats/{date}"
        os.makedirs(result_dir, exist_ok=True)

        suffix = f"_{args.group_size}" if args.group_size != 0 else ""
        output_path = f"{result_dir}/t_pc_stats_rdma_per_{args.group_by}{suffix}.xlsx"

        final_df.to_excel(output_path, index=False)

        print("\n" + "="*80)
        print(f"{f'PC Stats for RDMA (Remote) per {args.group_by}':^80}")
        print("="*80)
        print(f"Total Rows: {len(final_df)}")
        print(f"Saved results to: {output_path}")
        print("="*80)
        print(final_df.head(15).to_string(index=False))
    else:
        print("No results generated.")