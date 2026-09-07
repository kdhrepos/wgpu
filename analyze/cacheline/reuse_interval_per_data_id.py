"""data_id(버퍼)별 reuse interval(재사용 간격)을 메모리 트레이스에서 계산 + 벤치마크당 figure.

working_set_size_per_data_id.py 의 형제. WSS 대신 reuse interval 을 본다.

reuse interval 정의 (기존 pc_cacheline_reuse_interval.py 와 동일):
    같은 (칩렛, cacheline) 을 연속으로 두 번 건드릴 때, 그 사이의 cycle 수.
    작을수록 = 짧은 시간 안에 다시 씀 = temporal locality 좋음 (캐시에 살아있을 때 재사용).
    클수록 = 오래 뒤에 다시 씀 = 그 사이 evict 되기 쉬움 (재사용 못 살림).

집계 단위 = 요청자 칩렛(chiplet). WSS 스크립트와 동일하게 칩렛 자기 스트림 안에서만
연속 접근을 본다. cacheline 은 data_id 하나에만 속하므로 data_id 로 자연히 쪼개진다.

figure = reuse interval CDF (가장 직관적·표준). 벤치마크당 1장, data_id별 곡선:
    x = reuse interval (cycles, log10),  y = 누적 비율 (interval <= x 인 reuse 표본 비율).
    곡선이 왼쪽 = 재사용 간격 짧음(locality 좋음), 오른쪽 = 김(evict 위험).
    각 곡선의 median 을 x 마커로 표시.

출력:
    _out/cacheline/<config>-<ts>/reuse-per-data/<benchmark>.png   (벤치마크당 1장)
    _out/cacheline/<config>-<ts>/reuse-per-data/reuse_interval_per_data_id.xlsx

기본은 데이터 접근만 (is_ptw==1, data_id==-1 제거).

실행:
    analyze/run.sh cacheline/reuse_interval_per_data_id.py mesh-6x6/latest
    analyze/run.sh cacheline/reuse_interval_per_data_id.py <run-id> --top 6
"""

import os
import re
import sys
import argparse
import numpy as np
import pandas as pd
from concurrent.futures import ProcessPoolExecutor

sys.path.append(os.path.abspath(os.path.join(os.path.dirname(__file__), "../common")))
sys.path.append(os.path.abspath(os.path.join(os.path.dirname(__file__), "../../scripts")))

from _paths import run_root, memory_trace_dir, run_config, run_tag
from params import benchmarks

# __main__ 에서 세팅, fork 로 워커에 상속.
TRACE_DIR = None
OUT_DIR = None
XDIM = YDIM = 6
FREQ_GHZ = 1.0
INCLUDE_PTW = False
INCLUDE_UNMATCHED = False
TOP_N = 0                             # 0 = 전 data_id, >0 = 표본 상위 N개만
CDF_POINTS = 400                      # CDF 곡선 샘플 점 개수 (quantile grid)


def _fmt_count(n):
    """1234567 -> 1.2M, 12345 -> 12.3K."""
    if n >= 1_000_000:
        return f"{n / 1_000_000:.1f}M"
    if n >= 1_000:
        return f"{n / 1_000:.1f}K"
    return str(int(n))


# ------------------------------------- 한 data_id 의 interval 배열 -> 통계 + CDF
def _percentiles(iv):
    """정렬 안 된 interval 배열 -> 통계 dict."""
    return {
        'n': int(iv.size),
        'min': int(iv.min()),
        'p25': float(np.percentile(iv, 25)),
        'median': float(np.percentile(iv, 50)),
        'p75': float(np.percentile(iv, 75)),
        'p90': float(np.percentile(iv, 90)),
        'p99': float(np.percentile(iv, 99)),
        'mean': float(iv.mean()),
        'max': int(iv.max()),
    }


def _cdf_xy(iv):
    """interval 배열 -> (x, y) CDF 곡선. x=quantile 위치, y=누적비율."""
    qs = np.linspace(0.0, 1.0, CDF_POINTS)
    xs = np.quantile(iv, qs)
    xs = np.clip(xs, 1, None)          # log 축용: 0 cycle(동시 재사용) -> 1 로 바닥
    return xs, qs


