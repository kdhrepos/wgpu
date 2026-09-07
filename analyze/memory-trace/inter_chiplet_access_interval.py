import os
import sys
import pandas as pd
import numpy as np

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
    
    print(f"[{benchmark}] Analyzing Inter-Chiplet Access Intervals...")
    
    try:
        # Load time, addr, chiplet
        df = pd.read_csv(trace_file, skipinitialspace=True)
        df = df.drop_duplicates()
        df = df[['time', 'addr', 'chiplet', 'type', 'rdma']]
        
        df = df[(df['rdma']==True) & 
                (df['type'].isin([2, 5]))].copy()
        
        # 1GHz 가정: 1 seconds = 1,000,000,000 cycles
        # time을 cycle로 변환 (time 단위가 초(s)라고 가정)
        df['cycle'] = (df['time'] * 1e9).astype(np.int64)
        
        if args.group_size != 0:
            group_size = args.group_size
            df['addr'] = df['addr'] // (64 * group_size)
        
        if df.empty:
            return None

        # 1. 주소 및 사이클 순으로 정렬
        df = df.sort_values(by=['addr', 'cycle'])

        # 2. 동일한 캐시 라인(addr) 내에서 직전 접근 기록(chiplet, cycle) 구하기
        df['prev_chiplet'] = df.groupby('addr')['chiplet'].shift(1)
        df['prev_cycle'] = df.groupby('addr')['cycle'].shift(1)

        # 3. 칩렛이 변경된 순간만 필터링 (공유 데이터 접근 이전이 발생)
        inter_chiplet_df = df[df['prev_chiplet'].notna() & (df['chiplet'] != df['prev_chiplet'])].copy()

        if inter_chiplet_df.empty:
            return None

        # 4. 시간 차이(Interval) 계산
        inter_chiplet_df['interval'] = inter_chiplet_df['cycle'] - inter_chiplet_df['prev_cycle']

        intervals = inter_chiplet_df['interval']
        
        res_dict = {
            'benchmark': benchmark,
            'total_chiplet_switches': len(intervals),
            'mean_interval': round(intervals.mean(), 2),
            'median_interval': round(intervals.median(), 2),
            'min_interval': intervals.min(),
            'max_interval': intervals.max(),
            'interval_<100_cycles_%': round((intervals < 100).sum() / len(intervals) * 100, 2),
            'interval_100to1K_%': round(((intervals >= 100) & (intervals < 1000)).sum() / len(intervals) * 100, 2),
            'interval_1Kto10K_%': round(((intervals >= 1000) & (intervals < 10000)).sum() / len(intervals) * 100, 2),
            'interval_>10K_%': round((intervals >= 10000).sum() / len(intervals) * 100, 2)
        }
        
        return pd.DataFrame([res_dict])

    except Exception as e:
        print(f"[{benchmark}] Error: {e}")
        return None

if __name__ == "__main__":
    import argparse
    parser = argparse.ArgumentParser(description="Analyze time interval between accesses from different chiplets to shared data.")
    parser.add_argument('run_id', nargs='?', default='latest',
                        help="run-id (latest, <config>, <config>/<timestamp>, ...)")
    parser.add_argument('--group-size', type=int, default=1, help='Group size for address grouping')
    args = parser.parse_args()

    with ProcessPoolExecutor() as executor:
        results = list(executor.map(process_benchmark, benchmarks, repeat(args)))

    results = [r for r in results if r is not None]

    if results:
        final_df = pd.concat(results, ignore_index=True)
        
        result_dir = f"{username}/mgvm/analyze/_out/memory-trace/{date}"
        os.makedirs(result_dir, exist_ok=True)
        
        suffix = f"_{args.group_size}" if args.group_size != 0 else ""
        output_path = f"{result_dir}/t_inter_chiplet_access_interval{suffix}.xlsx"

        final_df.to_excel(output_path, index=False)
        
        print("\n" + "="*80)
        print(f"{'Inter-Chiplet Access Interval For Shared Data':^80}")
        print("="*80)
        print(final_df.to_string(index=False))
        print("="*80)
        print(f"Saved results to: {output_path}")
    else:
        print("No results generated.")
