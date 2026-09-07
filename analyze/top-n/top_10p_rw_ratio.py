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
        
        result = top_10_rw_df.groupby('addr')['rw'].value_counts().unstack(fill_value=0)
        result.columns = ['read', 'write']

        # 결과 저장 (딕셔너리 형태)
        results.append({
            'benchmark': benchmark,
            'rw_read_count': result['read'].sum(),
            'rw_write_count': result['write'].sum(),
        })

    # 리스트를 데이터프레임으로 변환
    results_df = pd.DataFrame(results)

    # 결과를 CSV 파일로 저장
    output_path = f"{username}/mgvm/analyze/_out/top-n/top_10%_metrics.xlsx"
    results_df.to_excel(output_path, index=False)
    
    print("\n--- Analysis Summary ---")
    print(results_df)
    print(f"\nSummary saved to: {output_path}")
