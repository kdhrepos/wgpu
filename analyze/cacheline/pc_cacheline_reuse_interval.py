import os
import sys
import argparse
import numpy as np
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

CACHELINE_BYTES = 64
FREQ_HZ = 1_000_000_000  # 1 GHz -> time(s) * 1e9 = cycles


def _fmt(n):
    """Compact cycle-count label: 100 -> 100, 1000 -> 1K, 10000 -> 10K, 1_000_000 -> 1M."""
    if n >= 1_000_000 and n % 1_000_000 == 0:
        return f"{n // 1_000_000}M"
    if n >= 1_000 and n % 1_000 == 0:
        return f"{n // 1_000}K"
    return str(int(n))


def load_trace(trace_file, args):
    usecols = ['time', 'pc', 'addr', 'chiplet']
    if args.filter == 'l2miss':
        usecols += ['rdma', 'type']
    if args.exclude_ptw or args.only_ptw:
        usecols += ['is_ptw']

    df = pd.read_csv(trace_file, skipinitialspace=True)
    df = df.drop_duplicates()
    df = df[usecols]

    if args.filter == 'l2miss':
        df = df[(df['rdma'] == True) & (df['type'].isin([2, 5]))]
    if args.exclude_ptw:
        df = df[df['is_ptw'] == 0]
    if args.only_ptw:
        df = df[df['is_ptw'] == 1]

    return df


def process_benchmark(benchmark, args):
    trace_file = f"{trace_dir}/{benchmark}_mesh_{xDim}x{yDim}_memory_trace.csv"
    if not os.path.exists(trace_file):
        return None

    print(f"[{benchmark}] Computing reuse intervals (filter={args.filter}, exclude_ptw={args.exclude_ptw}, only_ptw={args.only_ptw})...")

    try:
        df = load_trace(trace_file, args)
        if df.empty:
            return None

        df['cycle'] = (df['time'] * FREQ_HZ).astype(np.int64)
        df['cacheline'] = df['addr'] // CACHELINE_BYTES

        # Group by (pc, chiplet, cacheline); consecutive accesses within a group give a reuse interval.
        df = df.sort_values(['pc', 'chiplet', 'cacheline', 'cycle'])
        df['prev_cycle'] = df.groupby(['pc', 'chiplet', 'cacheline'])['cycle'].shift(1)
        reuse = df.dropna(subset=['prev_cycle']).copy()
        if reuse.empty:
            return None
        reuse['interval'] = (reuse['cycle'] - reuse['prev_cycle']).astype(np.int64)

        # Bucket reuse intervals per PC and report the % of samples in each bucket.
        edges = args.bucket_edges  # e.g. [100, 1000, 10000]
        labels = []
        prev = None
        for e in edges:
            labels.append(f"<{_fmt(e)}" if prev is None else f"{_fmt(prev)}to{_fmt(e)}")
            prev = e
        labels.append(f">={_fmt(prev)}")

        bins = [-np.inf] + list(edges) + [np.inf]
        reuse['bucket'] = pd.cut(reuse['interval'], bins=bins, labels=labels, right=False)

        counts = (reuse.groupby(['pc', 'bucket'], observed=False)
                       .size()
                       .unstack(fill_value=0))
        # ensure deterministic column order
        counts = counts.reindex(columns=labels, fill_value=0)

        totals = counts.sum(axis=1)
        pct = counts.div(totals, axis=0).mul(100).round(2)
        pct.columns = [f"interval_{c}_cycles_%" for c in pct.columns]

        out = pct.reset_index()
        out.insert(1, 'reuse_sample_count', totals.values)
        out.insert(0, 'benchmark', benchmark)

        out = out.sort_values('reuse_sample_count', ascending=False)
        return out

    except Exception as e:
        print(f"[{benchmark}] Error: {e}")
        return None


if __name__ == "__main__":
    parser = argparse.ArgumentParser(
        description="Average reuse interval (in cycles) of cache lines per PC, "
                    "grouped by (pc, chiplet, cacheline)."
    )
    parser.add_argument('run_id', nargs='?', default='latest',
                        help="run-id (latest, <config>, <config>/<timestamp>, ...)")
    parser.add_argument('--filter', choices=['all', 'l2miss'], default='all',
                        help="all: every access; l2miss: rdma==True & type in {2,5}")
    ptw_group = parser.add_mutually_exclusive_group()
    ptw_group.add_argument('--exclude-ptw', action='store_true',
                           help="exclude is_ptw==1 rows (page-table walks); keep data only")
    ptw_group.add_argument('--only-ptw', action='store_true',
                           help="include only is_ptw==1 rows (page-table walks); exclude data")
    parser.add_argument('--bucket-edges', type=lambda s: [int(x) for x in s.split(',')],
                        default=[100, 1000, 10000],
                        help="comma-separated ascending cycle edges. default: 100,1000,10000 "
                             "-> buckets <100 / 100to1K / 1Kto10K / >=10K")
    args = parser.parse_args()
    if sorted(args.bucket_edges) != args.bucket_edges or len(set(args.bucket_edges)) != len(args.bucket_edges):
        parser.error("--bucket-edges must be strictly ascending unique ints")

    with ProcessPoolExecutor() as executor:
        results = list(executor.map(process_benchmark, benchmarks, repeat(args)))

    results = [r for r in results if r is not None]
    if not results:
        print("No results generated.")
        sys.exit(0)

    final_df = pd.concat(results, ignore_index=True)

    result_dir = f"{username}/mgvm/analyze/_out/cacheline/{date}"
    os.makedirs(result_dir, exist_ok=True)

    suffix_parts = [args.filter]
    if args.exclude_ptw:
        suffix_parts.append('no-ptw')
    if args.only_ptw:
        suffix_parts.append('only-ptw')
    suffix = "_" + "_".join(suffix_parts)
    output_path = f"{result_dir}/t_pc_cacheline_reuse_interval{suffix}.xlsx"

    final_df.to_excel(output_path, index=False)

    print("\n" + "=" * 80)
    print(f"{'Per-PC Cacheline Reuse Interval (cycles)':^80}")
    print("=" * 80)
    print(f"Total rows: {len(final_df)}")
    print(f"Saved to:   {output_path}")
    print("=" * 80)
    print(final_df.head(15).to_string(index=False))
