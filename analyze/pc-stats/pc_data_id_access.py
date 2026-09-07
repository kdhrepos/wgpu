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
xDim = 6
yDim = 6

L2_MISS_TYPES = {2, 5}


def process_benchmark(benchmark, args):
    trace_file = f"{trace_dir}/{benchmark}_mesh_{xDim}x{yDim}_memory_trace.csv"

    if not os.path.exists(trace_file):
        return None

    print(f"[{benchmark}] Analyzing per-PC per-data_id access counts...")

    try:
        df = pd.read_csv(
            trace_file,
            skipinitialspace=True,
        )
        df = df.drop_duplicates()
        df = df[['pc', 'addr', 'chiplet', 'rw', 'rdma', 'type', 'data_id', 'is_ptw']]

        if not args.include_ptw:
            df = df[df['is_ptw'].astype(bool) == False]
        if not args.include_unmatched:
            df = df[df['data_id'] != -1]
        if args.rdma_l2miss_only:
            df = df[(df['rdma'] == True) & (df['type'].isin(L2_MISS_TYPES))]
        elif args.rdma_only:
            df = df[df['rdma'] == True]

        if df.empty:
            return None

        if args.group_size != 0:
            df['addr'] = df['addr'] // (64 * args.group_size)

        # (pc, data_id) 별 통계
        stats = df.groupby(['pc', 'data_id']).agg(
            access_count=('addr', 'count'),
            unique_addrs=('addr', 'nunique'),
            read_count=('rw', lambda x: (x == 0).sum()),
            write_count=('rw', lambda x: (x == 1).sum()),
        ).reset_index()

        # 캐시 라인 공유 정도: (pc, data_id, addr) 마다 distinct chiplet 수를 센 뒤
        # (pc, data_id) 단위로 집계 (참고: pc_stats_all_per_owner.py 의 avg_requestor_chiplets)
        line_chiplets = (
            df.groupby(['pc', 'data_id', 'addr'])['chiplet']
              .nunique()
              .reset_index(name='chiplet_count')
        )
        sharing = line_chiplets.groupby(['pc', 'data_id']).agg(
            avg_chiplets_per_cl=('chiplet_count', 'mean'),
            max_chiplets_per_cl=('chiplet_count', 'max'),
            shared_cl_count=('chiplet_count', lambda x: (x > 1).sum()),
            cl_count=('chiplet_count', 'size'),
        ).reset_index()
        sharing['shared_cl_ratio_%'] = (sharing['shared_cl_count'] / sharing['cl_count'] * 100).round(2)
        sharing['avg_chiplets_per_cl'] = sharing['avg_chiplets_per_cl'].round(2)
        sharing = sharing.drop(columns=['cl_count'])  # unique_addrs 와 동일

        stats = stats.merge(sharing, on=['pc', 'data_id'])

        # PC 전체 접근수 대비 비율 (해당 PC가 이 data_id 에 얼마나 집중적으로 접근했는지)
        pc_totals = stats.groupby('pc')['access_count'].sum().rename('pc_total_access')
        stats = stats.merge(pc_totals, on='pc')
        stats['pc_access_ratio_%'] = (stats['access_count'] / stats['pc_total_access'] * 100).round(2)

        # data_id 전체 접근수 대비 비율 (이 data_id 에 대해 해당 PC 가 차지하는 비중)
        did_totals = stats.groupby('data_id')['access_count'].sum().rename('data_id_total_access')
        stats = stats.merge(did_totals, on='data_id')
        stats['data_id_access_ratio_%'] = (stats['access_count'] / stats['data_id_total_access'] * 100).round(2)

        stats['access_per_addr'] = (stats['access_count'] / stats['unique_addrs']).round(2)

        stats.insert(0, 'benchmark', benchmark)

        column_order = [
            'benchmark', 'pc', 'data_id',
            'access_count', 'unique_addrs', 'access_per_addr',
            'read_count', 'write_count',
            'avg_chiplets_per_cl', 'max_chiplets_per_cl',
            'shared_cl_count', 'shared_cl_ratio_%',
            'pc_total_access', 'pc_access_ratio_%',
            'data_id_total_access', 'data_id_access_ratio_%',
        ]
        stats = stats[column_order]

        # 정렬: PC 전체 접근수 내림차순 → 동일 PC 안에서 access_count 내림차순
        stats = stats.sort_values(
            by=['pc_total_access', 'pc', 'access_count'],
            ascending=[False, True, False],
        )

        return stats

    except Exception as e:
        print(f"[{benchmark}] Error: {e}")
        return None


if __name__ == "__main__":
    import argparse
    parser = argparse.ArgumentParser(description="Per-PC per-data_id access counts.")
    parser.add_argument('run_id', nargs='?', default='latest',
                        help="run-id (latest, <config>, <config>/<timestamp>, ...)")
    parser.add_argument('--group-size', type=int, default=1,
                        help='Cache-line group size (addr // (64 * group_size)); 0 = raw addr')
    parser.add_argument('--rdma-only', action='store_true',
                        help='count only RDMA accesses (rdma == 1)')
    parser.add_argument('--rdma-l2miss-only', action='store_true',
                        help='count only RDMA-served L2 misses (rdma == 1 AND type in {2,5})')
    parser.add_argument('--include-unmatched', action='store_true',
                        help='include data_id == -1 rows (default: drop)')
    parser.add_argument('--include-ptw', action='store_true',
                        help='include is_ptw == 1 rows (default: drop)')
    args = parser.parse_args()

    if args.rdma_only and args.rdma_l2miss_only:
        parser.error("--rdma-only and --rdma-l2miss-only are mutually exclusive")

    with ProcessPoolExecutor() as executor:
        results = list(executor.map(process_benchmark, benchmarks, repeat(args)))

    results = [r for r in results if r is not None]

    if results:
        final_df = pd.concat(results, ignore_index=True)

        result_dir = f"{username}/mgvm/analyze/_out/pc-stats/{date}"
        os.makedirs(result_dir, exist_ok=True)

        tag_parts = []
        if args.group_size != 1:
            tag_parts.append(f"g{args.group_size}")
        if args.rdma_l2miss_only:
            tag_parts.append("rdma-l2miss-only")
        elif args.rdma_only:
            tag_parts.append("rdma-only")
        if args.include_unmatched:
            tag_parts.append("with-unmatched")
        if args.include_ptw:
            tag_parts.append("with-ptw")
        suffix = ("_" + "_".join(tag_parts)) if tag_parts else ""

        output_path = f"{result_dir}/t_pc_data_id_access{suffix}.xlsx"
        final_df.to_excel(output_path, index=False)

        print("\n" + "=" * 80)
        print(f"{'Per-PC Per-data_id Access Counts':^80}")
        print("=" * 80)
        print(f"Total Rows: {len(final_df)}")
        print(f"Saved results to: {output_path}")
        print("=" * 80)
        print(final_df.head(15).to_string(index=False))
    else:
        print("No results generated.")
