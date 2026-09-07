import os
import sys
import pandas as pd

parent_dir = os.path.abspath(os.path.join(os.path.dirname(__file__), "../../"))

if parent_dir not in sys.path:
    sys.path.append(parent_dir)

from concurrent.futures import ProcessPoolExecutor
from itertools import repeat
from params import benchmarks

username = os.path.expanduser('~')
# run_id 결정: argv 첫 positional, 없으면 'latest'
_run_id = next((a for a in sys.argv[1:] if not a.startswith('-')), 'latest')
from _paths import run_root as _run_root, memory_trace_dir as _mtd, run_tag as _run_tag
date = _run_tag(_run_id)
trace_dir = str(_mtd(_run_id))

xDim = 4
yDim = 4

def process_benchmark(benchmark, args):
    trace_file = f"{trace_dir}/{benchmark}_mesh_{xDim}x{yDim}_memory_trace.csv"

    if not os.path.exists(trace_file):
        return None
    
    print(f"[{benchmark}] Analyzing overall Read/Write Access Ratio...")
    
    try:
        # Load only 'rw' column to count total read and write accesses
        df = pd.read_csv(trace_file, skipinitialspace=True)
        df = df.drop_duplicates()
        df = df[['rw']]
        
        total_accesses = len(df)
        if total_accesses == 0:
            return None

        write_accesses = int(df['rw'].sum())
        read_accesses = total_accesses - write_accesses

        res_dict = {
            'benchmark': benchmark,
            'total_accesses': total_accesses,
            'read_accesses': read_accesses,
            'write_accesses': write_accesses,
            'read_ratio_%': round(read_accesses / total_accesses * 100, 2),
            'write_ratio_%': round(write_accesses / total_accesses * 100, 2)
        }

        return pd.DataFrame([res_dict])

    except Exception as e:
        print(f"[{benchmark}] Error: {e}")
        return None

if __name__ == "__main__":
    import argparse
    parser = argparse.ArgumentParser(description="Analyze Overall Read/Write Access Ratio.")
    parser.add_argument('run_id', nargs='?', default='latest',
                        help="run-id (latest, <config>, <config>/<timestamp>, ...)")
    args = parser.parse_args()

    with ProcessPoolExecutor() as executor:
        results = list(executor.map(process_benchmark, benchmarks, repeat(args)))

    results = [r for r in results if r is not None]

    if results:
        final_df = pd.concat(results, ignore_index=True)
        
        result_dir = f"{username}/mgvm/analyze/_out/access-count/{date}"
        os.makedirs(result_dir, exist_ok=True)
        
        # Only saving to xlsx as requested
        output_path = f"{result_dir}/t_access_rw_ratio.xlsx"

        final_df.to_excel(output_path, index=False)
        
        print("\n" + "="*80)
        print(f"{'Overall Read/Write Access Ratio':^80}")
        print("="*80)
        print(final_df.to_string(index=False))
        print("="*80)
        print(f"Saved results to:\n- {output_path}")
    else:
        print("No results generated.")