# ----------------------------------------------------- 벤치마크 1개 처리
def process_benchmark(benchmark):
    trace = f"{TRACE_DIR}/{benchmark}_mesh_{XDIM}x{YDIM}_memory_trace.csv"
    if not os.path.exists(trace):
        return None
    print(f"[{benchmark}] per-data_id reuse interval 계산...", flush=True)

    cols = ['time', 'addr', 'chiplet', 'type', 'data_id']
    dtype = {'time': 'float64', 'addr': 'uint64', 'chiplet': 'int16',
             'type': 'int8', 'data_id': 'int32'}
    if not INCLUDE_PTW:
        cols.append('is_ptw')
        dtype['is_ptw'] = 'int8'
    try:
        df = pd.read_csv(trace, dtype=dtype, skipinitialspace=True)
        df = df.drop_duplicates()
        df = df[cols]
    except Exception as e:
        print(f"[{benchmark}] 읽기 실패: {e}")
        return None
    if df.empty:
        return None

    # demand data access 만.
    df = df[~df['type'].isin([6, 7, 8, 9])]
    if not INCLUDE_PTW:
        df = df[df['is_ptw'] == 0]
    if not INCLUDE_UNMATCHED:
        df = df[df['data_id'] != -1]
    if df.empty:
        return None

    df['cacheline'] = df['addr'].to_numpy() >> 6
    df['cycle'] = (df['time'].to_numpy() * FREQ_GHZ * 1e9).astype('int64')

    # 같은 (칩렛, cacheline) 연속 접근 사이 cycle 간격.
    df = df.sort_values(['chiplet', 'cacheline', 'cycle'])
    df['prev'] = df.groupby(['chiplet', 'cacheline'], sort=False)['cycle'].shift(1)
    reuse = df.dropna(subset=['prev'])
    if reuse.empty:
        return None
    interval = (reuse['cycle'].to_numpy() - reuse['prev'].to_numpy()).astype('int64')
    data_id = reuse['data_id'].to_numpy()

    curves, rows = [], []
    for did in np.unique(data_id):
        iv = interval[data_id == did]
        if iv.size == 0:
            continue
        stats = _percentiles(iv)
        xs, ys = _cdf_xy(iv)
        curves.append({'data_id': int(did), 'xs': xs, 'ys': ys,
                       'median': stats['median'], 'n': stats['n']})
        rows.append({'benchmark': benchmark, 'data_id': int(did),
                     'reuse_samples': stats['n'],
                     'min_cy': stats['min'], 'p25_cy': round(stats['p25'], 1),
                     'median_cy': round(stats['median'], 1),
                     'p75_cy': round(stats['p75'], 1), 'p90_cy': round(stats['p90'], 1),
                     'p99_cy': round(stats['p99'], 1), 'mean_cy': round(stats['mean'], 1),
                     'max_cy': stats['max']})

    # 표본 큰 순 정렬. --top 이면 상위 N개만 플롯.
    order = sorted(range(len(curves)), key=lambda i: curves[i]['n'], reverse=True)
    curves = [curves[i] for i in order]
    plot_curves = curves[:TOP_N] if TOP_N > 0 else curves

    plot_benchmark(benchmark, plot_curves)
    return rows


