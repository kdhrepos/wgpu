import pandas as pd
import os
from ast import literal_eval
from collections import Counter
import numpy as np
import matplotlib.pyplot as plt
import matplotlib.patches as mpatches
import seaborn as sns

from params import benchmarks

username = os.path.expanduser('~')

if __name__ == "__main__":
    base_dir = f'{username}/mgvm/results/memory-traces'
    date = "20260409-113720-mesh"
    trace_dir = f"{base_dir}/{date}"
    print(f"{trace_dir}")

    results = []

    for benchmark in benchmarks:
        trace_file = f"{trace_dir}/{benchmark}_mesh_6x6_memory_trace.csv"

        if not os.path.exists(trace_file):
            print(f"No {trace_file}, skip")
            continue
        
        print(f"Processing {benchmark}")
        
        # 데이터 로드 및 중복 제거
        df = pd.read_csv(trace_file, skipinitialspace=True)
        df = df.drop_duplicates()

        is_rw_addr = df.groupby('addr')['rw'].transform('max') == 1
        rw_addrs = set(df.loc[is_rw_addr, 'addr']) # 쓰기가 발생한 주소들 (Set)

        # Top 10% 주소들 추출
        cutoff = int(len(df) * 0.10)
        if cutoff == 0: cutoff = 1 # 최소 1개는 포함
        addr_counts = df['addr'].value_counts().reset_index()
        addr_counts = addr_counts.sort_values(by='count', ascending=False).reset_index(drop=True)
        top_10_addrs = addr_counts.head(cutoff)

        # Top 10% 주소 중 rw_addrs(쓰기 발생 주소)에 포함되는 개수 확인
        top_10_rw_df = df[df['addr'].isin(top_10_addrs['addr']) & df['addr'].isin(rw_addrs)]
        top_10_rw_df['time_bin'] = pd.cut(top_10_rw_df['time'], bins=100)

        # 2. 시간 구간별로 rw(0, 1)의 개수를 집계합니다.
        pivot_df = top_10_rw_df.groupby(['time_bin', 'rw']).size().unstack(fill_value=0)

        # 3. 개수를 비율(Percentage)로 변환합니다.
        pivot_df_ratio = pivot_df.div(pivot_df.sum(axis=1), axis=0)

        import pandas as pd
        import matplotlib.pyplot as plt
        import matplotlib.ticker as ticker

        # 1. 시간 구간 설정 (bins 숫자를 조절해 해상도를 변경하세요)
        bins = 100
        df['time_bin'] = pd.cut(df['time'], bins=bins)

        # 2. 피벗 테이블 생성 및 비율 계산
        # ['rw']가 0(Read), 1(Write)라고 가정
        pivot_df = df.groupby(['time_bin', 'rw'], observed=False).size().unstack(fill_value=0)

        # 데이터가 아예 없는 빈 구간(NaN)이 생기지 않도록 보정 후 비율 계산
        pivot_df_ratio = pivot_df.div(pivot_df.sum(axis=1), axis=0).fillna(0)

        # 3. 그래프 그리기
        fig, ax = plt.subplots(figsize=(14, 6))

        # X축 레이블을 구간(Interval) 이름 대신 숫자의 중간값으로 변환하여 시인성 확보
        x_axis = [interval.mid for interval in pivot_df_ratio.index]

        # Stacked Area Chart (순서 명시: Read를 아래에, Write를 위에)
        ax.stackplot(x_axis, 
                    pivot_df_ratio[0], pivot_df_ratio[1], 
                    labels=['Read (0)', 'Write (1)'], 
                    colors=['#3498db', '#e67e22'], alpha=0.8)

        # X축 포맷 설정 (과학적 표기법 방지)
        ax.xaxis.set_major_formatter(ticker.FormatStrFormatter('%.4f'))

        plt.title('Read/Write Ratio Over Time (Fixed)', fontsize=15)
        plt.xlabel('Time', fontsize=12)
        plt.ylabel('Ratio (0.0 - 1.0)', fontsize=12)
        plt.legend(loc='upper right')
        plt.margins(0, 0)
        plt.grid(axis='y', linestyle='--', alpha=0.5)

        plt.tight_layout()
        
        result_dir = f"{username}/mgvm/analyze/_out/top-n/top-10%-rw-ratio-fig"
        os.makedirs(result_dir, exist_ok=True)

        plt.savefig(f"{result_dir}/top_10%_rw_ratio_{benchmark}.png", dpi=400)
        plt.close()