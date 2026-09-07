import os
import pandas as pd
import matplotlib.pyplot as plt
from concurrent.futures import ProcessPoolExecutor
from itertools import repeat


def plot_benchmark(bench, df_subset, output_dir, access_cols, group_size, read_only):
    b_df = df_subset.copy()
    
    # 총 접근 횟수가 많은 PC를 왼쪽(먼저) 배치하기 위해 정렬
    b_df = b_df.sort_values(by='total_accesses', ascending=False)
    
    # PC 개수가 너무 많으면 x축 레이블이 겹칠 수 있으므로 상위 N개만 자를 수도 있습니다.
    # (원한다면 .head(50) 등의 방식으로 제한 가능)
    # b_df = b_df.head(50) 
    
    # PC 값을 문자열로 변환
    b_df['pc_str'] = b_df['pc'].astype(str)
    
    # DataFrame 인덱스를 PC 문자열로 설정하여 플롯의 x축 레이블이 되도록 함
    b_df.set_index('pc_str', inplace=True)
    plot_df = b_df[access_cols].copy()
    
    # 범례(Legend)를 보기 편하게 개선 (Access_%_ 제거)
    plot_df.columns = [c.replace('Access_%_', '') for c in plot_df.columns]
    
    # PC 개수에 맞춰 동적으로 가로 길이(width) 설정 (최소 10, PC당 0.4 픽셀 할당)
    fig_width = max(10, len(b_df) * 0.4)
    fig, ax = plt.subplots(figsize=(fig_width, 6))
    
    # 세로 누적 막대 그래프 그리기 (100% Stacked)
    plot_df.plot(
        kind='bar', 
        stacked=True, 
        ax=ax, 
        colormap='viridis', 
        edgecolor='black',
        linewidth=0.5
    )
    
    title_suffix = " (Read-Only)" if read_only else ""
    ax.set_title(f'[{bench}] PC-Level Sharing, Cache Lines & Accesses{title_suffix}', fontsize=14, pad=15)
    ax.set_xlabel('PC (Sorted by Total Accesses)', fontsize=12)
    ax.set_ylabel('Sharing Ratio (%)', fontsize=12)
    ax.set_ylim(0, 100)
    
    # 2번째 y축 생성하여 캐시 라인(total_addrs) 꺾은선 그래프 추가
    ax2 = ax.twinx()
    ax2.plot(range(len(b_df)), b_df['total_addrs'], color='red', marker='o', linestyle='-', linewidth=2, markersize=4, label='Cache Lines')
    ax2.set_ylabel('Number of Cache Lines (Log Scale)', fontsize=12, color='red')
    ax2.tick_params(axis='y', labelcolor='red')
    ax2.set_yscale('log')
    
    # 3번째 y축 생성하여 총 접근 횟수(total_accesses) 꺾은선 그래프 추가
    ax3 = ax.twinx()
    # 3번째 축을 2번째 축보다 더 오른쪽으로 충분히 밀어냄 (겹침 방지)
    ax3.spines['right'].set_position(('outward', 85))
    ax3.plot(range(len(b_df)), b_df['total_accesses'], color='blue', marker='s', linestyle='--', linewidth=2, markersize=4, label='Total Accesses')
    ax3.set_ylabel('Total Accesses (Log Scale)', fontsize=12, color='blue')
    ax3.tick_params(axis='y', labelcolor='blue')
    ax3.set_yscale('log')
    
    # 막대그래프(ax1), 2번째 꺾은선(ax2), 3번째 꺾은선(ax3) 범례를 하나로 합침
    handles1, labels1 = ax.get_legend_handles_labels()
    handles2, labels2 = ax2.get_legend_handles_labels()
    handles3, labels3 = ax3.get_legend_handles_labels()
    
    # 범례가 y축과 겹치지 않도록 충분히 먼 위치로 이동 (예: x=1.3)
    ax.legend(handles1 + handles2 + handles3, labels1 + labels2 + labels3, 
              title='Legend', bbox_to_anchor=(1.30, 1), loc='upper left', frameon=False)

    plt.tight_layout()
    
    # PNG 파일로 저장 (group_size 및 read_only 포함)
    suffix = f"_{group_size}" if group_size != 0 else ""
    ro_suffix = "_readonly" if read_only else ""
    out_png = os.path.join(output_dir, f"{bench}_pc_sharing{suffix}{ro_suffix}.png")
    plt.savefig(out_png, dpi=400, bbox_inches='tight')
    plt.close(fig)
    
    return f"  - Saved plot for {bench}: {out_png}"

def main():
    import argparse
    parser = argparse.ArgumentParser(description="Plot PC-level chiplet sharing distribution from CSV.")
    parser.add_argument('--group-size', type=int, default=1, help='Group size for plotting')
    parser.add_argument('--read-only', action='store_true', help='Plot only read-only PCs')
    args = parser.parse_args()

    group_size = args.group_size
    read_only = args.read_only

    # 시각화할 CSV 파일의 경로 (group_size 값에 맞춰 suffix 추가)
    suffix = f"_{group_size}" if group_size != 0 else ""
    ro_suffix = "_readonly" if read_only else ""
    csv_file = f"/home/ehdgus7279/mgvm/scripts/cacheline-metrics/scripts/20260429-210946-mesh/t_pc_chiplet_sharing_distribution{suffix}.csv"

    if not os.path.exists(csv_file):
        print(f"File not found: {csv_file}")
        return
    
    # 출력 파일들이 저장될 디렉토리(group_size 및 readonly 포함) 생성
    output_dir = os.path.dirname(csv_file) + f"/plots_pc_sharing{suffix}{ro_suffix}"
    os.makedirs(output_dir, exist_ok=True)
    
    # 데이터 로드
    df = pd.read_csv(csv_file)
    
    # read-only 필터링: 쓰기 횟수가 0인 경우만 추출
    if read_only:
        df = df[df['write_count'] == 0]
        if df.empty:
            print("No read-only data found.")
            return

    # 접근 횟수(Access Count) 기준 비율 컬럼 필터링
    # 주소 개수 기준(Addr_%_)을 보고 싶다면 아래 Access_%_ 를 Addr_%_ 로 바꾸면 됩니다.
    access_cols = [c for c in df.columns if c.startswith('Access_%_')]
    
    # 벤치마크 목록 추출
    benchmarks = df['benchmark'].unique()
    
    print(f"Found {len(benchmarks)} benchmarks. Generating plots in parallel with group_size={group_size}, read_only={read_only}...")
    
    # 병렬 처리를 위해 각 벤치마크에 해당하는 데이터프레임을 미리 나눔
    tasks = []
    for bench in benchmarks:
        df_subset = df[df['benchmark'] == bench]
        # plot_benchmark 에 파라미터 추가 전달
        tasks.append((bench, df_subset, output_dir, access_cols, group_size, read_only))
    
    # ProcessPoolExecutor를 사용해 병렬 렌더링
    with ProcessPoolExecutor() as executor:
        # 각 작업 제출
        futures = [executor.submit(plot_benchmark, *task) for task in tasks]
        
        # 결과 대기 및 출력
        for future in futures:
            try:
                msg = future.result()
                print(msg)
            except Exception as e:
                print(f"Error occurred during plotting: {e}")

if __name__ == "__main__":
    main()