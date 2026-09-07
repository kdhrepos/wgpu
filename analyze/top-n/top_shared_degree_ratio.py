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

xDim = 4
yDim = 4

def plot_access_interval(
    inter_per_addr_stats,
    benchmark,
    save_fig=False,
    metric = 'mean',
):
    import matplotlib.pyplot as plt
    import seaborn as sns

    # 1. 고해상도 설정 (DPI를 150~300으로 설정하면 훨씬 선명해집니다)
    plt.rcParams['figure.dpi'] = 150 
    sns.set_theme(style="whitegrid") # 깔끔한 배경 스타일 적용

    plt.figure(figsize=(12, 7))

    # 2. 히스토그램 세부 설정
    # linewidth=0으로 설정하면 막대 테두리 때문에 색이 뭉치는 현상을 방지합니다.
    sns.histplot(
        data=inter_per_addr_stats, 
        x='mean', 
        bins=60, 
        kde=True, 
        log_scale=True,
        color='#2c3e50',   # 차분한 네이비톤 컬러
        edgecolor='white', # 막대 사이 구분선
        linewidth=0.5,
        alpha=0.7
    )

    # 3. 제목 및 라벨 가독성 향상
    plt.title('Distribution of Average Memory Access Intervals', fontsize=16, fontweight='bold', pad=20)
    plt.xlabel('Average Interval (Cycles, Log Scale)', fontsize=12)
    plt.ylabel('Address Count', fontsize=12)

    # 4. 그리드 및 레이아웃 정리
    plt.grid(True, which="both", ls="--", alpha=0.4)
    plt.tight_layout()

    # 5. 저장 시에도 고화질 유지 (필요 시 주석 해제)
    result_dir = f"{username}/mgvm/analyze/_out/top-n/top-shared-access-interval/{metric}"
    os.makedirs(result_dir, exist_ok=True)

    if save_fig:
        plt.savefig(f'{result_dir}/{benchmark}_memory_interval.png', dpi=400, bbox_inches='tight')


