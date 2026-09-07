"""
Per-data_id access summary from memory traces.

For each benchmark in a memory-trace directory, computes:
  - access_count      : total number of memory accesses to that data_id
  - cacheline_count   : number of distinct cache lines (unique addr) touched
  - avg_access_per_cl : access_count / cacheline_count (reuse density per line)
  - avg_chiplet_per_cl: for each cacheline, count how many distinct chiplets
                        touched it; then average across the data_id's lines.
                        This is the true "sharing degree" — a data_id-level
                        nunique would be nearly always 16 (uninformative).

`addr` in the trace is already cacheline-aligned (matches the convention used
by analyze/pc_shared_ratio.py and pc_rdma_metrics.py — they `nunique` on addr).

data_id == -1 means the address fell outside every allocation range; those rows
are excluded from the output by default (pass --include-unmatched to keep them).

`is_ptw == 1` rows (page-table-walk memory accesses) are dropped by default;
pass --include-ptw to keep them. Older traces without the `is_ptw` column are
treated as if every row is non-PTW.

Access-mode filters (mutually exclusive — pick at most one):
  (default)            all rows
  --rdma-only          rdma == 1
  --rdma-l2miss-only   rdma == 1 AND type in {2, 5}  — L2 misses actually
                       served via RDMA, useful for narrowing access counts to
                       the cross-chiplet traffic that actually paid latency.

Usage:
    python3 analyze/data_structure_access_summary.py \\
        --trace-id 20260520-235018-mesh-6x6 \\
        [--benchmark BENCH] \\
        [--rdma-only | --rdma-l2miss-only] \\
        [--include-unmatched] \\
        [--include-ptw] \\
        [--out-dir ./data-structure-metrics/access-summary]

Input path : ~/mgvm/results/<config>/{trace-id}/memory-traces/
Output path: {out-dir}/{option-tag}/{benchmark}_data_structure_access_summary.csv

  {option-tag} encodes which filtering options were used so results from
  different option combos don't overwrite each other:
    (no options)         → "default"
    --rdma-only          → "rdma-only"
    --rdma-l2miss-only   → "rdma-l2miss-only"
    --include-unmatched  → adds "with-unmatched"
    --include-ptw        → adds "with-ptw"
    combinations are joined with "_" (e.g. "rdma-l2miss-only_with-ptw").
"""

import argparse
import os
username = os.path.expanduser('~')
import re
import sys
from concurrent.futures import ProcessPoolExecutor, as_completed

import pandas as pd

sys.path.insert(0, os.path.join(os.path.dirname(os.path.abspath(__file__)), "..", "scripts"))
from params import benchmarks as PARAMS_BENCHMARKS
from _paths import memory_trace_dir

TRACE_SUFFIX = "_memory_trace.csv"
CHUNK_SIZE = 500_000

# L2-miss access types in the memory tracer's `type` column. The tracer emits
# task.ID, and the memory-access tasks tagged at L2 miss boundaries carry type
# values 2 and 5. Used by --rdma-l2miss-only to isolate L2 misses that actually
# crossed the chiplet boundary (i.e., served via RDMA).
L2_MISS_TYPES = {2, 5}


def _find_traces(trace_dir: str) -> dict[str, str]:
    result = {}
    for f in os.listdir(trace_dir):
        if f.endswith(TRACE_SUFFIX):
            m = re.match(r"^(.+?)_mesh_\d+x\d+_memory_trace\.csv$", f)
            if m:
                result[m.group(1)] = os.path.join(trace_dir, f)
    return result


def _worker(item):
    bench, trace_path, access_mode, include_unmatched, include_ptw, out_dir = item
    print(f"[RUN ] {bench}", flush=True)
    result = summarize(
        trace_path,
        access_mode=access_mode,
        include_unmatched=include_unmatched,
        include_ptw=include_ptw,
    )
    out_path = os.path.join(out_dir, f"{bench}_data_structure_access_summary.csv")
    result.to_csv(out_path, index=False)
    return bench, out_path, len(result)


