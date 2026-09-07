def plot_access_count_per_addrs(df, benchmark, result_dir, save_fig=True):
    import matplotlib.pyplot as plt
    import seaborn as sns
    import matplotlib.ticker as mtick
    import numpy as np

    # 1. 데이터 준비
    plot_data = df.groupby('addr').agg(
                    access_count= ('addr', 'size'),
                    # shared_chiplets= ('chiplet', 'nunique'),
                ).rename(columns={'addr': 'count'}).reset_index()
    total_access = plot_data['access_count'].sum()
    num_cachelines = df['addr'].nunique()

    plot_data['cumsum_percent'] = (plot_data['access_count'].cumsum() / total_access) * 100

    # 2. 시각화 설정
    plt.style.use('seaborn-v0_8-paper')
    fig, ax1 = plt.subplots(figsize=(10, 6), dpi=300)

    # --- 3. 왼쪽 축 (ax1): Scatter (점 크기 s=8, 투명도 alpha=0.3으로 조절) ---
    scatter = ax1.scatter(plot_data['addr'], plot_data['access_count'], 
                          color='#1f77b4', s=8, alpha=0.5, edgecolor='none', 
                          label='Individual Access')

    ax1.set_ylabel('Access Count', fontsize=10, fontweight='bold', color='#1f77b4')
    ax1.tick_params(axis='y', labelcolor='#1f77b4')

    # --- 4. 오른쪽 축 (ax2): 누적 선 ---
    ax2 = ax1.twinx()
    line, = ax2.plot(plot_data['addr'], plot_data['cumsum_percent'], 
                     color='#e74c3c', linewidth=1.5, label='Cumulative Total %')

    ax2.set_ylabel('Cumulative Access (%)', fontsize=10, fontweight='bold', color='#e74c3c')
    ax2.tick_params(axis='y', labelcolor='#e74c3c')
    ax2.set_ylim(0, 105)
    ax2.yaxis.set_major_formatter(mtick.PercentFormatter())

    # --- 5. X축 16진수 포맷팅 ---
    @mtick.FuncFormatter
    def hex_formatter(x, pos):
        return f"0x{int(x):08X}"
    ax1.xaxis.set_major_formatter(hex_formatter)

    # 6. 축 및 레이블 설정
    ax1.set_title(f'Memory Access Distribution & Cumulative Traffic\n(Total Accesses: {total_access:,})\n(Total Cachelines: {num_cachelines:,})', 
                fontsize=13, fontweight='bold', pad=20)
    ax1.set_xlabel('Memory Address (Hex)', fontsize=10, fontweight='bold')

    # --- [핵심] 범례 하나로 통합하여 띄우기 ---
    # 수동으로 핸들과 라벨을 리스트로 묶어 전달합니다.
    handles = [scatter, line]
    labels = [h.get_label() for h in handles]
    
    ax1.legend(handles, labels, 
               loc='upper left', 
               bbox_to_anchor=(0.02, 0.98), 
               fontsize=9, 
               frameon=True, 
               facecolor='white', 
               edgecolor='lightgrey')

    # 7. 디자인 정리
    plt.xticks(rotation=30, ha='right')
    ax1.spines['top'].set_visible(False)
    ax1.grid(True, linestyle='--', alpha=0.3)
    ax2.grid(False) 

    plt.tight_layout()
    
    if save_fig:
        plt.savefig(f"{result_dir}/{benchmark}_access_count_per_addrs.png", bbox_inches='tight')

    plt.close()


