import os
import pandas as pd
import matplotlib.pyplot as plt
from concurrent.futures import ProcessPoolExecutor

def plot_benchmark(bench, df_subset, output_dir, group_size):
    b_df = df_subset.copy()
    
    # 1. 막대 그래프를 위한 데이터 가공 (평균 요청 칩렛 수)
    b_df = b_df.sort_values(by='total_access_count', ascending=False)
    
    b_df['pc_str'] = b_df['pc'].astype(str)
    b_df.set_index('pc_str', inplace=True)
    
    plot_df = b_df[['avg_requestor_chiplets']].copy()
    plot_df.columns = ['Avg Requestor Chiplets']
    
    fig_width = max(10, len(b_df) * 0.4)
    fig, ax = plt.subplots(figsize=(fig_width, 6))
    
    # 막대 그래프 도출 (Avg Requestor Chiplets)
    plot_df.plot(
        kind='bar', 
        stacked=False, 
        ax=ax, 
        color='mediumpurple',
        edgecolor='black',
        linewidth=0.5
    )
    
    ax.set_title(f'[{bench}] PC-Level L2 Miss (RDMA) - Shared Chiplets & Access Stats', fontsize=14, pad=15)
    ax.set_xlabel('PC (Sorted by Total L2 Misses)', fontsize=12)
    ax.set_ylabel('Avg Requestor Chiplets', fontsize=12)
    ax.set_ylim(0, 16) # 최대 16개 칩렛
    
    # 2. 첫 번째 꺾은선: 해당 PC가 접근한 Unique Address 개수
    ax2 = ax.twinx()
    ax2.plot(range(len(b_df)), b_df['total_unique_addrs'], color='red', marker='o', linestyle='-', linewidth=2, markersize=4, label='Unique Addrs')
    ax2.set_ylabel('Number of Unique Addrs (Log Scale)', fontsize=12, color='red')
    ax2.tick_params(axis='y', labelcolor='red')
    ax2.set_yscale('log')
    
    # 3. 두 번째 꺾은선: 총 L2 Miss 접근 횟수
    ax3 = ax.twinx()
    ax3.spines['right'].set_position(('outward', 85))
    ax3.plot(range(len(b_df)), b_df['total_access_count'], color='blue', marker='s', linestyle='--', linewidth=2, markersize=4, label='Total L2 Misses')
    ax3.set_ylabel('Total L2 Misses (Log Scale)', fontsize=12, color='blue')
    ax3.tick_params(axis='y', labelcolor='blue')
    ax3.set_yscale('log')
    
    # 4. 세 번째 꺾은선: 캐시 라인당 평균 접근 횟수
    ax4 = ax.twinx()
    ax4.spines['right'].set_position(('outward', 170))
    ax4.plot(range(len(b_df)), b_df['avg_access_per_cacheline'], color='green', marker='^', linestyle='-.', linewidth=2, markersize=4, label='Avg Access / Cache Line')
    ax4.set_ylabel('Avg Access / Cache Line (Log Scale)', fontsize=12, color='green')
    ax4.tick_params(axis='y', labelcolor='green')
    ax4.set_yscale('log')
    
    # 5. 범례 통합
    handles1, labels1 = ax.get_legend_handles_labels()
    handles2, labels2 = ax2.get_legend_handles_labels()
    handles3, labels3 = ax3.get_legend_handles_labels()
    handles4, labels4 = ax4.get_legend_handles_labels()
    
    ax.legend(handles1 + handles2 + handles3 + handles4, labels1 + labels2 + labels3 + labels4, 
              title='Metrics', bbox_to_anchor=(2.0, 1), loc='upper left', frameon=False)
    
    plt.tight_layout()
    
    # 결과 이미지 저장
    suffix = f"_{group_size}" if group_size != 0 else ""
    out_png = os.path.join(output_dir, f"{bench}_l2miss_pc_stats{suffix}.png")
    plt.savefig(out_png, dpi=400, bbox_inches='tight')
    plt.close(fig)
    
    return f"  - Saved plot for {bench}: {out_png}"

def main():
    import argparse
    parser = argparse.ArgumentParser(description="Plot L2 Miss (RDMA) PC Stats.")
    parser.add_argument('--group-size', type=int, default=1, help='Group size for plotting')
    args = parser.parse_args()

    group_size = args.group_size

    suffix = f"_{group_size}" if group_size != 0 else ""
    xlsx_file = f"/home/ehdgus7279/mgvm/scripts/cacheline-metrics/scripts/20260504-154415-mesh/t_pc_stats_l2miss_per_owner{suffix}.xlsx"

    if not os.path.exists(xlsx_file):
        print(f"File not found: {xlsx_file}")
        return
    
    output_dir = os.path.dirname(xlsx_file) + f"/plots_pc_l2miss_stats{suffix}"
    os.makedirs(output_dir, exist_ok=True)
    
    df = pd.read_excel(xlsx_file)
    benchmarks = df['benchmark'].unique()
    
    print(f"Found {len(benchmarks)} benchmarks. Generating plots in parallel...")
    
    tasks = []
    for bench in benchmarks:
        df_subset = df[df['benchmark'] == bench]
        tasks.append((bench, df_subset, output_dir, group_size))
    
    with ProcessPoolExecutor() as executor:
        futures = [executor.submit(plot_benchmark, *task) for task in tasks]
        for future in futures:
            try:
                print(future.result())
            except Exception as e:
                print(f"Error occurred: {e}")

if __name__ == "__main__":
    main()
