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

xDim=4
yDim=4


def _generate_dynamic_bins(max_clusters):
    """
    최대 클러스터 수를 기반으로 동적으로 bin 경계와 라벨 생성.
    """
    if max_clusters <= 1:
        return [1, np.inf], ['1']
    
    bins = [1]
    labels = ['1']
    
    power = 2
    while power <= max_clusters:
        bins.append(power)
        next_power = power * 2
        
        # Calculate max value in this bin
        bin_max = next_power - 1
        
        if next_power > max_clusters:
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
    
    print(f"[{benchmark}] Analyzing cluster sharing by PC...")
    
    try:
        # NOTE: pc 정보를 포함합니다.
        df = pd.read_csv(trace_file,
                        skipinitialspace=True)
        df = df.drop_duplicates()
        df = df[['pc', 'addr', 'chiplet', 'rdma', 'owner', 'rw']]
        
        # RDMA 요청 필터링 여부 (기존 방식과 동일하게 유지)
        df = df[df['rdma']==True].copy()

        # Mesh Topology에서의 Hop Count 계산 (Manhattan distance)
        df['src_x'] = df['chiplet'] % xDim
        df['src_y'] = df['chiplet'] // xDim
        df['dst_x'] = df['owner'] % xDim
        df['dst_y'] = df['owner'] // xDim
        df['hop_count'] = (df['src_x'] - df['dst_x']).abs() + (df['src_y'] - df['dst_y']).abs()

        if args.group_size != 0:
            group_size = args.group_size
            df['addr'] = df['addr'] // (64 * group_size)
        
        if df.empty:
            return None

        # 1. PC 및 addr 조합별로 클러스터 개수 접근 횟수 집계
        # 클러스터 ID 계산 (Chiplet 좌표 기반 클러스터링)
        # 예: 2x2 사이즈의 클러스터라고 하면 cluster_id = (df['src_x'] // 2) + (df['src_y'] // 2) * (xDim // 2)
        df['cluster_id'] = (df['src_x'] // 2) + (df['src_y'] // 2) * (xDim // 2)
        
        pc_addr_df = df.groupby(['pc', 'addr']).agg(
            access_count=('cluster_id', 'count'),     # 해당 주소로의 총 접근 횟수 (특정 PC 기준)
            unique_chiplets=('cluster_id', 'nunique') # 공유된 클러스터 수
        ).reset_index()

        if pc_addr_df.empty:
            return None

        max_chiplets = pc_addr_df['unique_chiplets'].max()
        bins, labels = _generate_dynamic_bins(max_chiplets)

        # 각 (pc, addr) 구간 분류
        pc_addr_df['range'] = pd.cut(pc_addr_df['unique_chiplets'], bins=bins, labels=labels, right=False)

        # 2. PC 단위로 요약 메트릭 생성
        pc_stats = pc_addr_df.groupby('pc').agg(
            total_addrs=('addr', 'count'),
            total_accesses=('access_count', 'sum'),
            avg_sharing=('unique_chiplets', 'mean'),
            max_sharing=('unique_chiplets', 'max')
        )
        
        # PC별 Hop Count 메트릭 생성
        pc_hop_stats = df.groupby('pc').agg(
            avg_hop_count=('hop_count', 'mean'),
            max_hop_count=('hop_count', 'max')
        )

        # PC별 Read/Write 메트릭 생성 (일반적으로 0: Read, 1: Write)
        pc_rw_stats = df.groupby('pc')['rw'].agg(
            read_count=lambda x: (x == 0).sum(),
            write_count=lambda x: (x == 1).sum()
        )
        total_rw = pc_rw_stats['read_count'] + pc_rw_stats['write_count']
        pc_rw_stats['read_ratio_%'] = (pc_rw_stats['read_count'] / total_rw * 100).fillna(0)
        pc_rw_stats['write_ratio_%'] = (pc_rw_stats['write_count'] / total_rw * 100).fillna(0)

        # 3. PC 별 분포 계산 (Address Count 기준 - %)
        addr_dist = pd.crosstab(pc_addr_df['pc'], pc_addr_df['range'], dropna=False)
        addr_dist_pct = addr_dist.div(addr_dist.sum(axis=1), axis=0) * 100
        addr_dist_pct.columns = [f'Addr_%_{label}' for label in labels]

        # 4. PC 별 분포 계산 (Access Count 기준 - %)
        acc_dist = pc_addr_df.pivot_table(index='pc', columns='range', values='access_count', aggfunc='sum', observed=False).fillna(0)
        acc_dist_pct = acc_dist.div(acc_dist.sum(axis=1), axis=0) * 100
        acc_dist_pct.columns = [f'Access_%_{label}' for label in labels]

        # 5. 병합 (통계 + hop count + RW 비율 + 주소 분포 + 접근 분포)
        res_df = pc_stats.join(pc_hop_stats).join(pc_rw_stats).join(addr_dist_pct).join(acc_dist_pct).reset_index()
        res_df.insert(0, 'benchmark', benchmark)

        # 접근이 많은 PC 순으로 정렬
        res_df = res_df.sort_values(by='total_accesses', ascending=False)
        
        # 반올림
        numeric_cols = res_df.select_dtypes(include=[np.number]).columns
        res_df[numeric_cols] = res_df[numeric_cols].round(2)

        return res_df

    except Exception as e:
        print(f"[{benchmark}] Error: {e}")
        return None


if __name__ == "__main__":
    import argparse
    parser = argparse.ArgumentParser(description="Analyze cluster sharing distribution per PC.")
    parser.add_argument('run_id', nargs='?', default='latest',
                        help="run-id (latest, <config>, <config>/<timestamp>, ...)")
    parser.add_argument('--group-size', type=int, default=1, help='Group size for address grouping (0 means no grouping)')
    args = parser.parse_args()

    with ProcessPoolExecutor() as executor:
        results = list(executor.map(process_benchmark, benchmarks, repeat(args)))

    results = [r for r in results if r is not None]

    if results:
        # 여러 벤치마크 데이터를 하나로 통합
        final_df = pd.concat(results, ignore_index=True)
        
        result_dir = f"{username}/mgvm/analyze/_out/pc-stats/{date}"
        os.makedirs(result_dir, exist_ok=True)
        
        suffix = f"_{args.group_size}"
        output_path = f"{result_dir}/t_pc_cluster_sharing_distribution{suffix}.csv"

        final_df.to_csv(output_path, index=False)
        final_df.to_excel(output_path.replace(".csv", ".xlsx"), index=False)
        
        print("\n" + "="*80)
        print(f"{'Per-PC Cluster Sharing & Access Distribution (Top 10 rows shown)':^80}")
        print("="*80)
        
        # 터미널용 요약: 접근 횟수가 가장 많은 상위 10개 행만 출력 (모든 벤치마크 통합 내)
        summary_df = final_df.sort_values(by='total_accesses', ascending=False).head(10)
        
        # 일부 컬럼만 선택 출력 (hop, r/w 포함)
        base_cols = ['benchmark', 'pc', 'total_addrs', 'total_accesses', 'max_sharing', 'avg_hop_count', 'max_hop_count', 'read_ratio_%', 'write_ratio_%']
        addr_cols = [c for c in final_df.columns if c.startswith('Addr_%_')]
        access_cols = [c for c in final_df.columns if c.startswith('Access_%_')]
        
        if addr_cols and access_cols:
            summary_cols = base_cols + [addr_cols[-1], access_cols[-1]] # 17+ 같이 제일 높은 공유의 경우
        else:
            summary_cols = base_cols
        
        existing_summary = [c for c in summary_cols if c in final_df.columns]
        print(summary_df[existing_summary].to_string(index=False))
        
        print("="*80)
        print(f"Full detailed summary saved to: {output_path}")
    else:
        print("No valid results found. Check data and filters.")