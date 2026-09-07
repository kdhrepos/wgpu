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

# run_id 결정: argv 첫 positional, 없으면 'latest'
_run_id = next((a for a in sys.argv[1:] if not a.startswith('-')), 'latest')
from _paths import run_root as _run_root, memory_trace_dir as _mtd, run_tag as _run_tag
date = _run_tag(_run_id)
trace_dir = str(_mtd(_run_id))

xDim = 6
yDim = 6
N_CHIPLETS = xDim * yDim

L2_MISS_TYPES = {2, 5}


def process_benchmark(benchmark, args):
    trace_file = f"{trace_dir}/{benchmark}_mesh_{xDim}x{yDim}_memory_trace.csv"

    if not os.path.exists(trace_file):
        return None

    print(f"[{benchmark}] Analyzing per-data_id owner distribution...")

    try:
        df = pd.read_csv(
            trace_file,
            skipinitialspace=True,
        )
        df = df.drop_duplicates()
        df = df[['addr', 'owner', 'rdma', 'type', 'data_id', 'is_ptw']]

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

        # (data_id, owner) 별 접근 횟수와 unique cacheline 수
        grp = df.groupby(['data_id', 'owner']).agg(
            access_count=('addr', 'count'),
            unique_addrs=('addr', 'nunique'),
        ).reset_index()

        # data_id 합계
        did_totals = grp.groupby('data_id').agg(
            total_access=('access_count', 'sum'),
            total_unique_addrs=('unique_addrs', 'sum'),
        ).reset_index()

        # access ratio (%) — owner 별로 해당 data_id 의 access 중 차지하는 비율
        grp = grp.merge(did_totals, on='data_id')
        grp['access_ratio_%'] = (grp['access_count'] / grp['total_access'] * 100).round(2)
        grp['addr_ratio_%'] = (grp['unique_addrs'] / grp['total_unique_addrs'] * 100).round(2)

        # wide format pivot: 행=data_id, 열=chiplet 0..N-1, 값=access_ratio_%
        wide_acc = (
            grp.pivot(index='data_id', columns='owner', values='access_ratio_%')
               .reindex(columns=range(N_CHIPLETS), fill_value=0.0)
               .fillna(0.0)
               .round(2)
        )
        wide_acc.columns = [f'chiplet_{c}_access_%' for c in wide_acc.columns]
        wide_acc = wide_acc.reset_index()

        # unique cacheline 기준 분포도 같이 — 데이터 배치 자체의 owner 분포
        wide_addr = (
            grp.pivot(index='data_id', columns='owner', values='addr_ratio_%')
               .reindex(columns=range(N_CHIPLETS), fill_value=0.0)
               .fillna(0.0)
               .round(2)
        )
        wide_addr.columns = [f'chiplet_{c}_addr_%' for c in wide_addr.columns]
        wide_addr = wide_addr.reset_index()

        out = did_totals.merge(wide_acc, on='data_id').merge(wide_addr, on='data_id')
        out.insert(0, 'benchmark', benchmark)

        # owner 분산도 요약: max / nonzero count (얼마나 한 chiplet 에 쏠리는지)
        ratio_cols = [c for c in out.columns if c.endswith('_access_%')]
        ratios = out[ratio_cols]
        out.insert(3, 'max_owner_access_%', ratios.max(axis=1).round(2))
        out.insert(4, 'nonzero_owner_count', (ratios > 0).sum(axis=1))

        out = out.sort_values('total_access', ascending=False)
        return out

    except Exception as e:
        print(f"[{benchmark}] Error: {e}")
        return None


if __name__ == "__main__":
    import argparse
    parser = argparse.ArgumentParser(description="Per-data_id owner distribution (access % across chiplets).")
    parser.add_argument('run_id', nargs='?', default='latest',
                        help="run-id (latest, <config>, <config>/<timestamp>, ...)")
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

        result_dir = f"{username}/mgvm/analyze/_out/data-structure/{date}"
        os.makedirs(result_dir, exist_ok=True)

        tag_parts = []
        if args.rdma_l2miss_only:
            tag_parts.append("rdma-l2miss-only")
        elif args.rdma_only:
            tag_parts.append("rdma-only")
        if args.include_unmatched:
            tag_parts.append("with-unmatched")
        if args.include_ptw:
            tag_parts.append("with-ptw")
        suffix = ("_" + "_".join(tag_parts)) if tag_parts else ""

        output_path = f"{result_dir}/t_data_id_owner_distribution{suffix}.xlsx"
        final_df.to_excel(output_path, index=False)

        print("\n" + "=" * 80)
        print(f"{'Per-data_id Owner Distribution':^80}")
        print("=" * 80)
        print(f"Total Rows: {len(final_df)}")
        print(f"Saved results to: {output_path}")
        print("=" * 80)
        preview_cols = ['benchmark', 'data_id', 'total_access',
                        'max_owner_access_%', 'nonzero_owner_count']
        print(final_df[preview_cols].head(15).to_string(index=False))
    else:
        print("No results generated.")