def plot_temporal_access_and_footprint(df, benchmark, result_dir, save_fig=True):
    import matplotlib.pyplot as plt
    import seaborn as sns
    import matplotlib.ticker as mtick
    import numpy as np
    import os

    # --- 하드웨어 파라미터 및 Binning (동일) ---
    freq_hz = 10**9  # 1GHz
    cycles_per_interval = 10000
    interval_sec = cycles_per_interval / freq_hz 


    df['time_bin'] = (df['time'] // interval_sec) * interval_sec
    temporal_stats = df.groupby(['time_bin', 'addr']).agg(
        access_count = ('addr', 'size')
    ).rename(columns={'addr': 'count'}).reset_index()

    temporal_stats['cycles'] = (temporal_stats['time_bin'] * freq_hz).astype(int)

    # 3. 시각화 설정 (figsize를 범례 공간만큼 세로로 조금 더 늘려줍니다)
    plt.style.use('seaborn-v0_8-paper')
    # 범례가 위에 들어가므로 figsize 세로를 6 -> 6.5로 약간 늘림
    fig, ax1 = plt.subplots(figsize=(12, 6.5), dpi=300) 

    # --- 4. 왼쪽 축 (ax1): Total Access Count (Bar) ---
    width = cycles_per_interval * 0.8
    bar = ax1.bar(temporal_stats['cycles'], temporal_stats['access_count'], 
                  width=width, color='#3498db', alpha=0.6, label='Total Access Count')

    ax1.set_xlabel('Execution Time (Cycles)', fontsize=11, fontweight='bold')
    ax1.set_ylabel('Total Access Count', fontsize=11, fontweight='bold', color='#2980b9')
    ax1.tick_params(axis='y', labelcolor='#2980b9')
    ax1.xaxis.set_major_formatter(mtick.StrMethodFormatter('{x:,.0f}'))
    ax1.yaxis.set_major_formatter(mtick.StrMethodFormatter('{x:,.0f}'))

    # --- 5. 오른쪽 축 (ax2): Unique Cache Lines (Line) ---
    ax2 = ax1.twinx()
    line, = ax2.plot(temporal_stats['cycles'], temporal_stats['addr'], 
                     color='#e67e22', marker='o', markersize=3, linewidth=1.2, label='Unique Cache Lines')

    ax2.set_ylabel('Unique Cache Lines (Footprint)', fontsize=11, fontweight='bold', color='#d35400')
    ax2.tick_params(axis='y', labelcolor='#d35400')
    ax2.yaxis.set_major_formatter(mtick.StrMethodFormatter('{x:,.0f}'))

    # --- 6. [핵심] 범례를 차트 바깥(상단)으로 빼기 ---
    # 범례 통합
    handles = [bar, line]
    labels = [h.get_label() for h in handles]
    
    # bbox_to_anchor와 loc를 조합하여 위치 지정
    # loc='lower left', bbox_to_anchor=(0, 1.02): 그래프 왼쪽 상단(0,1)보다 2% 위(1.02)에 배치
    # ncol=2: 가로로 나열
    ax1.legend(handles, labels, 
               loc='lower left', 
               bbox_to_anchor=(0.0, 1.02), 
               ncol=2, 
               fontsize=10, 
               frameon=True, 
               facecolor='white', 
               edgecolor='lightgrey')
    
    # 만약 '오른쪽 바깥'에 세로로 배치하고 싶다면 아래 주석을 해제하고 위 코드를 주석처리하세요.
    # ax1.legend(handles, labels, loc='upper left', bbox_to_anchor=(1.05, 1), fontsize=10)

    # 7. 제목 및 디자인 정리
    # 범례가 위에 들어가므로 pad 값을 늘려 제목을 위로 더 밀어줍니다.
    plt.title(f'Temporal Memory Access Analysis [{benchmark}]\n(Resolution: {cycles_per_interval:,} Cycles @ 1GHz)', 
              fontsize=14, fontweight='bold', pad=45) 

    ax1.grid(True, axis='y', linestyle='--', alpha=0.3)
    ax1.spines['top'].set_visible(False)
    ax2.spines['top'].set_visible(False)

    # fig.tight_layout() 대신 bbox_inches='tight'를 사용하는 것이 바깥 범례를 포함하여 저장할 때 더 유리합니다.
    if save_fig:
        plt.savefig(f"{result_dir}/{benchmark}_temporal_analysis.png", bbox_inches='tight')

    plt.close()


def plot_memory_access_trace(df, agg_df, benchmark, result_dir, freq_ghz=1.0, save_fig=True):
    import matplotlib.pyplot as plt
    import seaborn as sns
    import matplotlib.ticker as mtick

    # 1. 데이터 및 지표 준비
    freq_hz = freq_ghz * 10**9
    df = df.copy()
    df['cycles'] = (df['time'] * freq_hz).astype(int)
    
    # 정적 임계값 기반 분류 (agg_df 활용)
    global_addrs = agg_df[agg_df['access_count'] >= 255]['addr'].unique()
    hot_addrs    = agg_df[(agg_df['access_count'] >= 127) & (agg_df['access_count'] < 255)]['addr'].unique()
    warm_addrs   = agg_df[(agg_df['access_count'] >= 63) & (agg_df['access_count'] < 127)]['addr'].unique()
    
    # 전체 데이터에 레벨 마킹
    def assign_level(addr):
        if addr in global_addrs: return 'Global'
        if addr in hot_addrs:    return 'Hot'
        if addr in warm_addrs:   return 'Warm'
        return 'Normal'

    df['level'] = df['addr'].map(assign_level)

    # 지표 계산 (제목용)
    total_accesses = df['access_count'].sum()
    unique_cachelines = df['addr'].nunique()

    # 2. 시각화 설정
    plt.style.use('seaborn-v0_8-paper')
    fig, ax = plt.subplots(figsize=(14, 9), dpi=300)

    # 레벨별 설정 (색상, 크기, 투명도, 그리기 순서)
    # zorder가 높을수록 위쪽에 그려집니다.
    levels_config = {
        'Normal': {'color': "#acdbfa", 's': 1.0, 'alpha': 0.30, 'zorder': 1, 'label': 'Normal'},
        'Warm':   {'color': '#2ecc71', 's': 1.0, 'alpha': 0.30, 'zorder': 2, 'label': 'Warm (64+)'},
        'Hot':    {'color': '#f39c12', 's': 1.0, 'alpha': 0.30, 'zorder': 3, 'label': 'Hot (128+)'},
        'Global': {'color': '#e74c3c', 's': 1.0, 'alpha': 0.30, 'zorder': 4, 'label': 'Global (256+)'}
    }

    # 3. Scatter Plot 레이어별 실행
    for lvl, config in levels_config.items():
        lvl_data = df[df['level'] == lvl]
        if not lvl_data.empty:
            ax.scatter(lvl_data['cycles'], lvl_data['addr'], 
                       color=config['color'], s=config['s'], alpha=config['alpha'], 
                       label=config['label'], zorder=config['zorder'],
                       edgecolor='none', rasterized=True)

    # 4. 축 포맷팅
    @mtick.FuncFormatter
    def hex_formatter(x, pos):
        return f"0x{int(x):08X}"
    
    ax.yaxis.set_major_formatter(hex_formatter)
    ax.xaxis.set_major_formatter(mtick.StrMethodFormatter('{x:,.0f}'))

    if not df.empty:
        ymin, ymax = df['addr'].min(), df['addr'].max()
        padding = (ymax - ymin) * 0.05
        ax.set_ylim(ymin - padding, ymax + padding)

    # 5. 상세 제목 설정
    title_str = (
        f"Memory Access Trace: {benchmark}\n"
        f"(Total Accesses: {total_accesses:,}, Unique Cachelines: {unique_cachelines:,})\n"
        f"Levels: Global {len(global_addrs):,} | Hot {len(hot_addrs):,} | Warm {len(warm_addrs):,}"
    )
    ax.set_title(title_str, fontsize=15, fontweight='bold', pad=40, loc='center')

    # 6. 레이블 및 디자인 정리
    ax.set_xlabel('Execution Time (Cycles)', fontsize=12, fontweight='bold')
    ax.set_ylabel('Memory Address (Hex)', fontsize=12, fontweight='bold')

    # 범례 배치 (상단 외부)
    ax.legend(loc='lower left', bbox_to_anchor=(0.0, 1.02), ncol=4, 
              fontsize=10, frameon=True, markerscale=6)

    ax.spines['top'].set_visible(False)
    ax.spines['right'].set_visible(False)
    ax.grid(True, linestyle='--', alpha=0.15)

    plt.tight_layout()

    if save_fig:
        plt.savefig(f"{result_dir}/{benchmark}_memory_access_trace.png", bbox_inches='tight')
    
    plt.close()


def plot_temporal_mean_std_analysis(df, benchmark, result_dir, save_fig=True):
    import matplotlib.pyplot as plt
    import seaborn as sns
    import matplotlib.ticker as mtick
    import numpy as np

    # 1. 하드웨어 파라미터 및 Binning
    freq_hz = 10**9  # 1GHz
    cycles_per_interval = 10000
    interval_sec = cycles_per_interval / freq_hz 

    df = df.copy()
    df['time_bin'] = (df['time'] // interval_sec) * interval_sec
    
    # 1단계: 각 시간 구간(time_bin) 내에서 각 주소(addr)별 접근 횟수 합계 구하기
    # (특정 주소가 해당 구간 동안 총 몇 번 호출되었는지)
    addr_sums_per_bin = df.groupby(['time_bin', 'addr'])['access_count'].sum().reset_index()

    # 2단계: 위에서 구한 '주소별 합계'들을 바탕으로 시간 구간별 평균과 표준편차 계산
    # (해당 구간에 접근된 주소들은 평균적으로 몇 번씩 접근되었는가?)
    stats = addr_sums_per_bin.groupby('time_bin')['access_count'].agg(['mean', 'std']).reset_index()
    
    # 데이터가 1개인 구간은 표준편차가 NaN이 되므로 0으로 처리
    stats['std'] = stats['std'].fillna(0)
    stats['cycles'] = (stats['time_bin'] * freq_hz).astype(int)

    # 3. 시각화 설정
    plt.style.use('seaborn-v0_8-paper')
    fig, ax = plt.subplots(figsize=(12, 6.5), dpi=300)

    # --- 4. 평균선(Mean Line) 그리기 ---
    line_mean, = ax.plot(stats['cycles'], stats['mean'], 
                         color='#2c3e50', linewidth=2, marker='o', markersize=4, 
                         label='Mean Access Count')

    # --- 5. 표준 편차 영역(Std Dev Band) 그리기 ---
    # 평균 ± 표준편차 범위를 색칠하여 변동성을 시각화
    ax.fill_between(stats['cycles'], 
                    stats['mean'] - stats['std'], 
                    stats['mean'] + stats['std'], 
                    color='#2c3e50', alpha=0.2, label='Std Dev (Variation)')

    # 6. 축 및 레이블 설정
    ax.set_xlabel('Execution Time (Cycles)', fontsize=11, fontweight='bold')
    ax.set_ylabel('Access Count per Cache Line', fontsize=11, fontweight='bold')
    ax.xaxis.set_major_formatter(mtick.StrMethodFormatter('{x:,.0f}'))
    ax.yaxis.set_major_formatter(mtick.StrMethodFormatter('{x:,.1f}')) # 소수점 첫째자리까지

    # 7. 범례 및 제목 (상단 외부 배치)
    handles, labels = ax.get_legend_handles_labels()
    ax.legend(handles, labels, 
               loc='lower left', 
               bbox_to_anchor=(0.0, 1.02), 
               ncol=2, 
               fontsize=10, 
               frameon=True, 
               facecolor='white', 
               edgecolor='lightgrey')

    plt.title(f'Temporal Access Stability Analysis [{benchmark}]\n(Mean & Std Dev per Cache Line)', 
              fontsize=14, fontweight='bold', pad=45)

    # 디자인 정리
    ax.grid(True, linestyle='--', alpha=0.3)
    ax.spines['top'].set_visible(False)
    ax.spines['right'].set_visible(False)

    # Y축 하한선 설정 (음수 방지)
    current_ymin, current_ymax = ax.get_ylim()
    ax.set_ylim(max(0, current_ymin), current_ymax * 1.1)

    plt.tight_layout()

    if save_fig:
        plt.savefig(f"{result_dir}/{benchmark}_temporal_mean_std.png", bbox_inches='tight')
    
    plt.close()

    
def plot_access_count_with_shared_degree(df, benchmark, result_dir, save_fig=True):
    import matplotlib.pyplot as plt
    import matplotlib.ticker as mtick
    import matplotlib.colors as colors
    import numpy as np

    # 1. 데이터 준비 및 정렬
    # 주소 순서대로 정렬하여 연속적인 주소 공간을 시각화
    addr_counts = df.groupby('addr').agg(
        access_count= ('addr', 'size'),
        shared_chiplets= ('chiplet', 'nunique'),
    ).rename(columns={'addr': 'count'}).reset_index()
    
    plot_data = addr_counts.copy().sort_values(by='addr')
    
    # 지표 계산 (제목 및 분석용)
    total_accesses = plot_data['access_count'].sum()
    unique_cachelines = len(plot_data)
    max_sharing = plot_data['shared_chiplets'].max()

    # 2. 시각화 설정 (논문용 스타일)
    plt.style.use('seaborn-v0_8-paper')
    # 가로 길이를 충분히 늘려 주소 공간 가독성 확보 (16x8)
    fig, ax = plt.subplots(figsize=(16, 8), dpi=300) 

    # --- 3. 산점도 (Scatter Plot) 그리기 ---
    # s: 점 크기 (데이터가 많으니 작게 설정)
    # cmap: Heatmap과 동일한 'YlOrRd' (노랑-오렌지-빨강) 사용
    # norm: 데이터 편차가 심하므로 LogNorm 적용 (vmin=1 필수)
    scatter = ax.scatter(plot_data['addr'], plot_data['shared_chiplets'], 
                        c=plot_data['access_count'], 
                        cmap='YlOrRd', 
                        s=5.0, # 점 크기 미세 조정
                        alpha=1, # 약간의 투명도로 겹침 현상 완화
                        norm=colors.LogNorm(vmin=1, vmax=plot_data['access_count'].max()),
                        rasterized=True) # 데이터가 많을 때 저장 속도 향상

    # 축 설정
    ax.set_xlabel('Memory Address (Hex)', fontsize=12, fontweight='bold')
    ax.set_ylabel('Shared Chiplets (Nodes)', fontsize=12, fontweight='bold')
    
    # Y축 포맷팅 (정수형)
    ax.yaxis.set_major_formatter(mtick.StrMethodFormatter('{x:,.0f}'))
    
    # [팁] Y축 범위 최적화 (음수 방지 및 상단 여백 확보)
    ax.set_ylim(0, max_sharing + 1.5)

    # X축 포맷팅 (16진수 주소)
    @mtick.FuncFormatter
    def hex_formatter(x, pos):
        return f"0x{int(x):08X}"
    ax.xaxis.set_major_formatter(hex_formatter)

    # 4. 컬러바 (Colorbar) 추가
    # 점의 색상이 의미하는 Access Count를 보여줍니다.
    cbar = plt.colorbar(scatter, label='Access Count (Log Scale)')
    # 컬러바 눈금에 1,000단위 콤마 추가
    cbar.formatter = mtick.StrMethodFormatter('{x:,.0f}')
    cbar.update_ticks()

    # 5. 제목 설정 (상세 지표 포함)
    title_str = (
        f"Memory Access Intensity & Sharing Mapping: {benchmark}\n"
        f"(Total Accesses: {total_accesses:,}, Unique Lines: {unique_cachelines:,}, Max Sharing: {max_sharing:,} Nodes)"
    )
    plt.title(title_str, fontsize=15, fontweight='bold', pad=35, loc='center')

    # 6. 디자인 정리
    ax.grid(True, linestyle='--', alpha=0.15)
    ax.spines['top'].set_visible(False)
    ax.spines['right'].set_visible(False)

    # X축 레이블 회전 (주소가 길 경우 대비)
    plt.xticks(rotation=30, ha='right')

    plt.tight_layout()

    if save_fig:
        plt.savefig(f"{result_dir}/{benchmark}_access_sharing_mapping.png", bbox_inches='tight')
    
    plt.close()


def plot_temporal_access_and_footprint_per_chiplet(
    df, 
    benchmark, 
    result_dir,
    page_level=False,
    save_fig=True,
):
    import matplotlib.pyplot as plt
    import seaborn as sns
    import matplotlib.ticker as mtick
    import numpy as np
    import os

    # --- 1. 하드웨어 파라미터 및 시간 Binning ---
    freq_hz = 10**9  # 1GHz
    cycles_per_interval = 10000
    interval_sec = cycles_per_interval / freq_hz 

    if page_level:
        df = df.copy()
        df['addr'] = df['addr'].values >> 12
        result_dir += "-page-level"
        os.makedirs(result_dir, exist_ok=True)

    # 시간대별 bin 생성
    df['time_bin'] = (df['time'] // interval_sec) * interval_sec
    
    # 먼저 각 시간대별 + 각 칩렛(owner)별로 통계를 집계합니다.
    # 예: (Time 0, Chiplet A) -> 100 access, (Time 0, Chiplet B) -> 200 access
    temporal_stats = df.groupby(['time_bin', 'owner']).agg(
        access_count=('addr', 'size'),      # 전체 접근 횟수 (Total Accesses)
        memory_footprint=('addr', 'nunique') # 고유 주소 개수 (Unique Addresses)
    ).reset_index()
    
    # 그래프 X축을 위해 cycles로 변환
    temporal_stats['cycles'] = (temporal_stats['time_bin'] * freq_hz).astype(int)

    # --- [핵심 수정: 각 시간별 '칩렛들의 평균' 구하기] ---
    # 각 시간(cycles)에 대해 존재하는 칩렛들의 수치들을 평균(mean) 냅니다.
    # 예: Time 0에 Chiplet A(100), B(200)가 있었다면 이 시점의 값은 150이 됩니다.
    plot_df = temporal_stats.groupby('cycles').agg({
        'access_count': 'mean', 
        'memory_footprint': 'mean'
    }).reset_index().sort_values('cycles')

    # --- 2. 시각화 설정 ---
    plt.style.use('seaborn-v0_8-paper')
    fig, ax1 = plt.subplots(figsize=(12, 6.5), dpi=300) 

    # --- 3. 왼쪽 축 (ax1): Average Access Count per Chiplet (Bar) ---
    width = cycles_per_interval * 0.8
    bar = ax1.bar(plot_df['cycles'], plot_df['access_count'], 
                  width=width, color='#3498db', alpha=0.6, label='Avg Access Count (per Chiplet)')

    ax1.set_xlabel('Execution Time (Cycles)', fontsize=11, fontweight='bold')
    ax1.set_ylabel('Access Count per Chiplet (avg)', fontsize=11, fontweight='bold', color='#2980b9')
    ax1.tick_params(axis='y', labelcolor='#2980b9')
    ax1.xaxis.set_major_formatter(mtick.StrMethodFormatter('{x:,.0f}'))
    ax1.yaxis.set_major_formatter(mtick.StrMethodFormatter('{x:,.0f}'))

    # --- 4. 오른쪽 축 (ax2): Average Unique Cache Lines per Chiplet (Line) ---
    ax2 = ax1.twinx()
    line, = ax2.plot(plot_df['cycles'], plot_df['memory_footprint'], 
                     color='#e67e22', marker='o', markersize=3, linewidth=1.2, label='Avg Unique Cache Lines (per Chiplet)')

    ax2.set_ylabel('Unique Cache Lines per Chiplet (avg)', fontsize=11, fontweight='bold', color='#d35400')
    ax2.tick_params(axis='y', labelcolor='#d35400')
    ax2.yaxis.set_major_formatter(mtick.StrMethodFormatter('{x:,.0f}'))

    # --- 5. 범례 설정 (상단) ---
    handles = [bar, line]
    labels = [h.get_label() for h in handles]
    
    ax1.legend(handles, labels, 
               loc='lower left', 
               bbox_to_anchor=(0.0, 1.02), 
               ncol=2, 
               fontsize=10, 
               frameon=True, 
               facecolor='white', 
               edgecolor='lightgrey')
    
    # --- 6. 제목 및 디자인 마무리 ---
    plt.title(f'Temporal Memory Access Analysis [{benchmark}]\n(Avg per Chiplet, Resolution: {cycles_per_interval:,} Cycles)', 
              fontsize=14, fontweight='bold', pad=45) 

    ax1.grid(True, axis='y', linestyle='--', alpha=0.3)
    ax1.spines['top'].set_visible(False)
    ax2.spines['top'].set_visible(False)

    if save_fig:
        plt.savefig(f"{result_dir}/{benchmark}_temporal_analysis_per_chiplet.png", bbox_inches='tight')

    plt.close()


def plot_access_count_per_pages(df, benchmark, result_dir, save_fig=True):
    import matplotlib.pyplot as plt
    import seaborn as sns
    import matplotlib.ticker as mtick
    import numpy as np
    import os
    import pandas as pd

    df['page'] = df['addr'].values >> 12
    df['line_idx'] = (df['addr'].values >> 6) & 0x3F

    page_line_pivot_nan = df.groupby(['page', 'line_idx'])['access_count'].sum().unstack()

    # 2. 각 페이지(행) 내부의 '접근된' 라인들에 대해서만 통계 계산 (axis=1)
    # Pandas의 통계 함수는 기본적으로 skipna=True 이므로 NaN을 제외하고 계산합니다.
    page_stats = pd.DataFrame(index=page_line_pivot_nan.index)
    page_stats['intra_page_mean'] = page_line_pivot_nan.mean(axis=1)
    page_stats['intra_page_std'] = page_line_pivot_nan.std(axis=1)

    # 3. 추가 메트릭: 해당 페이지 내에서 몇 개의 라인이 활성화되었는가 (Density 확인용)
    page_stats['active_line_count'] = page_line_pivot_nan.count(axis=1)

    # 4. 변동 계수 (CV) 계산
    page_stats['intra_page_cv'] = page_stats['intra_page_std'] / (page_stats['intra_page_mean'] + 1e-9)

    # 0. Skewness 계산 (데이터프레임에 없는 경우를 위해 표기)
    page_stats['intra_page_skew'] = page_line_pivot_nan.skew(axis=1)

    # --- 1. 고해상도 글로벌 설정 ---
    plt.rcParams['figure.dpi'] = 300 
    plt.rcParams['font.size'] = 12 
    plt.rcParams['axes.linewidth'] = 1.2 
    plt.rcParams['font.family'] = 'sans-serif'
    plt.rcParams['font.sans-serif'] = ['DejaVu Sans', 'Arial', 'Liberation Sans', 'sans-serif']

    # --- 2. 데이터 준비 ---
    stats = page_stats.sort_index().copy()
    stats['plot_idx'] = np.arange(len(stats))

    # --- 3. 그래프 생성 ---
    fig, ax = plt.subplots(figsize=(10, 6), dpi=300, tight_layout=True)

    # X축: Page Index
    # Y축: Intra-Page Mean Access
    # 색상(c): Intra-Page Skewness (기존 std에서 변경)
    scatter = ax.scatter(stats['plot_idx'], 
                        stats['intra_page_mean'], 
                        c=stats['intra_page_skew'], # Skewness 적용
                        cmap='plasma',     
                        alpha=0.5,        
                        s=15,              
                        edgecolors='none') 

    # --- 4. 축 레이블 및 타이틀 ---
    ax.set_xlabel('Page Index (Ordered by Address Space)', fontsize=12, fontweight='bold', labelpad=12)
    ax.set_ylabel('Intra-Page Mean Access Count', fontsize=12, fontweight='bold', labelpad=12)
    ax.set_title('Spatial Access Locality: Hotness vs. Internal Skewness per Page', 
                fontsize=18, fontweight='bold', pad=25)

    # 축 눈금 스타일
    ax.xaxis.set_major_formatter(mtick.StrMethodFormatter('{x:,.0f}'))
    ax.tick_params(axis='both', which='major', labelsize=13)
    ax.grid(True, linestyle='--', alpha=0.3, color='#e0e0e0')

    # --- 5. 컬러바 설정 (Skewness 표시) ---
    cbar = fig.colorbar(scatter, ax=ax, pad=0.03, aspect=30)
    # 레이블을 Skewness로 변경
    cbar.set_label('Intra-Page Skewness (Asymmetry)', fontsize=12, fontweight='bold', labelpad=12)
    cbar.ax.tick_params(labelsize=11)
    cbar.solids.set_alpha(1.0) 

    # --- 6. 디자인 정리 ---
    ax.spines['top'].set_visible(False)
    ax.spines['right'].set_visible(False)
    ax.set_ylim(bottom=0)

    if save_fig:
        plt.savefig(f"{result_dir}/{benchmark}_access_count_per_pages.png", bbox_inches='tight')