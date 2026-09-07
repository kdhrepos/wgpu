import os
import sys
import pandas as pd
import numpy as np

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


def _generate_dynamic_bins(max_chiplets):
    """
    최대 칩렛 수를 기반으로 동적으로 bin 경계와 라벨 생성.
    칩렛 개수가 다양한 시스템을 지원하기 위해 로그 스케일 기반 binning.
    
    Args:
        max_chiplets: 데이터에서 관찰된 최대 칩렛 공유 개수
        
    Returns:
        (bins, labels): bin 경계 리스트, 라벨 리스트
    """
    if max_chiplets <= 1:
        return [1, np.inf], ['1']
    
    bins = [1]
    labels = ['1']
    
    power = 2
    while power <= max_chiplets:
        bins.append(power)
        next_power = power * 2
        
        # Calculate max value in this bin
        bin_max = next_power - 1
        
        if next_power > max_chiplets:
            labels.append(f'{power}+')
            bins.append(np.inf)
            break
        else:
            if power == bin_max:
                labels.append(f'{power}')
            else:
                labels.append(f'{power}-{bin_max}')
        
        power = next_power
        
    return bins, labels


def process_benchmark(benchmark, args):
    trace_file = f"{trace_dir}/{benchmark}_mesh_{xDim}x{yDim}_memory_trace.csv"

    if not os.path.exists(trace_file):
        return None
    
    print(f"[{benchmark}] Analyzing chiplet sharing & access counts...")
    
    try:
        # 필요한 컬럼만 로드
        usecols = ['addr', 'chiplet', 'rdma']
        if args.read_only:
            usecols.append('rw')
        if args.rdma_type:
            usecols.append('type')

        df = pd.read_csv(trace_file, skipinitialspace=True)
        df = df.drop_duplicates()
        df = df[usecols]

        # 읽기 전용 주소 목록: 전체 트레이스(rdma 포함 모든 접근) 기준으로
        # 쓰기(rw != 0)가 한 번이라도 발생한 주소를 미리 추출
        written_addrs = None
        if args.read_only:
            tmp = df.groupby('addr')['rw'].max().reset_index()
            written_addrs = tmp[tmp['rw'] != 0]['addr'].unique()

        # 기본 필터: RDMA 요청만
        df = df[df['rdma']==True]

        # 읽기 전용 주소만 (쓰기가 한 번도 없었던 주소)
        if args.read_only:
            df = df[~df['addr'].isin(written_addrs)]

        # rdma==True 중에서 type이 2, 5인 요청만
        if args.rdma_type:
            df = df[df['type'].isin([2, 5])]

        if args.group_size != 0:
            group_size = args.group_size
            df['addr'] = df['addr'] // (64 * group_size)
        
        if df.empty:
            return None

        # 1. 주소별 고유 칩렛 개수 및 접근 횟수 집계
        shared_chiplets = df.groupby('addr').agg({
            'addr': 'count',    # 해당 주소로의 총 접근 횟수
            'chiplet': 'nunique' # 공유된 칩렛 수
        }).rename(columns={
            'addr': 'access_count', 
            'chiplet': 'unique_chiplets'
        }).reset_index()

        max_chiplets = shared_chiplets['unique_chiplets'].max()
        total_accesses = shared_chiplets['access_count'].sum()

        # 2. 칩렛 개수별 구간(Bins) 동적 설정
        # max_chiplets 값에 따라 자동으로 bin 경계와 라벨 생성
        bins, labels = _generate_dynamic_bins(max_chiplets)
        
        # 3. 구간 분류
        shared_chiplets['range'] = pd.cut(shared_chiplets['unique_chiplets'], bins=bins, labels=labels, right=False)
        
        # --- (A) 주소(캐시 라인) 개수 기준 분포 ---
        addr_stats_pct = shared_chiplets['range'].value_counts(normalize=True).sort_index() * 100

        # --- (B) 접근 횟수(Access Count) 기준 분포 ---
        # 각 구간에 속하는 캐시 라인들의 access_count를 모두 합산
        access_stats_sum = shared_chiplets.groupby('range', observed=False)['access_count'].sum()
        access_stats_pct = (access_stats_sum / total_accesses) * 100

        # 4. 결과 사전 생성
        res = {
            'benchmark': benchmark,
            'total_unique_addrs': len(shared_chiplets),
            'total_rdma_accesses': total_accesses,
            'max_sharing_degree': max_chiplets
        }

        # 라벨별로 주소 비율, 접근 합계, 접근 비율을 저장 (동적 라벨 사용)
        for label in labels:
            res[f'Access_%_{label}'] = access_stats_pct.get(label, 0.0)
            res[f'Addr_Count_%_{label}'] = addr_stats_pct.get(label, 0.0)
            res[f'Access_Sum_{label}'] = access_stats_sum.get(label, 0)

        return res

    except Exception as e:
        print(f"[{benchmark}] Error: {e}")
        return None


