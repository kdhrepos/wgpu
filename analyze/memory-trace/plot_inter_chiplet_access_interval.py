import os
import pandas as pd
import matplotlib.pyplot as plt

def main():
    import argparse
    parser = argparse.ArgumentParser(description="Plot Inter-Chiplet Access Intervals for Shared Data.")
    parser.add_argument('--group-size', type=int, default=1, help='Group size used in data generation')
    args = parser.parse_args()

    group_size = args.group_size
    suffix = f"_{group_size}" if group_size != 0 else ""
    # 입력 파일 경로
    xlsx_file = f"/home/ehdgus7279/mgvm/scripts/cacheline-metrics/scripts/20260504-164059-mesh-small/t_inter_chiplet_access_interval{suffix}.xlsx"

    if not os.path.exists(xlsx_file):
        print(f"File not found: {xlsx_file}")
        return

    # 출력 폴더
    output_dir = os.path.dirname(xlsx_file) + f"/plots_inter_chiplet_interval{suffix}"
    os.makedirs(output_dir, exist_ok=True)

    df = pd.read_excel(xlsx_file)
    
    # 시각화할 퍼센트 컬럼들
    pct_cols = ['interval_<100_cycles_%', 'interval_100to1K_%', 'interval_1Kto10K_%', 'interval_>10K_%']
    
    # 해당 컬럼들이 존재하는지 체크
    missing_cols = [c for c in pct_cols if c not in df.columns]
    if missing_cols:
        print(f"Missing columns in data: {missing_cols}")
        return
        
    # index를 benchmark로 만들고 필요한 퍼센트 컬럼만 추출
    plot_df = df.set_index('benchmark')[pct_cols]
    
    # 범례(legend)에 이쁘게 보이기 위해 컬럼명 변경
    plot_df.columns = ['< 100 cycles', '100 - 1K cycles', '1K - 10K cycles', '> 10K cycles']

    # 그래프 생성
    fig, ax = plt.subplots(figsize=(12, 6))
    
    plot_df.plot(
        kind='bar',
        stacked=True,
        ax=ax,
        colormap='coolwarm',
        edgecolor='black',
        linewidth=0.5
    )
    
    ax.set_title('Inter-Chiplet Shared Data Access Interval Distribution', fontsize=14, pad=15)
    ax.set_xlabel('Benchmark', fontsize=12)
    ax.set_ylabel('Distribution (%)', fontsize=12)
    ax.set_ylim(0, 100)
    plt.xticks(rotation=45, ha='right')
    
    # 우측 바깥으로 범례 빼기
    ax.legend(title='Interval Range', bbox_to_anchor=(1.05, 1), loc='upper left', frameon=False)
    
    plt.tight_layout()
    
    # 파일 형태 저장
    out_png = os.path.join(output_dir, "inter_chiplet_interval_distribution.png")
    plt.savefig(out_png, dpi=400, bbox_inches='tight')
    plt.close(fig)
    
    print(f"Saved stacked bar chart to: {out_png}")

if __name__ == "__main__":
    main()