def process_benchmark(benchmark):
    """
    개별 벤치마크 파일을 처리하는 함수 (병렬 실행 대상)
    """
    trace_file = f"{trace_dir}/{benchmark}_mesh_{xDim}x{yDim}_memory_trace.csv"

    if not os.path.exists(trace_file):
        print(f"No {trace_file}, skip")
        return None
    
    print(f"Processing {benchmark}...")
    
    # 데이터 로드
    df = pd.read_csv(trace_file, skipinitialspace=True)
    df = df.drop_duplicates()
    df = df[df['rdma'] == True]

    ns = 1e-9
    df['cycle'] = df['time'] / ns
    df['cycle'] = df['cycle'].astype(int)

    is_rw_addr = df.groupby('addr')['rw'].transform('max') == 1
    rw_addrs = set(df.loc[is_rw_addr, 'addr'])

    # 2. Hop Count 벡터화 계산 (속도 최적화 핵심)
    w = xDim
    s_x, s_y = df['chiplet'] % w, df['chiplet'] // w
    d_x, d_y = df['owner'] % w, df['owner'] // w
    df['hop_count'] = (d_x - s_x).abs() + (d_y - s_y).abs()

    clusters_3x3 = {
        'cluster_0_3x3': [0, 1, 2, 6, 7, 8, 12, 13, 14],
        'cluster_1_3x3': [3, 4, 5, 9, 10, 11, 15, 16, 17],
        'cluster_2_3x3': [18, 19, 20, 24, 25, 26, 30, 31, 32],
        'cluster_3_3x3': [21, 22, 23, 27, 28, 29, 33, 34, 35],
    }

    chiplet_to_cluster = {
        chiplet: cluster 
        for cluster, chiplets in clusters_3x3.items() 
        for chiplet in chiplets
    }

    # 2. .map() 사용 (apply보다 훨씬 빠름)
    df['cluster_3x3'] = df['chiplet'].map(chiplet_to_cluster).fillna('unknown')
    df['owner_cluster_3x3'] = df['owner'].map(chiplet_to_cluster).fillna('unknown')

    remote_cluster_df = df[df['cluster_3x3'] != df['owner_cluster_3x3']]

    addr_metrics_df = remote_cluster_df.groupby("addr").agg({
        'cluster_3x3': 'nunique',  # 데이터를 빌려간 외부 클러스터 수
        'chiplet': 'nunique',      # 데이터를 빌려간 외부 칩렛 수
        'addr': 'count',           # 외부에서 발생한 총 접근 횟수
        'hop_count': 'sum',          # 외부에서 발생한 총 hop count
    }).rename(columns={
        'cluster_3x3': 'shared_clusters', 
        'chiplet': 'shared_chiplets',
        'addr': 'count',
        'hop_count': 'hop_count',
    }).reset_index()

    total_access_count = addr_metrics_df['count'].sum()
    total_hop_count = df['hop_count'].sum()
    total_cachelines = len(addr_metrics_df)

    top_3x3_cluster_shared_count = addr_metrics_df[addr_metrics_df['shared_clusters']>=3]['count'].sum()
    top_3x3_cluster_shared_hop_count = addr_metrics_df[addr_metrics_df['shared_clusters']>=3]['hop_count'].sum()
    top_3x3_cluster_shared_cachelines = len(addr_metrics_df[addr_metrics_df['shared_clusters']>=3])

    top_3x3_cluster_ro_shared = addr_metrics_df[
        (addr_metrics_df['shared_clusters']>=3) & 
        (addr_metrics_df['addr'].isin(rw_addrs)==False)]['count'].sum()
    top_3x3_cluster_ro_shared_hop_count = addr_metrics_df[
        (addr_metrics_df['shared_clusters']>=3) & 
        (addr_metrics_df['addr'].isin(rw_addrs)==False)]['hop_count'].sum()
    top_3x3_cluster_ro_shared_cachelines = len(addr_metrics_df[
        (addr_metrics_df['shared_clusters']>=3) & 
        (addr_metrics_df['addr'].isin(rw_addrs)==False)])

    # top_3x3_cluster_shared_df = addr_metrics_df[addr_metrics_df['shared_clusters']>=3]
    # top_shared_df = df[df['addr'].isin(top_3x3_cluster_shared_df['addr'])]
    # top_shared_df['inter_arrival'] = top_shared_df.groupby(['addr', 'chiplet'])['cycle'].diff()
    # inter_arrival_df = top_shared_df.dropna(subset=['inter_arrival'])
    # inter_per_addr_stats = inter_arrival_df.groupby('addr')['inter_arrival'].agg(['mean', 'median', 'std', 'max'])
    
    # plot_access_interval(inter_per_addr_stats, benchmark, save_fig=True, metric='mean')

    return {
        'benchmark': benchmark,
        'total_access_count': total_access_count,
        'total_cachelines': total_cachelines,
        'total_hop_count': total_hop_count,
        'top_3x3_cluster_shared_count': top_3x3_cluster_shared_count,
        'top_3x3_cluster_shared_cachelines': top_3x3_cluster_shared_cachelines,
        'top_3x3_cluster_shared_hop_count': top_3x3_cluster_shared_hop_count,
        'top_3x3_cluster_ro_shared': top_3x3_cluster_ro_shared,
        'top_3x3_cluster_ro_shared_cachelines': top_3x3_cluster_ro_shared_cachelines,
        'top_3x3_cluster_ro_shared_hop_count': top_3x3_cluster_ro_shared_hop_count,
    }

if __name__ == "__main__":
    print(f"Target Directory: {trace_dir}")

    import argparse
    parser = argparse.ArgumentParser(description='Analyze top shared degree ratio across benchmarks.')
    parser.add_argument('run_id', nargs='?', default='latest',
                        help="run-id (latest, <config>, <config>/<timestamp>, ...)")
    parser.add_argument('--plot', type=bool, default=False, help='Whether to generate and save plots for each benchmark.')
    args = parser.parse_args()

    # --- 병렬 처리 핵심 부분 ---
    # max_workers를 지정하지 않으면 현재 시스템의 CPU 코어 수만큼 프로세스를 생성합니다.
    with ProcessPoolExecutor() as executor:
        results = list(executor.map(process_benchmark, benchmarks))

    # None 결과값 필터링 (파일이 없었던 경우)
    results = [r for r in results if r is not None]

    # 결과 저장
    results_df = pd.DataFrame(results)
    output_path = f"{username}/mgvm/analyze/_out/top-n/top_10%_shared_degree_ratio.csv"
    results_df.to_csv(output_path, index=False)
    
    print("\n--- Analysis Summary ---")
    print(results_df)
    print(f"\nSummary saved to: {output_path}")