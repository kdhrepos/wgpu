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
from params import benchmarks

username = os.path.expanduser('~')

# run_id 결정: argv 첫 positional, 없으면 'latest'
_run_id = next((a for a in sys.argv[1:] if not a.startswith('-')), 'latest')
from _paths import run_root as _run_root, memory_trace_dir as _mtd, run_tag as _run_tag
date = _run_tag(_run_id)
trace_dir = str(_mtd(_run_id))

xDim = 6
yDim = 6
num_chiplets = xDim * yDim

CHUNK_SIZE = 4_000_000
USE_COLS = ['addr', 'chiplet']
DTYPES = {'addr': 'uint64', 'chiplet': 'int16'}
PTE_FILTER = False  # overridden by memory_footprint_pte.py


def _accumulate(trace_file, pte_only):
    cols = USE_COLS + (['is_ptw'] if pte_only else [])
    dtypes = dict(DTYPES)
    if pte_only:
        dtypes['is_ptw'] = 'int8'

    per_chiplet = {}  # chiplet -> set(addr)
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
        if pte_only:
            chunk = chunk[chunk['is_ptw'] == 1]
            if chunk.empty:
                continue
        # group once per chunk; faster than per-row updates
        for chiplet, sub in chunk.groupby('chiplet', sort=False)['addr']:
            s = per_chiplet.get(chiplet)
            if s is None:
                per_chiplet[chiplet] = set(sub.to_numpy())
            else:
                s.update(sub.to_numpy())
    return per_chiplet


def process_benchmark(benchmark):
    trace_file = f"{trace_dir}/{benchmark}_mesh_{xDim}x{yDim}_memory_trace.csv"

    if not os.path.exists(trace_file):
        print(f"No {trace_file}, skip")
        return None

    print(f"Processing {benchmark}...", flush=True)

    per_chiplet = _accumulate(trace_file, PTE_FILTER)

    # global unique addrs = union of per-chiplet sets
    total_unique = set()
    for s in per_chiplet.values():
        total_unique.update(s)

    addrs = len(total_unique)
    addrs_in_mb = addrs * 64 / (1024 * 1024)

    per_chiplet_counts = {c: len(s) for c, s in per_chiplet.items()}
    if per_chiplet_counts:
        avg_unique = sum(per_chiplet_counts.values()) / len(per_chiplet_counts)
    else:
        avg_unique = 0.0
    avg_mb = avg_unique * 64 / (1024 * 1024)

    res = {
        "benchmark": benchmark,
        "unique_addresses": addrs,
        "memory_footprint_mb": addrs_in_mb,
        "avg_unique_addresses_per_chiplet": avg_unique,
        "avg_memory_footprint_per_chiplet_mb": avg_mb,
    }

    for i in range(num_chiplets):
        cnt = per_chiplet_counts.get(i, per_chiplet_counts.get(f"c{i}", 0))
        res[f"c{i}_footprint_mb"] = cnt * 64 / (1024 * 1024)

    # free memory before returning
    del per_chiplet, total_unique
    return res


if __name__ == "__main__":
    import argparse as _ap
    _p = _ap.ArgumentParser(add_help=False)
    _p.add_argument('run_id', nargs='?', default='latest')
    _p.parse_known_args()

    print(f"Target Directory: {trace_dir}")

    results = []
    # Cap workers: each loads multi-GB CSVs. 4 is a safe default on slurm nodes.
    max_workers = int(os.environ.get("FOOTPRINT_WORKERS", "20"))
    with ProcessPoolExecutor(max_workers=max_workers) as executor:
        for result in executor.map(process_benchmark, benchmarks):
            if result is not None:
                results.append(result)

    if not results:
        print(f"No trace files found under {trace_dir}; aborting.")
        sys.exit(1)

    results_df = pd.DataFrame(results).sort_values("benchmark")

    # Long-form per-chiplet table for readability.
    long_rows = []
    for r in results:
        total = r["memory_footprint_mb"]
        for i in range(num_chiplets):
            mb = r.get(f"c{i}_footprint_mb", 0.0)
            long_rows.append({
                "benchmark": r["benchmark"],
                "chiplet": i,
                "footprint_mb": mb,
                "footprint_pct": (mb / total * 100) if total else 0.0,
            })
    per_chiplet_df = pd.DataFrame(long_rows).sort_values(["benchmark", "chiplet"])

    result_dir = f"{username}/mgvm/analyze/_out/memory-trace/{date}"
    os.makedirs(result_dir, exist_ok=True)

    output_path = f"{result_dir}/memory_footprint.xlsx"

    with pd.ExcelWriter(output_path, engine="openpyxl") as writer:
        results_df.to_excel(writer, sheet_name="summary", index=False)
        per_chiplet_df.to_excel(writer, sheet_name="per_chiplet", index=False)

    print("\n--- Analysis Summary ---")
    print(results_df)
    print(f"\nSummary saved to: {output_path}")
