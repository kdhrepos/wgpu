"""Quantify how much of cache eviction/overwrite activity is concentrated on
'hot' cachelines (the most-accessed ones).

Per benchmark, for each top-X% percentile of cachelines (ranked by demand
access count, desc):
  - access_share_%  : share of total demand accesses landing on top-X%
  - evict_share_%   : share of total evictions  (type == 6) landing on top-X%
  - overwrite_share_%: share of total overwrites (type == 7) landing on top-X%
  - evict_lift      : evict_share / access_share
                       ~1  -> evictions track demand proportionally
                       <1  -> hot data is preserved (good)
                       >1  -> hot data is evicted MORE than its access share (bad)
  - overwrite_lift  : same idea for overwrites

A lift > 1 is the smoking-gun signal that hot data is being mishandled
(thrashed/conflict-evicted) rather than kept resident.
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


def process_benchmark(benchmark, args):
    trace_file = f"{trace_dir}/{benchmark}_mesh_{xDim}x{yDim}_memory_trace.csv"
    if not os.path.exists(trace_file):
        return None

    print(f"[{benchmark}] Computing hot-data eviction/overwrite share...")

    try:
        df = pd.read_csv(trace_file, skipinitialspace=True)
        df = df.drop_duplicates()
        df = df[['addr', 'type']]
        if df.empty:
            return None

        df['cacheline'] = df['addr'] // CACHELINE_BYTES

        is_evict = df['type'] == EVICT_TYPE
        is_overwrite = df['type'] == OVERWRITE_TYPE
        is_access = ~(is_evict | is_overwrite)  # demand accesses

        # Per-cacheline counts
        per_line = pd.DataFrame({
            'access_count':    df.loc[is_access,    'cacheline'].value_counts(),
            'evict_count':     df.loc[is_evict,     'cacheline'].value_counts(),
            'overwrite_count': df.loc[is_overwrite, 'cacheline'].value_counts(),
        }).fillna(0).astype(np.int64)

        if per_line['access_count'].sum() == 0:
            return None

        # Rank by access_count desc; cachelines never demand-accessed go to the bottom.
        per_line = per_line.sort_values('access_count', ascending=False)

        total_lines      = len(per_line)
        total_accesses   = per_line['access_count'].sum()
        total_evicts     = per_line['evict_count'].sum()
        total_overwrites = per_line['overwrite_count'].sum()

        row = {
            'benchmark': benchmark,
            'total_unique_cachelines': total_lines,
            'total_accesses':          int(total_accesses),
            'total_evictions':         int(total_evicts),
            'total_overwrites':        int(total_overwrites),
        }

        for pct in args.top_pcts:
            k = max(1, int(np.ceil(total_lines * pct / 100)))
            head = per_line.iloc[:k]

            acc_share  = head['access_count'].sum()    / total_accesses   * 100 if total_accesses   else 0.0
            evc_share  = head['evict_count'].sum()     / total_evicts     * 100 if total_evicts     else 0.0
            ovw_share  = head['overwrite_count'].sum() / total_overwrites * 100 if total_overwrites else 0.0

            evc_lift = (evc_share / acc_share) if acc_share else float('nan')
            ovw_lift = (ovw_share / acc_share) if acc_share else float('nan')

            tag = f"top{pct}%"
            row[f"{tag}_access_share_%"]    = round(acc_share, 2)
            row[f"{tag}_evict_share_%"]     = round(evc_share, 2)
            row[f"{tag}_overwrite_share_%"] = round(ovw_share, 2)
            row[f"{tag}_evict_lift"]        = round(evc_lift, 3) if not np.isnan(evc_lift) else None
            row[f"{tag}_overwrite_lift"]    = round(ovw_lift, 3) if not np.isnan(ovw_lift) else None

        # Also report per-access cache-handling pressure on the top-10% bucket
        # vs everyone else: (evict+overwrite) / access. High ratio on hot data = thrashing.
        k10 = max(1, int(np.ceil(total_lines * 10 / 100)))
        hot  = per_line.iloc[:k10]
        cold = per_line.iloc[k10:]

        def _pressure(block):
            a = block['access_count'].sum()
            return ((block['evict_count'].sum() + block['overwrite_count'].sum()) / a) if a else float('nan')

        row['hot10_evict+ow_per_access']  = round(_pressure(hot),  4)
        row['cold90_evict+ow_per_access'] = round(_pressure(cold), 4)

        return pd.DataFrame([row])

    except Exception as e:
        print(f"[{benchmark}] Error: {e}")
        return None


if __name__ == "__main__":
    parser = argparse.ArgumentParser(
        description="Per-benchmark: share of cache evictions/overwrites concentrated on "
                    "the top-X% hottest cachelines (ranked by demand access count)."
    )
    parser.add_argument('run_id', nargs='?', default='latest',
                        help="run-id (latest, <config>, <config>/<timestamp>, ...)")
    parser.add_argument('--top-pcts', type=lambda s: [int(x) for x in s.split(',')],
                        default=[1, 5, 10, 20],
                        help="comma-separated percentile cutoffs. default: 1,5,10,20")
    args = parser.parse_args()

    for p in args.top_pcts:
        if not (0 < p <= 100):
            parser.error(f"--top-pcts entries must be in (0, 100]; got {p}")

    with ProcessPoolExecutor() as executor:
        results = list(executor.map(process_benchmark, benchmarks, repeat(args)))

    results = [r for r in results if r is not None]
    if not results:
        print("No results generated.")
        sys.exit(0)

    final_df = pd.concat(results, ignore_index=True)

    result_dir = f"{username}/mgvm/analyze/_out/top-n/{date}"
    os.makedirs(result_dir, exist_ok=True)
    output_path = f"{result_dir}/t_hot_data_eviction_share.xlsx"
    final_df.to_excel(output_path, index=False)

    print("\n" + "=" * 80)
    print(f"{'Hot-data eviction/overwrite share':^80}")
    print("=" * 80)
    print(f"Total rows: {len(final_df)}")
    print(f"Saved to:   {output_path}")
    print("=" * 80)
    # Print a focused view
    focus_cols = [c for c in final_df.columns
                  if c == 'benchmark'
                  or c.startswith('total_')
                  or c.startswith('top10%_')
                  or c.startswith('hot10_')
                  or c.startswith('cold90_')]
    print(final_df[focus_cols].to_string(index=False))
