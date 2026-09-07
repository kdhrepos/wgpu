"""3C miss classification (compulsory / conflict / capacity) for a configured cache,
driven by the memory trace.

Cache parameters (constants below, edit if simulating something other than the
project's L2):
    cache line size = 64 B
    sets            = 4096
    ways            = 16
    -> total size   = 4 MB
    addr bits       : [tag | 12-bit set | 6-bit offset]

Method per (benchmark, chiplet):
  * Stream all demand accesses (rows with type not in {6,7}) in time order
    through TWO caches kept side-by-side:
      - SA: 4096-set, 16-way LRU (real cache geometry)
      - FA: 65,536-entry fully-associative LRU (same total capacity)
    plus a global `seen_blocks` set.
  * Per access classify:
      SA hit                               -> Hit
      SA miss, block never seen            -> Compulsory
      SA miss, seen, FA hit                -> Conflict   (set associativity hurt us)
      SA miss, seen, FA miss               -> Capacity   (working set > 4 MB)
  * compulsory + conflict + capacity == total misses.

Aggregation:
  * Sum counters across 36 chiplets.
  * Sum per-set conflict counts across chiplets (logical-set view: tells us if
    a particular set index is systematically hot across instances). Then report
    what % of total conflict misses fall in the top-1/5/10/25% hottest sets.

Note this only models a single private cache per chiplet with the given
geometry; it does not model the directory/coherence layer or any prefetcher.
"""

import os
import sys
import argparse
import numpy as np
import pandas as pd
from collections import OrderedDict

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

# Cache geometry
LINE_BYTES = 64
NUM_SETS = 4096
NUM_WAYS = 16
FA_CAPACITY = NUM_SETS * NUM_WAYS  # 65,536 entries -> same total capacity as SA

OFFSET_BITS = 6   # log2(64)
INDEX_BITS = 12   # log2(4096)
SET_MASK = NUM_SETS - 1


def simulate_chiplet(addrs):
    """Stream block addresses through SA + FA LRU and return counter tuple +
    per-set conflict array."""
    sets = [OrderedDict() for _ in range(NUM_SETS)]
    fa = OrderedDict()
    seen = set()

    hits = compulsory = conflict = capacity = 0
    per_set_conflict = np.zeros(NUM_SETS, dtype=np.int64)

    for addr in addrs:
        block = int(addr) >> OFFSET_BITS
        set_idx = block & SET_MASK

        s = sets[set_idx]
        sa_hit = block in s
        fa_hit = block in fa

        if sa_hit:
            hits += 1
            s.move_to_end(block)
        else:
            if block not in seen:
                compulsory += 1
            elif fa_hit:
                conflict += 1
                per_set_conflict[set_idx] += 1
            else:
                capacity += 1

            # install into SA, evicting LRU if full
            s[block] = None
            if len(s) > NUM_WAYS:
                s.popitem(last=False)

        # FA state always advances with the access stream
        if fa_hit:
            fa.move_to_end(block)
        else:
            fa[block] = None
            if len(fa) > FA_CAPACITY:
                fa.popitem(last=False)

        seen.add(block)

    return hits, compulsory, conflict, capacity, per_set_conflict