# ------------------------------------------------------------------- 플롯
def plot_benchmark(benchmark, curves):
    try:
        import matplotlib
        matplotlib.use('Agg')
        import matplotlib.pyplot as plt
    except Exception as e:
        print(f"플롯 생략 ({e})")
        return
    if not curves:
        return

    cmap = plt.get_cmap('tab10')
    plt.figure(figsize=(11, 7))
    for i, c in enumerate(curves):
        color = cmap(i % 10)
        label = (f"data_id {c['data_id']} "
                 f"(med={int(c['median'])} cy, n={_fmt_count(c['n'])})")
        plt.plot(c['xs'], c['ys'], lw=1.6, color=color, label=label)
        # median 마커 (y=0.5 지점).
        plt.scatter([max(c['median'], 1)], [0.5], marker='x', s=55, c='k', zorder=5)
    plt.axhline(0.5, color='grey', ls=':', lw=1.0, alpha=0.7)
    plt.xscale('log', base=10)
    plt.ylim(0, 1)
    plt.xlabel('reuse interval (cycles, log10)')
    plt.ylabel('CDF  (fraction of reuses with interval <= x)')
    plt.title(f'{benchmark}: reuse-interval CDF per data_id  '
              f'(x = median; left = better locality)')
    plt.grid(True, which='both', alpha=0.3)
    plt.legend(fontsize=8, ncol=2, loc='lower right')
    plt.tight_layout()
    out_png = os.path.join(OUT_DIR, f"{benchmark}.png")
    plt.savefig(out_png, dpi=400)
    plt.close()
    print(f"[{benchmark}] 플롯 저장: {out_png}", flush=True)


if __name__ == "__main__":
    ap = argparse.ArgumentParser(
        description="data_id(버퍼)별 reuse interval CDF + 벤치마크당 figure.")
    ap.add_argument('run_id', nargs='?', default='mesh-6x6',
                    help="run-id (_paths.run_root 가 해석. 기본 mesh-6x6 최신)")
    ap.add_argument('--freq-ghz', type=float, default=1.0,
                    help="cycle<->time 변환 클럭 (기본 1.0 GHz: 1 cycle = 1 ns)")
    ap.add_argument('--top', type=int, default=0,
                    help="벤치마크당 표본 상위 N개 data_id만 플롯 (0=전부)")
    ap.add_argument('--include-ptw', action='store_true',
                    help="PTE walk 접근(is_ptw==1) 포함 (기본 제거)")
    ap.add_argument('--include-unmatched', action='store_true',
                    help="data_id==-1 (미매칭) 포함 (기본 제거)")
    ap.add_argument('--workers', type=int,
                    default=int(os.environ.get("FOOTPRINT_WORKERS", "16")))
    args = ap.parse_args()

    root = run_root(args.run_id)
    TRACE_DIR = str(memory_trace_dir(args.run_id))
    config, ts = run_config(args.run_id), run_tag(args.run_id)
    m = re.search(r'(\d+)x(\d+)', config)
    if m:
        XDIM, YDIM = int(m.group(1)), int(m.group(2))
    FREQ_GHZ = args.freq_ghz
    INCLUDE_PTW = args.include_ptw
    INCLUDE_UNMATCHED = args.include_unmatched
    TOP_N = args.top

    OUT_DIR = os.path.abspath(os.path.join(
        os.path.dirname(__file__), "..", "_out", "cacheline",
        f"{config}-{ts}", "reuse-per-data"))
    os.makedirs(OUT_DIR, exist_ok=True)

    print(f"Run:     {config}/{ts}")
    print(f"Trace:   {TRACE_DIR}")
    print(f"Out:     {OUT_DIR}")
    print(f"Grid:    {XDIM}x{YDIM}   include_ptw={INCLUDE_PTW}   "
          f"include_unmatched={INCLUDE_UNMATCHED}   top={TOP_N or 'all'}")
    print(f"Workers: {args.workers}")

    all_rows = []
    with ProcessPoolExecutor(max_workers=args.workers) as ex:
        for r in ex.map(process_benchmark, benchmarks):
            if r:
                all_rows.extend(r)
    if not all_rows:
        print("결과 없음.")
        sys.exit(0)

    df = pd.DataFrame(all_rows).sort_values(
        ['benchmark', 'reuse_samples'], ascending=[True, False])
    xlsx = os.path.join(OUT_DIR, "reuse_interval_per_data_id.xlsx")
    df.to_excel(xlsx, index=False)

    summary = ['benchmark', 'data_id', 'reuse_samples',
               'median_cy', 'p90_cy', 'p99_cy', 'mean_cy']
    print("\n" + "=" * 78)
    print(df[summary].to_string(index=False))
    print("=" * 78)
    print("median_cy 작을수록 temporal locality 좋음 (재사용 간격 짧음).")
    print(f"전체 표: {xlsx}")