def summarize(
    trace_path: str,
    access_mode: str = "all",
    include_unmatched: bool = False,
    include_ptw: bool = False,
) -> pd.DataFrame:
    """
    access_mode:
      "all"             — every row (default)
      "rdma-only"       — rdma == 1
      "rdma-l2miss-only" — rdma == 1 AND type in L2_MISS_TYPES (=={2, 5})
    """
    if access_mode not in ("all", "rdma-only", "rdma-l2miss-only"):
        raise ValueError(f"unknown access_mode: {access_mode!r}")

    # access_count is summed per chunk; cacheline / chiplet sharing needs unique
    # (data_id, addr, chiplet) triples, so we collect chunk-local unique triples
    # and dedupe once at the end (much cheaper than concatenating raw rows).
    access_counts = []      # per-chunk access counts (Series indexed by data_id)
    triple_chunks = []      # per-chunk unique (data_id, addr, chiplet)

    for chunk in pd.read_csv(trace_path, skipinitialspace=True, chunksize=CHUNK_SIZE):
        if access_mode == "rdma-only":
            chunk = chunk[chunk["rdma"].astype(bool)]
        elif access_mode == "rdma-l2miss-only":
            chunk = chunk[chunk["rdma"].astype(bool) & chunk["type"].isin(L2_MISS_TYPES)]
        if not include_ptw and "is_ptw" in chunk.columns:
            chunk = chunk[chunk["is_ptw"].astype(bool) == False]
        if not include_unmatched:
            chunk = chunk[chunk["data_id"] != -1]
        if chunk.empty:
            continue
        access_counts.append(chunk.groupby("data_id").size())
        triple_chunks.append(chunk[["data_id", "addr", "chiplet"]].drop_duplicates())

    empty_cols = [
        "data_id", "access_count", "cacheline_count",
        "avg_access_per_cl", "avg_chiplet_per_cl",
    ]
    if not triple_chunks:
        return pd.DataFrame(columns=empty_cols)

    # Total access per data_id
    acc = pd.concat(access_counts).groupby(level=0).sum().rename("access_count")

    # Dedupe triples across chunks → one row per (data_id, addr, chiplet) globally
    triples = pd.concat(triple_chunks, ignore_index=True).drop_duplicates()

    # For each (data_id, addr): how many distinct chiplets touched this line
    per_cl_chiplets = triples.groupby(["data_id", "addr"]).size()

    # Aggregate to data_id level
    per_data = per_cl_chiplets.groupby(level=0).agg(
        cacheline_count="size",
        avg_chiplet_per_cl="mean",
    )

    result = per_data.join(acc).reset_index()
    result["avg_access_per_cl"] = result["access_count"] / result["cacheline_count"]
    result = result.sort_values("data_id")
    return result[empty_cols]


def _option_tag(access_mode: str, include_unmatched: bool, include_ptw: bool) -> str:
    """
    Build a subdirectory name encoding which filtering options diverge from
    default. Keeps results from different option combos in separate folders so
    downstream consumers can't mix them by accident.

      access_mode=all,    no unmatched, no ptw → "default"
      access_mode=rdma-only                    → "rdma-only"
      access_mode=rdma-l2miss-only             → "rdma-l2miss-only"
      --include-unmatched                       → adds "with-unmatched"
      --include-ptw                             → adds "with-ptw"
      combinations are joined with "_" in the above order
        (e.g. "rdma-l2miss-only_with-ptw").
    """
    parts = []
    if access_mode != "all":
        parts.append(access_mode)
    if include_unmatched:
        parts.append("with-unmatched")
    if include_ptw:
        parts.append("with-ptw")
    return "_".join(parts) if parts else "default"


def main() -> None:
    parser = argparse.ArgumentParser(
        description=__doc__,
        formatter_class=argparse.RawDescriptionHelpFormatter,
    )
    parser.add_argument("--trace-id", required=True, help="trace run id (<config>/<timestamp> or <timestamp>) — resolves to ~/mgvm/results/<config>/<timestamp>/memory-traces/")
    parser.add_argument("--benchmark", default=None, help="process a single benchmark only")
    access_group = parser.add_mutually_exclusive_group()
    access_group.add_argument("--rdma-only", dest="access_mode", action="store_const", const="rdma-only",
                              help="count only RDMA accesses (rdma == 1)")
    access_group.add_argument("--rdma-l2miss-only", dest="access_mode", action="store_const", const="rdma-l2miss-only",
                              help="count only RDMA-served L2 misses (rdma == 1 AND type in {2, 5})")
    parser.set_defaults(access_mode="all")
    parser.add_argument("--include-unmatched", action="store_true", help="include data_id == -1 rows")
    parser.add_argument("--include-ptw", action="store_true", help="include is_ptw == 1 rows (default: exclude page-table-walk accesses)")
    parser.add_argument("--out-dir", default=f"{username}/mgvm/analyze/_out/data-structure/data-structure-metrics/access-summary")
    args = parser.parse_args()

    trace_dir = str(memory_trace_dir(args.trace_id))
    if not os.path.isdir(trace_dir):
        print(f"ERROR: trace dir not found: {trace_dir}", file=sys.stderr)
        sys.exit(1)

    trace_map = _find_traces(trace_dir)

    targets = [args.benchmark] if args.benchmark else PARAMS_BENCHMARKS
    out_dir = os.path.join(args.out_dir, _option_tag(args.access_mode, args.include_unmatched, args.include_ptw))
    os.makedirs(out_dir, exist_ok=True)
    print(f"Output dir: {out_dir}")

    work = {}
    for bench in targets:
        if bench not in trace_map:
            print(f"[SKIP] {bench}: no trace file in {trace_dir}")
            continue
        work[bench] = trace_map[bench]

    with ProcessPoolExecutor(max_workers=len(work)) as executor:
        items = [(bench, path, args.access_mode, args.include_unmatched, args.include_ptw, out_dir) for bench, path in work.items()]
        futures = {executor.submit(_worker, item): item[0] for item in items}
        for future in as_completed(futures):
            bench = futures[future]
            try:
                _, out_path, n = future.result()
                print(f"[DONE] {bench}: → {out_path}  ({n} data_ids)")
            except Exception as exc:
                print(f"[ERROR] {bench}: {exc}", file=sys.stderr)


if __name__ == "__main__":
    main()
