import pandas as pd
import sys
import os
from concurrent.futures import ProcessPoolExecutor
from params import benchmarks

username = os.path.expanduser('~')
# run_id 결정: argv 첫 positional, 없으면 'latest'
_run_id = next((a for a in sys.argv[1:] if not a.startswith('-')), 'latest')
from _paths import run_root as _run_root, memory_trace_dir as _mtd, run_tag as _run_tag
date = _run_tag(_run_id)
trace_dir = str(_mtd(_run_id))

def process_benchmark(benchmark):
    """
    개별 벤치마크 파일을 처리하는 함수 (병렬 실행 대상)
    """
    trace_file = f"{trace_dir}/{benchmark}_mesh_6x6_memory_trace.csv"

    if not os.path.exists(trace_file):
        print(f"No {trace_file}, skip")
        return None
    
    print(f"Processing {benchmark}...")
    
    # 데이터 로드
    df = pd.read_csv(trace_file, skipinitialspace=True)
    df = df.drop_duplicates()

    w = 6
    s_x, s_y = df['chiplet'] % w, df['chiplet'] // w
    d_x, d_y = df['owner'] % w, df['owner'] // w
    df['hop_count'] = (d_x - s_x).abs() + (d_y - s_y).abs()

    # 쓰기 발생 주소 미리 추출 (set)
    is_rw_addr = df.groupby('addr')['rw'].transform('max') == 1
    rw_addrs = set(df.loc[is_rw_addr, 'addr'])

    def get_stats(grouped_df):
        if grouped_df.empty:
            return 0, 0, 0, 0, 0.0, 0.0, 0  # 마지막 0은 hop_sum
        
        total_counts = grouped_df['count'].sum()
        total_mean = grouped_df['count'].mean()
        total_hop_sum = grouped_df['hop_sum'].sum() # 전체 hop 합계
        
        cutoff = max(1, int(len(grouped_df) * 0.10))
        top_10_df = grouped_df.head(cutoff)
        
        top_10_counts = top_10_df['count'].sum()
        top_10_mean = top_10_df['count'].mean()
        top_10_hop_sum = top_10_df['hop_sum'].sum() # 상위 10%의 hop 합계

        # RW/RO Ratio
        top_10_addrs = top_10_df['addr']
        rw_count = top_10_addrs.isin(rw_addrs).sum()
        rw_ratio = rw_count / len(top_10_addrs)
        
        return total_counts, total_mean, top_10_counts, top_10_mean, rw_ratio, 1.0 - rw_ratio, total_hop_sum, top_10_hop_sum

    # 1. Total Analysis
    total_counts_df = df.groupby('addr').agg(
        count=('addr', 'size'), 
        hop_sum=('hop_count', 'sum')).reset_index().sort_values(by='count', ascending=False)
    t_sum, t_mean, t10_sum, t10_mean, t10_rw, t10_ro, t_hop, t10_hop = get_stats(total_counts_df)

    # 2. RDMA Analysis
    rdma_df = df[df['rdma'] == True]
    rdma_counts_df = rdma_df.groupby('addr').agg(
        count=('addr', 'size'), 
        hop_sum=('hop_count', 'sum')).reset_index().sort_values(by='count', ascending=False)
    r_sum, r_mean, r10_sum, r10_mean, r10_rw, r10_ro, r_hop, r10_hop = get_stats(rdma_counts_df)

    # 3. Local Analysis
    local_df = df[df['rdma'] == False]
    local_counts_df = local_df.groupby('addr').agg(
        count=('addr', 'size'), 
        hop_sum=('hop_count', 'sum')).reset_index().sort_values(by='count', ascending=False)
    l_sum, l_mean, l10_sum, l10_mean, l10_rw, l10_ro, _, _ = get_stats(local_counts_df)

    remote_access_df = df[(df['rdma']==True) & 
                          ((df['type']==2) | 
                          (df['type']==5))]
    remote_access_counts_df = remote_access_df.groupby('addr').agg(
        count=('addr', 'size'),
        hop_sum=('hop_count', 'sum')).reset_index().sort_values(by='count', ascending=False)
    ra_sum, ra_mean, ra10_sum, ra10_mean, ra10_rw, ra10_ro, ra_hop, ra10_hop = get_stats(remote_access_counts_df)

    return {
        'benchmark': benchmark,
        'total_counts': t_sum, 'total_mean': t_mean, 'top_10_total_counts': t10_sum, 'top_10_mean': t10_mean,
        'total_hop_counts': t_hop, 'top_10_total_hop_counts': t10_hop,
        'total_top10_rw_ratio': t10_rw, 'total_top10_ro_ratio': t10_ro,

        'total_rdma_counts': r_sum, 'total_rdma_mean': r_mean, 'top_10_total_rdma_counts': r10_sum, 'top_10_rdma_mean': r10_mean,
        'total_rdma_hop_counts': r_hop, 'top_10_total_rdma_hop_counts': r10_hop,
        'rdma_top10_rw_ratio': r10_rw, 'rdma_top10_ro_ratio': r10_ro,

        'total_local_counts': l_sum, 'total_local_mean': l_mean, 'top_10_total_local_counts': l10_sum, 'top_10_local_mean': l10_mean,
        'local_top10_rw_ratio': l10_rw, 'local_top10_ro_ratio': l10_ro,

        # remote access (excluding read, write hit)
        'total_remote_access_counts': ra_sum, 'total_remote_access_mean': ra_mean, 'top_10_total_remote_access_counts': ra10_sum, 'top_10_remote_access_mean': ra10_mean,
        'total_remote_access_hop_counts': ra_hop, 'top_10_total_remote_access_hop_counts': ra10_hop,
        'remote_access_top10_rw_ratio': ra10_rw, 'remote_access_top10_ro_ratio': ra10_ro
        }


if __name__ == "__main__":
    import argparse as _ap
    _p = _ap.ArgumentParser(add_help=False)
    _p.add_argument('run_id', nargs='?', default='latest')
    _p.parse_known_args()

    print(f"Target Directory: {trace_dir}")

    # --- 병렬 처리 핵심 부분 ---
    # max_workers를 지정하지 않으면 현재 시스템의 CPU 코어 수만큼 프로세스를 생성합니다.
    with ProcessPoolExecutor() as executor:
        results = list(executor.map(process_benchmark, benchmarks))

    # None 결과값 필터링 (파일이 없었던 경우)
    results = [r for r in results if r is not None]

    # 결과 저장
    results_df = pd.DataFrame(results)
    output_path = f"{username}/mgvm/analyze/_out/top-n/top_10%_metrics.csv"
    results_df.to_csv(output_path, index=False)
    
    print("\n--- Analysis Summary ---")
    print(results_df)
    print(f"\nSummary saved to: {output_path}")