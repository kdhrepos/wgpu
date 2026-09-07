"""
Time-windowed local/remote and per-data_id L2 access ratios,
averaged across chiplets (macro-average).

For each time window AND each chiplet, computes that chiplet's own ratio at its
L2 (local = chiplet == owner, remote = chiplet != owner), then averages those
per-chiplet ratios over the chiplets active in the window (unweighted mean, so a
busy chiplet does not dominate). `std_*` is the inter-chiplet spread.

Only demand accesses count (type 0..5, is_ptw == 0). Window size = `--window-ns`
(1 ns == 1 cycle @ 1 GHz). Time is streamed; the trace's first row fixes t0.

Output (~/mgvm/analyze/_out/access-count/{date}/), one CSV pair per benchmark:
  - {bench}_local_remote_per_time.csv :
        start,end,active_chiplets,mean_local_ratio,mean_remote_ratio,std_remote_ratio
  - {bench}_data_id_per_time.csv (long form, mean per-chiplet share of each buffer):
        start,end,data_id,active_chiplets,mean_ratio,std_ratio
"""

import os
import sys
import argparse
import numpy as np
import pandas as pd
from collections import defaultdict

parent_dir = os.path.abspath(os.path.join(os.path.dirname(__file__), "../../"))
if parent_dir not in sys.path:
    sys.path.append(parent_dir)

scripts_dir = os.path.abspath(os.path.join(os.path.dirname(__file__), "../../../scripts"))
if scripts_dir not in sys.path:
    sys.path.append(scripts_dir)

from concurrent.futures import ProcessPoolExecutor
from itertools import repeat
from params import benchmarks
from _paths import run_root as _run_root, memory_trace_dir as _mtd, run_tag as _run_tag

username = os.path.expanduser('~')

_run_id = next((a for a in sys.argv[1:] if not a.startswith('-')), 'latest')

xDim = 6
yDim = 6

NS = 1e-9
CHUNK_SIZE = 4_000_000
USE_COLS = ['time', 'chiplet', 'owner', 'type', 'data_id', 'is_ptw']
DTYPES = {'time': 'float64', 'chiplet': 'int16', 'owner': 'int16',
          'type': 'int8', 'data_id': 'int32', 'is_ptw': 'int8'}


