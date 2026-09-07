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

import memory_footprint as mf

username = os.path.expanduser('~')

# run_id 결정: argv 첫 positional, 없으면 'latest'
_run_id = next((a for a in sys.argv[1:] if not a.startswith('-')), 'latest')
from _paths import run_root as _run_root, memory_trace_dir as _mtd, run_tag as _run_tag
date = _run_tag(_run_id)
trace_dir = str(_mtd(_run_id))

xDim = 6
yDim = 6
num_chiplets = xDim * yDim


def _init_worker():
    mf.PTE_FILTER = True
    mf.trace_dir = trace_dir
    mf.xDim = xDim
    mf.yDim = yDim
    mf.num_chiplets = num_chiplets


if __name__ == "__main__":
    import argparse as _ap
    _p = _ap.ArgumentParser(add_help=False)
    _p.add_argument('run_id', nargs='?', default='latest')
    _p.parse_known_args()

    print(f"Target Directory: {trace_dir}")

    mf.PTE_FILTER = True
    mf.trace_dir = trace_dir
    mf.xDim = xDim
    mf.yDim = yDim
    mf.num_chiplets = num_chiplets

    results = []
    max_workers = int(os.environ.get("FOOTPRINT_WORKERS", "4"))
    with ProcessPoolExecutor(max_workers=max_workers, initializer=_init_worker) as executor:
        for result in executor.map(mf.process_benchmark, benchmarks):
            if result is not None:
                results.append(result)

    results_df = pd.DataFrame(results).sort_values("benchmark")

    result_dir = f"{username}/mgvm/analyze/_out/memory-trace/{date}"
    os.makedirs(result_dir, exist_ok=True)

    output_path = f"{result_dir}/memory_footprint_pte.xlsx"

    with pd.ExcelWriter(output_path, engine="openpyxl") as writer:
        results_df.to_excel(writer, sheet_name="summary", index=False)

    print("\n--- Analysis Summary ---")
    print(results_df)
    print(f"\nSummary saved to: {output_path}")
