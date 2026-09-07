import pandas as pd
import sys
import numpy as np
import os

from concurrent.futures import ProcessPoolExecutor
from itertools import repeat, combinations
from params import benchmarks

username = os.path.expanduser('~')
# run_id 결정: argv 첫 positional, 없으면 'latest'
_run_id = next((a for a in sys.argv[1:] if not a.startswith('-')), 'latest')
from _paths import run_root as _run_root, memory_trace_dir as _mtd, run_tag as _run_tag
date = _run_tag(_run_id)
trace_dir = str(_mtd(_run_id))

# 1. 6x6 Mesh 거리 계산 함수 (ID만 사용)
def calculate_hop(id1, id2):
    # x = id % 6, y = id // 6
    return abs(id1 % 6 - id2 % 6) + abs(id1 // 6 - id2 // 6)


# 2. 스크린샷의 데이터프레임을 'df_agg'라고 가정합니다.
def process_group_distances(chiplet_list):
    if len(chiplet_list) < 2:
        return 0, 0 # 칩렛이 하나면 거리는 0
    
    # 칩렛 ID들 간의 모든 가능한 쌍(Pair) 생성
    pairs = list(combinations(chiplet_list, 2))
    
    # 모든 쌍에 대해 Hop Count 계산
    dists = [calculate_hop(p1, p2) for p1, p2 in pairs]
    
    # 평균 거리와 최대 거리 반환
    return np.mean(dists), np.max(dists)


import pandas as pd
import numpy as np
from itertools import combinations

def calculate_hop(id1, id2):
    return abs(id1 % 6 - id2 % 6) + abs(id1 // 6 - id2 // 6)

def process_benchmark(benchmark, args):
    trace_file = f"{trace_dir}/{benchmark}_mesh_6x6_memory_trace.csv"
    if not os.path.exists(trace_file): return None
    
    print(f"[{benchmark}] Calculating Hop Count Distribution...")
    
    try:
        df = pd.read_csv(trace_file, skipinitialspace=True)
        df = df.drop_duplicates()
        df = df[['addr', 'chiplet', 'rdma']]
        df = df[df['rdma'] == True]
        if df.empty: return None

        # 1. 주소 그룹화
        divisor = 64 * (args.group_size if args.group_size > 0 else 1)
        df['group'] = df['addr'] // divisor

        # 2. 그룹별 유니크 칩렛 목록 및 접근 횟수 추출
        df_agg = df.groupby('group').agg({
            'chiplet': 'unique',
            'addr': 'count' # 해당 그룹의 총 접근 횟수
        }).rename(columns={'addr': 'access_count'}).reset_index()

        # 3. 공유 그룹만 필터링 (칩렛 2개 이상)
        shared_groups = df_agg[df_agg['chiplet'].apply(len) >= 2]
        
        # 4. Hop Count 분포 수집 (가중치 적용)
        # 모든 공유 쌍의 거리를 구하고, 해당 그룹의 access_count만큼 가중치를 둡니다.
        hop_distribution = {h: 0 for h in range(1, 11)} # 1~10 hop
        total_pair_accesses = 0

        for _, row in shared_groups.iterrows():
            chiplets = row['chiplet']
            count = row['access_count']
            
            # 그룹 내 모든 칩렛 쌍 생성
            pairs = list(combinations(chiplets, 2))
            for p1, p2 in pairs:
                h = calculate_hop(p1, p2)
                if h > 0:
                    hop_distribution[h] += count
                    total_pair_accesses += count

        # 5. 결과 사전 생성
        res = {'benchmark': benchmark}
        
        # 6. Hop Count별 비율 계산 (Percent)
        for h in range(1, 11):
            percentage = (hop_distribution[h] / total_pair_accesses * 100) if total_pair_accesses > 0 else 0
            res[f'Hop_{h}_Ratio'] = percentage

        return res

    except Exception as e:
        print(f"[{benchmark}] Error: {e}")
        return None


if __name__ == "__main__":
    import argparse
    from concurrent.futures import ProcessPoolExecutor
    from itertools import repeat

    parser = argparse.ArgumentParser(description="Analyze chiplet sharing hop count distribution.")
    parser.add_argument('run_id', nargs='?', default='latest',
                        help="run-id (latest, <config>, <config>/<timestamp>, ...)")
    parser.add_argument('--group-size', type=int, default=0, help='Group size for address grouping (0 means no grouping)')
    args = parser.parse_args()

    # 1. 병렬 처리 실행
    with ProcessPoolExecutor() as executor:
        results = list(executor.map(process_benchmark, benchmarks, repeat(args)))

    # 2. 유효한 결과 필터링
    results = [r for r in results if r is not None]

    if results:
        results_df = pd.DataFrame(results)
        
        # 3. 파일 저장 경로 설정
        output_name = "t_chiplet_sharing_hop"
        if args.group_size != 0:
            output_name += f"_{args.group_size}"
        output_path = f"{username}/mgvm/analyze/_out/top-n/{output_name}.csv"

        # 4. 저장 전 컬럼 순서 정렬 (Benchmark 우선, 그 뒤로 Hop 비율 순서대로)
        hop_cols = [f'Hop_{h}_Ratio' for h in range(1, 11) if f'Hop_{h}_Ratio' in results_df.columns]
        other_cols = [c for c in results_df.columns if c not in hop_cols and c != 'benchmark']
        results_df = results_df[['benchmark'] + hop_cols + other_cols]

        results_df.to_csv(output_path, index=False)
        
        # 5. 터미널 요약 출력
        print("\n" + "="*80)
        print(f"{'Benchmark Summary: Inter-Chiplet Hop Count Distribution (%)':^80}")
        print("="*80)
        
        # 가독성을 위해 근거리(1-2), 중거리(3-5), 원거리(6+)로 묶어서 요약 출력하거나
        # 주요 Hop 비율만 선택해서 보여줍니다.
        summary_cols = ['benchmark'] + [f'Hop_{h}_Ratio' for h in [1, 2, 5, 10] if f'Hop_{h}_Ratio' in results_df.columns]
        
        # 소수점 둘째자리까지 포맷팅하여 출력
        pd.options.display.float_format = '{:.2f}'.format
        print(results_df[summary_cols].to_string(index=False))
        
        print("-" * 80)
        print(f"Full distribution data (Hop 1-10) saved to: {output_path}")
        print("=" * 80)
    else:
        print("No valid results found. Please check trace files and benchmarks list.")