def process_benchmark(benchmark, args):
    trace_file = f"{trace_dir}/{benchmark}_mesh_{xDim}x{yDim}_memory_trace.csv"
    if not os.path.exists(trace_file):
        return None

    print(f"[{benchmark}] 3C sim (per-chiplet)...")

    try:
        df = pd.read_csv(trace_file, skipinitialspace=True)
        df = df.drop_duplicates()
        df = df[['time', 'addr', 'chiplet', 'type']]
        if df.empty:
            return None

        # Demand accesses only: drop eviction/overwrite bookkeeping events.
        df = df[~df['type'].isin([6, 7, 8, 9])]
        if args.l2miss_only:
            df = df[df['type'].isin([2, 5])]
        if df.empty:
            return None

        # Sum across chiplets
        hits = compulsory = conflict = capacity = 0
        per_set_conflict = np.zeros(NUM_SETS, dtype=np.int64)

        for chip, group in df.groupby('chiplet', sort=False):
            addrs = group.sort_values('time')['addr'].to_numpy()
            h, comp, conf, cap, psc = simulate_chiplet(addrs)
            hits        += h
            compulsory  += comp
            conflict    += conf
            capacity    += cap
            per_set_conflict += psc

        total_misses    = compulsory + conflict + capacity
        total_accesses  = hits + total_misses

        row = {
            'benchmark': benchmark,
            'total_accesses': int(total_accesses),
            'total_misses':   int(total_misses),
            'hit_rate_%':     round(hits / total_accesses * 100, 2) if total_accesses else 0.0,
            'miss_rate_%':    round(total_misses / total_accesses * 100, 2) if total_accesses else 0.0,
            'compulsory':     int(compulsory),
            'conflict':       int(conflict),
            'capacity':       int(capacity),
            'compulsory_%_of_miss': round(compulsory / total_misses * 100, 2) if total_misses else 0.0,
            'conflict_%_of_miss':   round(conflict   / total_misses * 100, 2) if total_misses else 0.0,
            'capacity_%_of_miss':   round(capacity   / total_misses * 100, 2) if total_misses else 0.0,
            'conflict_per_kacc':    round(conflict / total_accesses * 1000, 3) if total_accesses else 0.0,
        }

        # Per-set conflict hotspot concentration (logical-set view: summed across chiplets)
        sorted_conf = np.sort(per_set_conflict)[::-1]
        total_conf = sorted_conf.sum()
        nonzero_sets = int((per_set_conflict > 0).sum())
        row['conflict_hotspot_sets_with_>0'] = nonzero_sets

        for pct in args.set_pcts:
            k = max(1, int(np.ceil(NUM_SETS * pct / 100)))
            share = sorted_conf[:k].sum() / total_conf * 100 if total_conf else 0.0
            row[f"top{pct}%_set_conflict_share_%"] = round(share, 2)

        return pd.DataFrame([row])

    except Exception as e:
        print(f"[{benchmark}] Error: {e}")
        return None


if __name__ == "__main__":
    parser = argparse.ArgumentParser(
        description="3C miss classification (compulsory/conflict/capacity) on a "
                    f"{NUM_SETS}-set, {NUM_WAYS}-way, {LINE_BYTES}B-line LRU cache, "
                    "simulated per chiplet."
    )
    parser.add_argument('run_id', nargs='?', default='latest',
                        help="run-id (latest, <config>, <config>/<timestamp>, ...)")
    parser.add_argument('--l2miss-only', action='store_true',
                        help="restrict input stream to type in {2,5} (project's L2-miss markers); "
                             "default: all rows except evict/overwrite (types 6/7)")
    parser.add_argument('--set-pcts', type=lambda s: [int(x) for x in s.split(',')],
                        default=[1, 5, 10, 25],
                        help="comma-separated top-N% set hotspot cutoffs (default: 1,5,10,25)")
    args = parser.parse_args()

    for p in args.set_pcts:
        if not (0 < p <= 100):
            parser.error(f"--set-pcts entries must be in (0,100]; got {p}")

    with ProcessPoolExecutor() as executor:
        results = list(executor.map(process_benchmark, benchmarks, repeat(args)))

    results = [r for r in results if r is not None]
    if not results:
        print("No results generated.")
        sys.exit(0)

    final_df = pd.concat(results, ignore_index=True)

    result_dir = f"{username}/mgvm/analyze/_out/cacheline/{date}"
    os.makedirs(result_dir, exist_ok=True)
    suffix = "_l2miss" if args.l2miss_only else ""
    output_path = f"{result_dir}/t_set_conflict_miss_3c{suffix}.xlsx"
    final_df.to_excel(output_path, index=False)

    print("\n" + "=" * 80)
    print(f"{'3C miss classification (per-chiplet sim, ' + str(NUM_SETS) + ' sets x ' + str(NUM_WAYS) + ' ways)':^80}")
    print("=" * 80)
    print(f"Rows: {len(final_df)}")
    print(f"Saved: {output_path}")
    print("=" * 80)
    cols = ['benchmark', 'miss_rate_%',
            'compulsory_%_of_miss', 'conflict_%_of_miss', 'capacity_%_of_miss',
            'conflict_per_kacc',
            'top1%_set_conflict_share_%', 'top10%_set_conflict_share_%']
    cols = [c for c in cols if c in final_df.columns]
    print(final_df[cols].to_string(index=False))