def _accumulate(trace_file, window_ns, include_ptw, include_evict, include_unmatched):
    lr = defaultdict(lambda: [0, 0])   # (bin, chiplet) -> [local, total]
    did = defaultdict(int)             # (bin, chiplet, data_id) -> count
    t0 = None

    reader = pd.read_csv(
        trace_file,
        usecols=USE_COLS,
        dtype=DTYPES,
        skipinitialspace=True,
        chunksize=CHUNK_SIZE,
        engine='c',
    )
    for chunk in reader:
        chunk = chunk.drop_duplicates()
        if not include_ptw:
            chunk = chunk[chunk['is_ptw'] == 0]
        if not include_evict:
            chunk = chunk[chunk['type'] <= 5]
        if chunk.empty:
            continue

        if t0 is None:
            t0 = chunk['time'].iloc[0]

        t_ns = (chunk['time'].to_numpy() - t0) / NS
        b = (t_ns // window_ns).astype('int64')
        is_local = (chunk['chiplet'].to_numpy() == chunk['owner'].to_numpy()).astype('int8')

        tmp = pd.DataFrame({
            'bin': b,
            'chiplet': chunk['chiplet'].to_numpy(),
            'local': is_local,
            'data_id': chunk['data_id'].to_numpy(),
        })

        g = tmp.groupby(['bin', 'chiplet'])['local'].agg(['sum', 'count'])
        for (bin_id, chip), row in g.iterrows():
            slot = lr[(bin_id, chip)]
            slot[0] += int(row['sum'])
            slot[1] += int(row['count'])

        did_chunk = tmp if include_unmatched else tmp[tmp['data_id'] != -1]
        if not did_chunk.empty:
            gd = did_chunk.groupby(['bin', 'chiplet', 'data_id']).size()
            for (bin_id, chip, data_id), cnt in gd.items():
                did[(bin_id, chip, int(data_id))] += int(cnt)

    return lr, did


def process_benchmark(benchmark, args):
    trace_file = f"{args.trace_dir}/{benchmark}_mesh_{args.xDim}x{args.yDim}_memory_trace.csv"

    if not os.path.exists(trace_file):
        print(f"No {trace_file}, skip")
        return None

    print(f"Processing {benchmark}...", flush=True)

    try:
        lr, did = _accumulate(trace_file, args.window_ns, args.include_ptw,
                              args.include_evict, args.include_unmatched)
    except ValueError as e:
        print(f"[{benchmark}] cannot read required columns: {e}")
        return None

    if not lr:
        return None

    w = args.window_ns

    # active chiplets (total > 0) per bin, carrying their totals
    active = defaultdict(list)   # bin -> [(chiplet, total, local)]
    for (b, c), (loc, tot) in lr.items():
        if tot > 0:
            active[b].append((c, tot, loc))

    did_by_bc = defaultdict(dict)  # (bin, chiplet) -> {data_id: count}
    for (b, c, data_id), cnt in did.items():
        did_by_bc[(b, c)][data_id] = cnt

    lr_rows = []
    did_rows = []
    for b in sorted(active):
        chips = active[b]                       # [(c, tot, loc)]
        n = len(chips)
        remote_ratios = np.array([(tot - loc) / tot for (_, tot, loc) in chips])
        mean_remote = float(remote_ratios.mean())
        std_remote = float(remote_ratios.std())  # population std across chiplets
        lr_rows.append({
            "start": b * w,
            "end": (b + 1) * w,
            "active_chiplets": n,
            "mean_local_ratio": 1.0 - mean_remote,
            "mean_remote_ratio": mean_remote,
            "std_remote_ratio": std_remote,
        })

        # per-data_id mean share across the SAME active chiplets (0 if untouched)
        ids = set()
        for (c, _, _) in chips:
            ids.update(did_by_bc.get((b, c), {}).keys())
        for data_id in sorted(ids):
            shares = np.array([did_by_bc.get((b, c), {}).get(data_id, 0) / tot
                               for (c, tot, _) in chips])
            did_rows.append({
                "start": b * w,
                "end": (b + 1) * w,
                "data_id": data_id,
                "active_chiplets": n,
                "mean_ratio": float(shares.mean()),
                "std_ratio": float(shares.std()),
            })

    lr_df = pd.DataFrame(lr_rows)
    did_df = pd.DataFrame(did_rows) if did_rows else pd.DataFrame(
        columns=["start", "end", "data_id", "active_chiplets", "mean_ratio", "std_ratio"])

    result_dir = f"{username}/mgvm/analyze/_out/access-count/{args.date}"
    os.makedirs(result_dir, exist_ok=True)
    lr_df.to_csv(f"{result_dir}/{benchmark}_local_remote_per_time.csv", index=False)
    did_df.to_csv(f"{result_dir}/{benchmark}_data_id_per_time.csv", index=False)

    return {
        "benchmark": benchmark,
        "windows": len(lr_df),
        "window_ns": w,
        "mean_remote_ratio": float(lr_df["mean_remote_ratio"].mean()) if len(lr_df) else 0.0,
        "num_data_ids": did_df["data_id"].nunique() if not did_df.empty else 0,
    }


if __name__ == "__main__":
    parser = argparse.ArgumentParser(
        description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    parser.add_argument('run_id', nargs='?', default=_run_id,
                        help='run id (e.g. mesh-6x6/latest, 20260625-234420, latest)')
    parser.add_argument('--xDim', type=int, default=xDim)
    parser.add_argument('--yDim', type=int, default=yDim)
    parser.add_argument('--window-ns', type=int, default=10000,
                        help='time window in ns (1 ns == 1 cycle @ 1 GHz)')
    parser.add_argument('--include-ptw', action='store_true',
                        help='include is_ptw == 1 rows (default: drop)')
    parser.add_argument('--include-evict', action='store_true',
                        help='include eviction/victim rows type 6..9 (default: drop)')
    parser.add_argument('--include-unmatched', action='store_true',
                        help='include data_id == -1 rows in the data_id sheet (default: drop)')
    args = parser.parse_args()

    args.date = _run_tag(args.run_id)
    args.trace_dir = str(_mtd(args.run_id))
    print(f"Target Directory: {args.trace_dir}")

    summaries = []
    max_workers = int(os.environ.get("FOOTPRINT_WORKERS", "20"))
    with ProcessPoolExecutor(max_workers=max_workers) as executor:
        for result in executor.map(process_benchmark, benchmarks, repeat(args)):
            if result is not None:
                summaries.append(result)

    if not summaries:
        print(f"No trace files found under {args.trace_dir}; aborting.")
        sys.exit(1)

    summary_df = pd.DataFrame(summaries).sort_values("benchmark")
    result_dir = f"{username}/mgvm/analyze/_out/access-count/{args.date}"
    summary_df.to_csv(f"{result_dir}/local_remote_per_time_summary.csv", index=False)

    print("\n--- Summary (per benchmark, chiplet-averaged) ---")
    print(summary_df.to_string(index=False))
    print(f"\nPer-benchmark CSVs saved under: {result_dir}")
