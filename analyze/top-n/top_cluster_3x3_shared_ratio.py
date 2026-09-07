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

xDim=6
yDim=6

def process_benchmark(benchmark, args):
    trace_file = f"{trace_dir}/{benchmark}_mesh_{xDim}x{yDim}_memory_trace.csv"
    if not os.path.exists(trace_file):
        return None
    
    print(f"[{benchmark}] Analyzing cluster sharing degree (Addr & Access)...")
    
    try:
        # 1. 데이터 로드
        usecols = ['addr', 'chiplet', 'owner', 'rdma']
        if getattr(args, 'read_only', False):
            usecols.append('rw')
            
        df = pd.read_csv(trace_file, skipinitialspace=True)
        df = df.drop_duplicates()
        df = df[usecols]
        
        # 2. 클러스터 맵핑 설정
        clusters_3x3 = {
            'cluster_0_3x3': [0, 1, 2, 6, 7, 8, 12, 13, 14],
            'cluster_1_3x3': [3, 4, 5, 9, 10, 11, 15, 16, 17],
            'cluster_2_3x3': [18, 19, 20, 24, 25, 26, 30, 31, 32],
            'cluster_3_3x3': [21, 22, 23, 27, 28, 29, 33, 34, 35],
        }
        chiplet_to_cluster = {chiplet: cluster for cluster, chiplets in clusters_3x3.items() for chiplet in chiplets}
        
        df['cluster_3x3'] = df['chiplet'].map(chiplet_to_cluster).fillna('unknown')
        df['owner_cluster_3x3'] = df['owner'].map(chiplet_to_cluster).fillna('unknown')

        # 3. RDMA 및 원격 클러스터 접근 필터링 (동일 3x3 클러스터 외부 접근만)
        if getattr(args, 'read_only', False):
            # 전체 트레이스에서 쓰기(rw != 0)가 한 번이라도 발생한 주소 목록 추출
            written_addrs = df.groupby('addr')['rw'].max().reset_index()
            written_addrs = written_addrs[written_addrs['rw'] != 0]['addr'].unique()
            rdma_df = df[(df['rdma'] == True) & (~df['addr'].isin(written_addrs))]
        else:
            rdma_df = df[df['rdma'] == True]
            
        remote_cluster_df = rdma_df[rdma_df['cluster_3x3'] != rdma_df['owner_cluster_3x3']].copy()
        
        if remote_cluster_df.empty:
            return None
        
        # 4. 주소 그룹화 (64B * group_size 단위)
        if args.group_size != 0:
            remote_cluster_df['addr'] = remote_cluster_df['addr'] // (64 * args.group_size)

        # 5. 주소별 메트릭 집계 (공유 클러스터 수 & 접근 횟수)
        addr_metrics = remote_cluster_df.groupby("addr").agg(
            shared_clusters_count=('cluster_3x3', 'nunique'),
            access_count=('addr', 'count')
        ).reset_index()

        # 6. 통계 산출
        # (A) 주소 개수 기준 분포
        addr_dist = addr_metrics['shared_clusters_count'].value_counts()
        total_addr_count = len(addr_metrics)

        # (B) 접근 횟수 기준 분포
        access_dist = addr_metrics.groupby('shared_clusters_count')['access_count'].sum()
        total_access_count = access_dist.sum()
        
        res = {
            'benchmark': benchmark,
            'total_remote_addrs': int(total_addr_count),
            'total_remote_accesses': int(total_access_count)
        }
        
        # 1~4개 클러스터 공유 범위에 대해 루프
        for i in range(1, 5):
            # 주소 비율 (%) 
            addr_cnt = addr_dist.get(i, 0)
            res[f'Addr_%_{i}Clus_3x3'] = round((addr_cnt / total_addr_count * 100), 2) if total_addr_count > 0 else 0.0
            
            # 접근 비율 (%)
            acc_cnt = access_dist.get(i, 0)
            res[f'Access_%_{i}Clus_3x3'] = round((acc_cnt / total_access_count * 100), 2) if total_access_count > 0 else 0.0

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
    parser.add_argument('--read-only', action='store_true', help='Analyze only read-only (rw=0) data')
    args = parser.parse_args()

    with ProcessPoolExecutor() as executor:
        results = list(executor.map(process_benchmark, benchmarks, repeat(args)))

    results = [r for r in results if r is not None]

    if results:
        results_df = pd.DataFrame(results)
        
        # 컬럼 순서 재배치 (가독성: 주소 비율 먼저, 그 다음 접근 비율)
        addr_cols = [f'Addr_%_{i}Clus_3x3' for i in range(1, 5)]
        access_cols = [f'Access_%_{i}Clus_3x3' for i in range(1, 5)]
        cols = ['benchmark', 'total_remote_addrs', 'total_remote_accesses'] + addr_cols + access_cols
        
        results_df = results_df[cols]
        
        result_dir = f"{username}/mgvm/analyze/_out/top-n/{date}"
        os.makedirs(result_dir, exist_ok=True)
        
        # 파일 저장 로직
        base_filename = "t_cluster_sharing_3x3"
        if args.read_only:
            base_filename += "_readonly"
        if args.group_size != 0:
            base_filename += f"_{args.group_size}"
            
        output_path = f"{result_dir}/{base_filename}.xlsx"
        results_df.to_excel(output_path, index=False)
        
        print("\n" + "="*100)
        print(f"{'Cluster Sharing Degree: Address vs Access Distribution (%)':^100}")
        print("="*100)
        
        # 요약 출력을 위해 1, 2, 4 클러스터 결과만 터미널에 표시 (가로 폭 고려)
        summary_cols = ['benchmark']
        for i in sorted(list(set([1, 2, 4]))):
            summary_cols.append(f'Addr_%_{i}Clus_3x3')
            summary_cols.append(f'Access_%_{i}Clus_3x3')
            
        print(results_df[summary_cols].to_string(index=False))
        print("="*100)
        print(f"Detailed statistics saved to: {output_path}")