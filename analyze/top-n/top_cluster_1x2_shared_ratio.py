import os
import sys
import pandas as pd
import numpy as np

parent_dir = os.path.abspath(os.path.join(os.path.dirname(__file__), "../../"))

# 검색 경로에 추가
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


def process_benchmark(benchmark, args):
    trace_file = f"{trace_dir}/{benchmark}_mesh_6x6_memory_trace.csv"
    if not os.path.exists(trace_file):
        return None
    
    print(f"[{benchmark}] Analyzing 1x2 Cluster sharing (Addr & Access)...")
    
    try:
        # 1. 데이터 로드
        df = pd.read_csv(trace_file, skipinitialspace=True)
        df = df.drop_duplicates()
        df = df[['addr', 'chiplet', 'owner', 'rdma']]
        
        # 2. 1x2 클러스터 맵핑 설정 (가로 2개 묶음, 총 18개)
        clusters_1x2 = {
            'c0': [0, 1], 'c1': [2, 3], 'c2': [4, 5],
            'c3': [6, 7], 'c4': [8, 9], 'c5': [10, 11],
            'c6': [12, 13], 'c7': [14, 15], 'c8': [16, 17],
            'c9': [18, 19], 'c10': [20, 21], 'c11': [22, 23],
            'c12': [24, 25], 'c13': [26, 27], 'c14': [28, 29],
            'c15': [30, 31], 'c16': [32, 33], 'c17': [34, 35]
        }
        chiplet_to_cluster1x2 = {cp: c for c, ids in clusters_1x2.items() for cp in ids}
        
        df['cluster_1x2'] = df['chiplet'].map(chiplet_to_cluster1x2).fillna('unknown')
        df['owner_cluster_1x2'] = df['owner'].map(chiplet_to_cluster1x2).fillna('unknown')

        # 3. RDMA 및 원격 클러스터 접근 필터링 (동일 1x2 클러스터 외부 접근만)
        rdma_df = df[df['rdma'] == True]
        remote_cluster_df = rdma_df[rdma_df['cluster_1x2'] != rdma_df['owner_cluster_1x2']].copy()
        
        if remote_cluster_df.empty:
            return None
        
        # 4. 주소 그룹화
        if args.group_size != 0:
            remote_cluster_df['addr'] = remote_cluster_df['addr'] // (64 * args.group_size)

        # 5. 주소별 메트릭 집계
        addr_metrics = remote_cluster_df.groupby("addr").agg(
            shared_clusters_count=('cluster_1x2', 'nunique'),
            access_count=('addr', 'count')
        ).reset_index()

        # 6. 통계 산출
        total_addr_count = len(addr_metrics)
        total_access_count = addr_metrics['access_count'].sum()
        
        addr_dist = addr_metrics['shared_clusters_count'].value_counts()
        access_dist = addr_metrics.groupby('shared_clusters_count')['access_count'].sum()
        
        res = {
            'benchmark': benchmark,
            'total_remote_addrs': int(total_addr_count),
            'total_remote_accesses': int(total_access_count)
        }
        
        # 1x2 클러스터는 총 18개이므로 최대 18개까지 공유 가능
        for i in range(1, 19):
            # 주소 개수 기준 비율 (%)
            addr_pct = (addr_dist.get(i, 0) / total_addr_count * 100) if total_addr_count > 0 else 0.0
            res[f'Addr_%_{i}Clus_1x2'] = round(addr_pct, 2)
            
            # 접근 횟수 기준 비율 (%)
            acc_pct = (access_dist.get(i, 0) / total_access_count * 100) if total_access_count > 0 else 0.0
            res[f'Access_%_{i}Clus_1x2'] = round(acc_pct, 2)

        return res

    except Exception as e:
        print(f"[{benchmark}] Error: {e}")
        return None


if __name__ == "__main__":
    import argparse
    parser = argparse.ArgumentParser(description="Analyze chiplet sharing distribution from memory traces.")
    parser.add_argument('run_id', nargs='?', default='latest',
                        help="run-id (latest, <config>, <config>/<timestamp>, ...)")
    parser.add_argument('--group-size', type=int, default=0, help='Group size for address grouping (0 means no grouping)')
    args = parser.parse_args()

    with ProcessPoolExecutor() as executor:
        results = list(executor.map(process_benchmark, benchmarks, repeat(args)))

    results = [r for r in results if r is not None]

    if results:
        results_df = pd.DataFrame(results)
        
        # 컬럼 순서 재배치 (총 18개 클러스터)
        addr_cols = [f'Addr_%_{i}Clus_1x2' for i in range(1, 19)]
        access_cols = [f'Access_%_{i}Clus_1x2' for i in range(1, 19)]
        cols = ['benchmark', 'total_remote_addrs', 'total_remote_accesses'] + addr_cols + access_cols
        
        results_df = results_df[cols]
        
        # 파일 저장 로직
        suffix = f"_{args.group_size}" if args.group_size != 0 else ""
        output_path = f"{username}/mgvm/analyze/_out/top-n/t_cluster_sharing_1x2_{suffix}.csv"
        results_df.to_csv(output_path, index=False)
        
        print("\n" + "="*100)
        print(f"{'Cluster Sharing Degree: Address vs Access Distribution (%)':^100}")
        print("="*100)
        # 요약 출력을 위해 1, 2, 18 클러스터 결과만 터미널에 표시
        summary_cols = [
            'benchmark', 
            'Addr_%_1Clus_1x2', 'Access_%_1Clus_1x2',
            'Addr_%_2Clus_1x2', 'Access_%_2Clus_1x2',
            'Addr_%_18Clus_1x2', 'Access_%_18Clus_1x2'
        ]
        print(results_df[summary_cols].to_string(index=False))
        print("="*100)
        print(f"Detailed statistics saved to: {output_path}")