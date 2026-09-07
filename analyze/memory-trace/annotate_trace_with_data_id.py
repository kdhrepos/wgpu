"""
Annotate memory trace CSVs with a DataID column derived from data allocation logs.

Approach mirrors analyze/figure-generator/test_data_log.ipynb:
  - Sort both DataFrames by address
  - pd.merge_asof(trace, alloc, left_on='addr', right_on='StartAddr', direction='backward')
  - Invalidate rows where addr >= EndAddr (address falls outside any allocation range)

Usage:
    python3 analyze/annotate_trace_with_data_id.py \
        --trace-id 20260504-154415-mesh \
        --alloc-id 20260517-154252-mesh-4x4 \
        [--benchmark BENCH]

Paths resolved from IDs (each is `<config>/<timestamp>` or `<timestamp>`):
    trace dir : ~/mgvm/results/<config>/{trace-id}/memory-traces/
    alloc dir : ~/mgvm/results/<config>/{alloc-id}/data-allocation-logs/
    output dir: ~/mgvm/results/<config>/{trace-id}/memory-traces-annotated/

Input files are opened read-only; originals are never modified.
"""

import argparse
import os
import re
import sys
import traceback
from concurrent.futures import ProcessPoolExecutor, as_completed

import pandas as pd

sys.path.insert(0, os.path.join(os.path.dirname(os.path.abspath(__file__)), "..", "scripts"))
from params import benchmarks as PARAMS_BENCHMARKS
from _paths import memory_trace_dir, data_allocation_log_dir, run_root

TRACE_SUFFIX = "_memory_trace.csv"
ALLOC_SUFFIX = "_data_allocation_log.csv"
CHUNK_SIZE = 500_000


def _find_pairs(trace_dir: str, alloc_dir: str) -> dict[str, tuple[str, str]]:
    """
    Return {bench: (trace_path, alloc_path)} for benchmarks listed in params.py
    that have both a trace file and an allocation log present on disk.
    """
    # Build filename maps from what exists on disk
    trace_map: dict[str, str] = {}
    for f in os.listdir(trace_dir):
        if f.endswith(TRACE_SUFFIX):
            m = re.match(r"^(.+?)_mesh_\d+x\d+_memory_trace\.csv$", f)
            if m:
                trace_map[m.group(1)] = os.path.join(trace_dir, f)

    alloc_map: dict[str, str] = {}
    for f in os.listdir(alloc_dir):
        if f.endswith(ALLOC_SUFFIX):
            m = re.match(r"^(.+?)_mesh_\d+x\d+_data_allocation_log\.csv$", f)
            if m:
                alloc_map[m.group(1)] = os.path.join(alloc_dir, f)

    pairs = {}
    for bench in PARAMS_BENCHMARKS:
        if bench in trace_map and bench in alloc_map:
            pairs[bench] = (trace_map[bench], alloc_map[bench])
        else:
            missing = []
            if bench not in trace_map:
                missing.append("trace")
            if bench not in alloc_map:
                missing.append("alloc-log")
            print(f"[SKIP] {bench}: missing {', '.join(missing)}")
    return pairs


def _annotate_chunk(chunk: pd.DataFrame, alloc_sorted: pd.DataFrame) -> pd.DataFrame:
    """
    Annotate one chunk with DataID using merge_asof.
    chunk is in original row order; we sort by addr for the merge then restore order.
    """
    original_index = chunk.index.copy()
    chunk_sorted = chunk.sort_values(by="addr")

    merged = pd.merge_asof(
        chunk_sorted,
        alloc_sorted[["DataID", "StartAddr", "EndAddr"]],
        left_on="addr",
        right_on="StartAddr",
        direction="backward",
    )

    invalid = (merged["addr"] >= merged["EndAddr"]) | merged["StartAddr"].isna()
    merged.loc[invalid, "DataID"] = -1
    merged["DataID"] = merged["DataID"].fillna(-1).astype(int)

    merged = merged.drop(columns=["StartAddr", "EndAddr"])
    merged.index = chunk_sorted.index
    return merged.loc[original_index]


def annotate_benchmark(
    bench: str, trace_path: str, alloc_path: str, output_dir: str
) -> int:
    alloc_df = pd.read_csv(alloc_path, skipinitialspace=True).drop_duplicates()
    alloc_sorted = alloc_df.sort_values(by="StartAddr").reset_index(drop=True)

    out_path = os.path.join(output_dir, os.path.basename(trace_path))
    total_rows = 0
    first_chunk = True

    for chunk in pd.read_csv(trace_path, skipinitialspace=True, chunksize=CHUNK_SIZE):
        annotated = _annotate_chunk(chunk, alloc_sorted)
        annotated.to_csv(
            out_path,
            mode="w" if first_chunk else "a",
            index=False,
            header=first_chunk,
        )
        total_rows += len(annotated)
        first_chunk = False

    return total_rows


def _worker(item):
    bench, (trace_path, alloc_path, output_dir) = item
    rows = annotate_benchmark(bench, trace_path, alloc_path, output_dir)
    return bench, rows


def main() -> None:
    parser = argparse.ArgumentParser(
        description=__doc__,
        formatter_class=argparse.RawDescriptionHelpFormatter,
    )
    parser.add_argument("--trace-id", required=True,
                        help="trace run id — '<config>/<timestamp>' or '<timestamp>'")
    parser.add_argument("--alloc-id", required=True,
                        help="alloc-log run id — '<config>/<timestamp>' or '<timestamp>'")
    parser.add_argument("--benchmark", default=None)
    args = parser.parse_args()

    trace_dir = str(memory_trace_dir(args.trace_id))
    alloc_dir = str(data_allocation_log_dir(args.alloc_id))
    output_dir = str(run_root(args.trace_id) / "memory-traces-annotated")

    for d, label in [(trace_dir, "trace-dir"), (alloc_dir, "alloc-dir")]:
        if not os.path.isdir(d):
            print(f"ERROR: {label} not found: {d}", file=sys.stderr)
            sys.exit(1)

    os.makedirs(output_dir, exist_ok=True)

    pairs = _find_pairs(trace_dir, alloc_dir)

    if args.benchmark:
        if args.benchmark not in pairs:
            print(
                f"ERROR: benchmark '{args.benchmark}' not in matched pairs",
                file=sys.stderr,
            )
            sys.exit(1)
        pairs = {args.benchmark: pairs[args.benchmark]}

    if not pairs:
        print("No benchmarks to process.", file=sys.stderr)
        sys.exit(0)

    n_workers = len(pairs)
    print(f"Processing {n_workers} benchmark(s) with {n_workers} worker(s) → {output_dir}")

    items = {
        bench: (bench, (trace_path, alloc_path, output_dir))
        for bench, (trace_path, alloc_path) in pairs.items()
    }

    with ProcessPoolExecutor(max_workers=n_workers) as executor:
        futures = {
            executor.submit(_worker, item): bench
            for bench, item in items.items()
        }
        for future in as_completed(futures):
            bench = futures[future]
            try:
                _, rows = future.result()
                print(f"[DONE] {bench}: {rows:,} rows written")
            except Exception as exc:
                print(f"[ERROR] {bench}: {exc}", file=sys.stderr)
                traceback.print_exc()


if __name__ == "__main__":
    main()
