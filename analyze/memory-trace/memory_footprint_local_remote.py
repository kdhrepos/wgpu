"""
Local vs remote memory footprint (unique-cacheline working set).

Sibling of memory_footprint.py, but splits the footprint by whether the
requesting chiplet owns the line's home:
  - local  = rows where chiplet == owner  (line lives in the requestor's chiplet)
  - remote = rows where chiplet != owner  (line pulled from another chiplet)

footprint = (#unique 64B cachelines) * 64 bytes.

A line can be in BOTH the local set (touched by its owner) and the remote set
(touched by some other chiplet), so local + remote >= total. `remote_only` is
the set of lines that were *never* touched locally (remote-exclusive working set).

Only demand accesses count (type 0..5, is_ptw == 0) — eviction/victim rows
(type 6..9) and PTW loads are dropped by default.

Output (~/mgvm/analyze/_out/memory-trace/{date}/memory_footprint_local_remote.xlsx):
  - sheet `summary`     : per benchmark — total/local/remote/remote_only MB + remote fraction
  - sheet `per_chiplet` : per (benchmark, chiplet) — owned working set vs remote working set
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
USE_COLS = ['addr', 'chiplet', 'owner', 'type', 'is_ptw']
DTYPES = {'addr': 'uint64', 'chiplet': 'int16', 'owner': 'int16',
          'type': 'int8', 'is_ptw': 'int8'}


def _mb(n_lines):
    return n_lines * CACHELINE_BYTES / (1024 * 1024)


def _accumulate(trace_file, include_ptw, include_evict):
    # per requesting chiplet: lines it touches that it owns / that are remote
    local_by_chip = {}   # chiplet -> set(addr), owner == chiplet
    remote_by_chip = {}  # chiplet -> set(addr), owner != chiplet

    reader = pd.read_csv(
        trace_file,
        usecols=USE_COLS,
        dtype=DTYPES,
        skipinitialspace=True,
        chunksize=CHUNK_SIZE,
        engine='c',
    )
    for chunk in reader:
        chunk = chunk.drop_duplicates()    # full-row 완전-중복 제거
        if not include_ptw:
            chunk = chunk[chunk['is_ptw'] == 0]
        if not include_evict:
            chunk = chunk[chunk['type'] <= 5]
        if chunk.empty:
            continue

        is_local = chunk['chiplet'].to_numpy() == chunk['owner'].to_numpy()
        loc = chunk[is_local]
        rem = chunk[~is_local]

        for chiplet, sub in loc.groupby('chiplet', sort=False)['addr']:
            s = local_by_chip.get(chiplet)
            if s is None:
                local_by_chip[chiplet] = set(sub.to_numpy())
            else:
                s.update(sub.to_numpy())
        for chiplet, sub in rem.groupby('chiplet', sort=False)['addr']:
            s = remote_by_chip.get(chiplet)
            if s is None:
                remote_by_chip[chiplet] = set(sub.to_numpy())
            else:
                s.update(sub.to_numpy())
    return local_by_chip, remote_by_chip


def process_benchmark(benchmark, args):
    num_chiplets = args.xDim * args.yDim
    trace_file = f"{args.trace_dir}/{benchmark}_mesh_{args.xDim}x{args.yDim}_memory_trace.csv"

    if not os.path.exists(trace_file):
        print(f"No {trace_file}, skip")
        return None

    print(f"Processing {benchmark}...", flush=True)

    try:
        local_by_chip, remote_by_chip = _accumulate(
            trace_file, args.include_ptw, args.include_evict)
    except ValueError as e:
        print(f"[{benchmark}] cannot read required columns: {e}")
        return None

    if not local_by_chip and not remote_by_chip:
        return None

    # global unique sets
    local_all = set()
    for s in local_by_chip.values():
        local_all.update(s)
    remote_all = set()
    for s in remote_by_chip.values():
        remote_all.update(s)
    total_all = local_all | remote_all
    remote_only = remote_all - local_all

    total_lines = len(total_all)
    summary = {
        "benchmark": benchmark,
        "total_footprint_mb": _mb(total_lines),
        "local_footprint_mb": _mb(len(local_all)),
        "remote_footprint_mb": _mb(len(remote_all)),
        "remote_only_footprint_mb": _mb(len(remote_only)),
        "remote_footprint_pct": (len(remote_all) / total_lines * 100) if total_lines else 0.0,
        "remote_only_footprint_pct": (len(remote_only) / total_lines * 100) if total_lines else 0.0,
    }

    # per-chiplet rows: owned vs remote working set that each chiplet actually touches
    rows = []
    for c in range(num_chiplets):
        loc_n = len(local_by_chip.get(c, ()))
        rem_n = len(remote_by_chip.get(c, ()))
        tot_n = loc_n + rem_n
        rows.append({
            "benchmark": benchmark,
            "chiplet": c,
            "local_footprint_mb": _mb(loc_n),
            "remote_footprint_mb": _mb(rem_n),
            "remote_fraction": (rem_n / tot_n) if tot_n else 0.0,
        })

    del local_by_chip, remote_by_chip, local_all, remote_all, total_all
    return summary, rows


if __name__ == "__main__":
    parser = argparse.ArgumentParser(
        description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    parser.add_argument('run_id', nargs='?', default=_run_id,
                        help='run id (e.g. mesh-6x6/latest, 20260625-234420, latest)')
    parser.add_argument('--xDim', type=int, default=xDim)
    parser.add_argument('--yDim', type=int, default=yDim)
    parser.add_argument('--include-ptw', action='store_true',
                        help='include is_ptw == 1 rows (default: drop)')
    parser.add_argument('--include-evict', action='store_true',
                        help='include eviction/victim rows type 6..9 (default: drop)')
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
    per_chiplet_df = pd.DataFrame(all_rows).sort_values(["benchmark", "chiplet"])

    result_dir = f"{username}/mgvm/analyze/_out/memory-trace/{args.date}"
    os.makedirs(result_dir, exist_ok=True)
    output_path = f"{result_dir}/memory_footprint_local_remote.xlsx"

    with pd.ExcelWriter(output_path, engine="openpyxl") as writer:
        summary_df.to_excel(writer, sheet_name="summary", index=False)
        per_chiplet_df.to_excel(writer, sheet_name="per_chiplet", index=False)

    print("\n--- Summary (per benchmark) ---")
    print(summary_df.to_string(index=False))
    print(f"\nSaved to: {output_path}")
