"""Per-benchmark: among the top-N% hottest cachelines (by demand access count),
what fraction was evicted-or-overwritten at least once?

  ratio = |{ cacheline in top-N% : (evict+overwrite)_count > 0 }| / |{ cacheline in top-N% }|

Memory-trace format (util/tracing/memoryaccesstracer.go):
  time,addr,pc,owner,rw,rdma,vector,type,chiplet,data_id,is_ptw
  type == 6 -> evict, type == 7 -> overwrite, else demand access.
  Both 6 and 7 count as eviction events here.
"""

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
EVICT_TYPE = 8
OVERWRITE_TYPE = 9


FILTER_CHOICES = ('all', 'data', 'pte')


def _apply_filter(df, mode):
    if mode == 'all':
        return df
    if mode == 'data':
        return df[df['is_ptw'] == 0]
    if mode == 'pte':
        return df[df['is_ptw'] == 1]
    raise ValueError(mode)


def _compute_row(df, mode, top_pcts):
    sub = _apply_filter(df, mode)
    if sub.empty:
        return None

    is_evict = sub['type'].isin([EVICT_TYPE, OVERWRITE_TYPE])
    is_access = ~is_evict

    access_counts = sub.loc[is_access, 'cacheline'].value_counts()
    if access_counts.empty:
        return None

    evicted_lines = set(sub.loc[is_evict, 'cacheline'].unique())

    access_sorted = access_counts.sort_values(ascending=False)
    total_lines = len(access_sorted)

    row = {
        'filter': mode,
        'unique_accessed_cachelines': int(total_lines),
        'unique_evicted_cachelines':  int(len(evicted_lines)),
    }
    for pct in top_pcts:
        k = max(1, int(np.ceil(total_lines * pct / 100)))
        top_lines = access_sorted.iloc[:k].index
        evicted_in_top = sum(1 for cl in top_lines if cl in evicted_lines)
        ratio = evicted_in_top / k if k else float('nan')

        tag = f"top{pct}%"
        row[f"{tag}_lines"]         = int(k)
        row[f"{tag}_evicted_lines"] = int(evicted_in_top)
        row[f"{tag}_evicted_ratio"] = round(ratio, 4)
    return row


def process_benchmark(benchmark, args):
    trace_file = f"{trace_dir}/{benchmark}_mesh_{xDim}x{yDim}_memory_trace.csv"
    if not os.path.exists(trace_file):
        return None

    print(f"[{benchmark}] hot-cacheline evict ratio ({','.join(args.filters)})...")

    try:
        df = pd.read_csv(trace_file, skipinitialspace=True)
        df = df.drop_duplicates()
        df = df[['addr', 'type', 'is_ptw']]
        if df.empty:
            return None
        df['cacheline'] = df['addr'] // CACHELINE_BYTES

        rows = []
        for mode in args.filters:
            r = _compute_row(df, mode, args.top_pcts)
            if r is None:
                continue
            r = {'benchmark': benchmark, **r}
            rows.append(r)
        if not rows:
            return None
        return pd.DataFrame(rows)

    except Exception as e:
        print(f"[{benchmark}] Error: {e}")
        return None


if __name__ == "__main__":
    parser = argparse.ArgumentParser(
        description="Fraction of top-N% hottest cachelines that were evicted at least once."
    )
    parser.add_argument('run_id', nargs='?', default='latest',
                        help="run-id (latest, <config>, <config>/<timestamp>, ...)")
    parser.add_argument('--top-pcts', type=lambda s: [float(x) for x in s.split(',')],
                        default=[1, 5, 10, 20, 50, 100],
                        help="comma-separated percentile cutoffs. default: 1,5,10,20,50,100")
    parser.add_argument('--filter', dest='filters',
                        type=lambda s: [x.strip() for x in s.split(',')],
                        default=list(FILTER_CHOICES),
                        help=f"comma-separated subset of {FILTER_CHOICES}. default: all,data,pte")
    args = parser.parse_args()

    for p in args.top_pcts:
        if not (0 < p <= 100):
            parser.error(f"--top-pcts entries must be in (0, 100]; got {p}")
    for f in args.filters:
        if f not in FILTER_CHOICES:
            parser.error(f"--filter must be subset of {FILTER_CHOICES}; got {f}")

    with ProcessPoolExecutor() as executor:
        results = list(executor.map(process_benchmark, benchmarks, repeat(args)))

    results = [r for r in results if r is not None]
    if not results:
        print("No results generated.")
        sys.exit(0)

    final_df = pd.concat(results, ignore_index=True)

    result_dir = f"{username}/mgvm/analyze/_out/top-n/{date}"
    os.makedirs(result_dir, exist_ok=True)
    output_path = f"{result_dir}/t_top_hot_evicted_ratio.xlsx"
    final_df.to_excel(output_path, index=False)

    print("\n" + "=" * 80)
    print(f"{'Top-N% hot cacheline evicted ratio':^80}")
    print("=" * 80)
    print(final_df.to_string(index=False))
    print(f"\nSaved to: {output_path}")
