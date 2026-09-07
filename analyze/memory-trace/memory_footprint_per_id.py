"""
Per-data_id + per-chiplet memory footprint.

Combines memory_footprint.py (chiplet breakdown) and
memory_footprint_per_data_id.py (buffer breakdown).

footprint = (#unique 64B cachelines touched) * 64 bytes.

Output (~/mgvm/analyze/_out/memory-trace/{date}/memory_footprint_per_id.xlsx):
  - sheet `summary`     : per benchmark totals
  - sheet `per_data_id` : one row per (benchmark, data_id) —
                          total_mb, avg_per_chiplet_mb, c0_mb … cN_mb
"""

import os
import sys
import argparse
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
from _paths import run_root as _run_root, memory_trace_dir as _mtd, run_tag as _run_tag

username = os.path.expanduser('~')

_run_id = next((a for a in sys.argv[1:] if not a.startswith('-')), 'latest')

xDim = 6
yDim = 6

CHUNK_SIZE = 4_000_000
CACHELINE_BYTES = 64
USE_COLS = ['addr', 'chiplet', 'data_id']
DTYPES = {'addr': 'uint64', 'chiplet': 'int16', 'data_id': 'int32'}


def _mb(n_lines):
    return n_lines * CACHELINE_BYTES / (1024 * 1024)


def _accumulate(trace_file, include_ptw, include_unmatched):
    cols = list(USE_COLS)
    dtypes = dict(DTYPES)
    if not include_ptw:
        cols.append('is_ptw')
        dtypes['is_ptw'] = 'int8'

    # (data_id, chiplet) -> set(addr)
    per_id_chiplet = {}
    reader = pd.read_csv(
        trace_file,
        usecols=cols,
        dtype=dtypes,
        skipinitialspace=True,
        chunksize=CHUNK_SIZE,
        engine='c',
    )
    for chunk in reader:
        chunk = chunk.drop_duplicates()    # full-row 완전-중복 제거
        if not include_ptw:
            chunk = chunk[chunk['is_ptw'] == 0]
        if not include_unmatched:
            chunk = chunk[chunk['data_id'] != -1]
        if chunk.empty:
            continue
        for (data_id, chiplet), sub in chunk.groupby(['data_id', 'chiplet'], sort=False)['addr']:
            key = (data_id, chiplet)
            s = per_id_chiplet.get(key)
            if s is None:
                per_id_chiplet[key] = set(sub.to_numpy())
            else:
                s.update(sub.to_numpy())

    return per_id_chiplet


def process_benchmark(benchmark, args):
    num_chiplets = args.xDim * args.yDim
    trace_file = f"{args.trace_dir}/{benchmark}_mesh_{args.xDim}x{args.yDim}_memory_trace.csv"

    if not os.path.exists(trace_file):
        print(f"No {trace_file}, skip")
        return None

    print(f"Processing {benchmark}...", flush=True)

    try:
        per_id_chiplet = _accumulate(trace_file, args.include_ptw, args.include_unmatched)
    except ValueError as e:
        print(f"[{benchmark}] cannot read required columns: {e}")
        return None

    if not per_id_chiplet:
        return None

    # Aggregate per data_id: chiplet_counts dict + total union
    data_ids = set(k[0] for k in per_id_chiplet)
    per_id_total = {}   # data_id -> set(addr) global union
    per_id_chips = {}   # data_id -> {chiplet: count}

    for (data_id, chiplet), s in per_id_chiplet.items():
        if data_id not in per_id_total:
            per_id_total[data_id] = set(s)
            per_id_chips[data_id] = {chiplet: len(s)}
        else:
            per_id_total[data_id].update(s)
            per_id_chips[data_id][chiplet] = len(s)

    # Global total
    global_union = set()
    for s in per_id_total.values():
        global_union.update(s)
    global_lines = len(global_union)

    # Summary row
    summary = {
        "benchmark": benchmark,
        "num_data_ids": len(data_ids),
        "unique_addresses": global_lines,
        "memory_footprint_mb": _mb(global_lines),
    }

    # Per-data_id rows with chiplet breakdown
    rows = []
    for data_id in sorted(data_ids):
        total_lines = len(per_id_total[data_id])
        chip_counts = per_id_chips[data_id]
        active = len(chip_counts)
        avg_lines = sum(chip_counts.values()) / active if active else 0.0

        row = {
            "benchmark": benchmark,
            "data_id": int(data_id),
            "unique_addresses": total_lines,
            "footprint_mb": _mb(total_lines),
            "footprint_pct": (total_lines / global_lines * 100) if global_lines else 0.0,
            "active_chiplets": active,
            "avg_per_chiplet_mb": _mb(avg_lines),
        }
        for i in range(num_chiplets):
            row[f"c{i}_mb"] = _mb(chip_counts.get(i, 0))
        rows.append(row)

    rows.sort(key=lambda r: r["footprint_mb"], reverse=True)

    del per_id_chiplet, per_id_total, global_union
    return summary, rows


if __name__ == "__main__":
    parser = argparse.ArgumentParser(description=__doc__,
                                     formatter_class=argparse.RawDescriptionHelpFormatter)
    parser.add_argument('run_id', nargs='?', default=_run_id,
                        help='run id (e.g. mesh-6x6/latest, 20260625-234420, latest)')
    parser.add_argument('--xDim', type=int, default=xDim)
    parser.add_argument('--yDim', type=int, default=yDim)
    parser.add_argument('--include-ptw', action='store_true')
    parser.add_argument('--include-unmatched', action='store_true')
    args = parser.parse_args()

    args.date = _run_tag(args.run_id)
    args.trace_dir = str(_mtd(args.run_id))
    print(f"Target Directory: {args.trace_dir}")

    summaries = []
    all_rows = []
    max_workers = int(os.environ.get("FOOTPRINT_WORKERS", "20"))
    with ProcessPoolExecutor(max_workers=max_workers) as executor:
        for result in executor.map(process_benchmark, benchmarks, repeat(args)):
            if result is not None:
                summary, rows = result
                summaries.append(summary)
                all_rows.extend(rows)

    if not summaries:
        print(f"No trace files found under {args.trace_dir}; aborting.")
        sys.exit(1)

    summary_df = pd.DataFrame(summaries).sort_values("benchmark")
    per_id_df = pd.DataFrame(all_rows).sort_values(
        ["benchmark", "footprint_mb"], ascending=[True, False])

    result_dir = f"{username}/mgvm/analyze/_out/memory-trace/{args.date}"
    os.makedirs(result_dir, exist_ok=True)
    output_path = f"{result_dir}/memory_footprint_per_id.xlsx"

    with pd.ExcelWriter(output_path, engine="openpyxl") as writer:
        summary_df.to_excel(writer, sheet_name="summary", index=False)
        per_id_df.to_excel(writer, sheet_name="per_data_id", index=False)

    print("\n--- Summary (per benchmark) ---")
    print(summary_df.to_string(index=False))
    print("\n--- Per data_id (head) ---")
    print(per_id_df.head(20).to_string(index=False))
    print(f"\nSaved to: {output_path}")