if __name__ == "__main__":
    import argparse
    parser = argparse.ArgumentParser(description="Analyze chiplet sharing distribution from memory traces.")
    parser.add_argument('run_id', nargs='?', default='latest',
                        help="run-id (latest, <config>, <config>/<timestamp>, ...)")
    parser.add_argument('--read-only', action='store_true', help='Whether to analyze read-only accesses (default: False)')
    parser.add_argument('--group-size', type=int, default=0, help='Group size for address grouping (0 means no grouping)')
    parser.add_argument('--rdma-type', action='store_true', help='Among rdma==True, keep only requests with type in {2, 5} (default: False)')
    args = parser.parse_args()

    with ProcessPoolExecutor() as executor:
        results = list(executor.map(process_benchmark, benchmarks, repeat(args)))

    results = [r for r in results if r is not None]

    if results:
        result_dir = f"{username}/mgvm/analyze/_out/top-n/{date}"
        os.makedirs(result_dir, exist_ok=True)

        results_df = pd.DataFrame(results)
        
        # 컬럼 순서 재배치: addr_count 끼리, access_sum 끼리, access_% 끼리 그룹화
        base_cols = ['benchmark', 'total_unique_addrs', 'total_rdma_accesses', 'max_sharing_degree']
        access_pct_cols = [c for c in results_df.columns if c.startswith('Access_%_')]
        addr_cols = [c for c in results_df.columns if c.startswith('Addr_Count_%_')]
        access_sum_cols = [c for c in results_df.columns if c.startswith('Access_Sum_')]
        
        ordered_cols = base_cols + addr_cols + access_sum_cols + access_pct_cols
        ordered_cols = [c for c in ordered_cols if c in results_df.columns]
        results_df = results_df[ordered_cols]
        
        # Excel 저장 (xlsx)
        base_filename = "t_chiplet_sharing_distribution"
        if args.read_only:
            base_filename += "_readonly"
        if args.rdma_type:
            base_filename += "_true_rdma"
        if args.group_size != 0:
            base_filename += f"_{args.group_size}"
        
        output_path = f"{result_dir}/{base_filename}.xlsx"
        
        results_df.to_excel(output_path, index=False)
        
        print("\n--- Integrated Sharing & Access Distribution ---")
        # 동적으로 생성된 라벨 기반의 주요 컬럼만 선택해서 출력
        base_cols = ['benchmark', 'total_unique_addrs', 'total_rdma_accesses', 'max_sharing_degree']
        # Addr_Count와 Access 컬럼들 중 첫 번째와 마지막만 선택
        addr_cols = [c for c in results_df.columns if c.startswith('Addr_Count_%_')]
        access_cols = [c for c in results_df.columns if c.startswith('Access_%_')]
        
        if addr_cols and access_cols:
            # 첫 번째와 마지막 컬럼만 요약 출력
            summary_cols = base_cols + [addr_cols[0], access_cols[0], addr_cols[-1], access_cols[-1]]
        else:
            summary_cols = base_cols
        
        existing_summary = [c for c in summary_cols if c in results_df.columns]
        print(results_df[existing_summary].to_string(index=False))
        
        print(f"\nFull detailed summary saved to: {output_path}")
    else:
        print("No valid results found. Check data and filters.")