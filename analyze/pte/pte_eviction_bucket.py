"""Per-benchmark histogram of how many times each PTE gets evicted.

A "PTE caching instance" is a (chiplet, cacheline-addr) pair: the same PTE can
live in multiple chiplets' PTE caches, and each instance gets evicted
independently. The universe of PTEs is every (chiplet, cacheline) that ever
appeared as a PTE access in the memory trace (`is_ptw == 1`). An eviction
event is `is_ptw == 1 && type in {8, 9}` (type 6 = evict, type 7 = overwrite,
treated identically).

For each benchmark we bucket PTE instances by their eviction count and report
the % of PTEs in each bucket. PTEs with zero evictions are included.

Default buckets: 0 / 1 / 2-5 / 6-20 / 21-100 / >100.
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
EVICT_TYPES = (8, 9)  # 8 = victim_evict, 9 = victim_overwrite


def _parse_buckets(s):
    """Parse a bucket spec like "0,1,5,20,100" into (bins, labels).

    Each integer is an inclusive upper edge for a bucket starting at the
    previous edge + 1 (the first bucket starts at 0). A final ">last" bucket
    catches everything above the last edge.

    Example: "0,1,5,20,100" -> labels ['0','1','2-5','6-20','21-100','>100']
    """
    edges = [int(x) for x in s.split(',')]
    if edges != sorted(set(edges)) or edges[0] < 0:
        raise argparse.ArgumentTypeError(
            "--bucket-edges must be ascending unique non-negative ints")

    labels = []
    prev = -1  # so first bucket starts at 0
    for e in edges:
        if e == prev + 1:
            labels.append(f"{e}")
        else:
            labels.append(f"{prev + 1}-{e}")
        prev = e
    labels.append(f">{edges[-1]}")

    # pd.cut with right=True: bin (a, b] catches counts in (a, b].
    # We want bucket boundaries at the .5 marks so integers fall cleanly.
    bins = [-0.5] + [e + 0.5 for e in edges] + [np.inf]
    return bins, labels


def process_benchmark(benchmark, args):
    trace_file = f"{trace_dir}/{benchmark}_mesh_{xDim}x{yDim}_memory_trace.csv"
    if not os.path.exists(trace_file):
        return None

    print(f"[{benchmark}] Computing PTE eviction histogram...")

    try:
        df = pd.read_csv(
            trace_file,
            skipinitialspace=True,
        )
        df = df.drop_duplicates()
        df = df[['addr', 'chiplet', 'is_ptw', 'type']]

        # Restrict to PTE-related rows only.
        df = df[df['is_ptw'] == 1]
        if df.empty:
            return None

        df['cacheline'] = df['addr'] // CACHELINE_BYTES
        df['is_evict'] = df['type'].isin(EVICT_TYPES)

        # Per (chiplet, cacheline): how many evictions and was it ever touched?
        evict_count = (df.groupby(['chiplet', 'cacheline'])['is_evict']
                         .sum()
                         .astype(np.int64))
        # evict_count's index is every (chiplet, cacheline) that ever appeared
        # as a PTE access — so 0-eviction PTEs are naturally included.

        bins, labels = args.buckets
        buckets = pd.cut(evict_count, bins=bins, labels=labels, right=True)
        counts = buckets.value_counts().reindex(labels, fill_value=0)
        total_ptes = int(counts.sum())
        total_evicts = int(evict_count.sum())

        pct = (counts / total_ptes * 100).round(2) if total_ptes else counts * 0.0

        row = {'benchmark': benchmark,
               'total_pte_instances': total_ptes,
               'total_evictions': total_evicts,
               'evicts_per_pte_mean': round(evict_count.mean(), 3) if total_ptes else 0.0,
               'evicts_per_pte_max': int(evict_count.max()) if total_ptes else 0}
        for lbl in labels:
            row[f"evict={lbl}_%"] = float(pct[lbl])
        return pd.DataFrame([row])

    except Exception as e:
        print(f"[{benchmark}] Error: {e}")
        return None


if __name__ == "__main__":
    parser = argparse.ArgumentParser(
        description="Per-benchmark histogram of PTE eviction count. "
                    "A PTE instance is a (chiplet, cacheline). "
                    "Eviction = is_ptw==1 & type in {8,9}.")
    parser.add_argument('run_id', nargs='?', default='latest',
                        help="run-id (latest, <config>, <config>/<timestamp>, ...)")
    parser.add_argument(
        '--bucket-edges',
        dest='buckets',
        type=_parse_buckets,
        default=_parse_buckets("0,1,5,20,100"),
        help="comma-separated ascending inclusive upper edges. "
             "default: 0,1,5,20,100 -> 0 / 1 / 2-5 / 6-20 / 21-100 / >100")
    args = parser.parse_args()

    with ProcessPoolExecutor() as executor:
        results = list(executor.map(process_benchmark, benchmarks, repeat(args)))

    results = [r for r in results if r is not None]
    if not results:
        print("No results generated.")
        sys.exit(0)

    final_df = pd.concat(results, ignore_index=True)

    result_dir = f"{username}/mgvm/analyze/_out/pte/{date}"
    os.makedirs(result_dir, exist_ok=True)
    output_path = f"{result_dir}/t_pte_eviction_bucket.xlsx"
    final_df.to_excel(output_path, index=False)

    print("\n" + "=" * 80)
    print(f"{'Per-PTE Eviction Count Histogram':^80}")
    print("=" * 80)
    print(f"Total rows: {len(final_df)}")
    print(f"Saved to:   {output_path}")
    print("=" * 80)
    print(final_df.to_string(index=False))
