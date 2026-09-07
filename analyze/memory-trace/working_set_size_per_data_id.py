"""data_id(버퍼)별 working-set 크기(WSS)를 메모리 트레이스에서 계산 + 벤치마크당 figure.

working_set_size.py 의 형제 스크립트.
  - working_set_size.py       : 벤치마크당 곡선 1개 (전체 데이터 합).
  - working_set_size_per_data_id.py : 벤치마크당 figure 1개, 그 안에 data_id별 곡선.

WSS(N) 정의는 동일:
    "N 사이클 window 안에서 한 칩렛이 건드리는 서로 다른 cacheline 개수의 평균".
    여기서는 그것을 data_id(=할당 버퍼) 단위로 쪼갠다.
    -> 각 data_id 안에서 칩렛별 WSS 곡선을 구하고 칩렛 평균을 낸다.

knee(평평해지는 지점) = 그 버퍼의 진짜 working set 크기.
빨간 점선 = 칩렛당 L2 = 4 MB. 곡선이 그 위면 그 버퍼 하나만으로 L2 thrashing.

출력:
    _out/working_set/<config>-<ts>/wss-per-data/<benchmark>.png   (벤치마크당 1장)
    _out/working_set/<config>-<ts>/wss-per-data/working_set_per_data_id.xlsx

기본은 데이터 접근만 (is_ptw==1, data_id==-1 제거). 플래그로 포함 가능.

실행:
    analyze/run.sh memory-trace/working_set_size_per_data_id.py mesh-6x6/latest
    analyze/run.sh memory-trace/working_set_size_per_data_id.py <run-id> --windows 1K,16K,256K
    analyze/run.sh memory-trace/working_set_size_per_data_id.py <run-id> --top 6
"""

import os
import re
import sys
import argparse
import numpy as np
import pandas as pd
from concurrent.futures import ProcessPoolExecutor
from itertools import repeat

sys.path.append(os.path.abspath(os.path.join(os.path.dirname(__file__), "../common")))
sys.path.append(os.path.abspath(os.path.join(os.path.dirname(__file__), "../../scripts")))

from _paths import run_root, memory_trace_dir, run_config, run_tag
from params import benchmarks

LINE_BYTES = 64                       # cacheline = 64 B
L2_CAPACITY_MB = 4.0                  # 칩렛당 L2 용량 (thrashing 기준선)

# 스윕할 window 크기들 (cycle). 1GHz면 256ns ~ 4ms.
DEFAULT_WINDOWS = [1 << e for e in (8, 10, 12, 14, 16, 18, 20, 22)]

# __main__ 에서 세팅, fork 로 워커에 상속.
TRACE_DIR = None
OUT_DIR = None
XDIM = YDIM = 6
WINDOWS = DEFAULT_WINDOWS
FREQ_GHZ = 1.0
INCLUDE_PTW = False
INCLUDE_UNMATCHED = False
TOP_N = 0                             # 0 = 전 data_id, >0 = footprint 상위 N개만


# ---------------------------------------------------------------- 단위 변환
def _parse_window(tok):
    """'16K' -> 16384, '4M' -> 4194304."""
    tok = tok.strip().upper()
    mult = 1
    if tok.endswith('K'):
        mult, tok = 1024, tok[:-1]
    elif tok.endswith('M'):
        mult, tok = 1024 * 1024, tok[:-1]
    return int(float(tok) * mult)


def blocks_to_mb(n_blocks):
    return n_blocks * LINE_BYTES / (1024 * 1024)


# ------------------------------------------------ 한 칩렛 스트림의 WSS 계산
def wss_curve(blocks, cycles):
    """한 칩렛의 (cacheline 시퀀스, cycle 시퀀스) -> window별 평균 WSS + footprint.

    blocks, cycles : 시간순 1D 배열.
    return         : ({N: 평균_WSS}, footprint)   단위는 '고유 cacheline 개수'.
    """
    n_access = len(blocks)
    footprint = int(np.unique(blocks).size) if n_access else 0

    out = {}
    for N in WINDOWS:
        if n_access == 0:
            out[N] = 0.0
            continue
        # 전체 실행이 window 하나에 다 들어가면 WSS == footprint.
        if cycles[-1] // N == cycles[0] // N:
            out[N] = float(footprint)
            continue
        window_id = cycles // N
        unique_per_window = pd.Series(blocks).groupby(window_id).nunique()
        out[N] = float(unique_per_window.mean())
    return out, footprint


def find_knee(wss_mb_per_window, grow_threshold=0.10):
    """WSS 곡선이 '평평해지는' 첫 지점 = working set 크기.

    다음 window로 넘어갈 때 증가율이 grow_threshold(기본 10%) 미만이면 knee.
    끝까지 자라면(streaming) 마지막 window 반환.  return: (knee_window, knee_wss_mb)
    """
    for i in range(len(WINDOWS) - 1):
        cur, nxt = wss_mb_per_window[i], wss_mb_per_window[i + 1]
        if cur > 0 and (nxt - cur) / cur < grow_threshold:
            return WINDOWS[i], cur
    return WINDOWS[-1], wss_mb_per_window[-1]


# ------------------------------------------- 한 data_id 스트림 -> 칩렛평균 곡선
def data_id_curve(sub):
    """(한 data_id 의 rows, 이미 time-정렬) -> (평균곡선_MB[list], footprint_MB).

    칩렛별로 WSS 곡선을 구한 뒤 칩렛 평균. footprint 도 칩렛 평균.
    """
    per_chip = {N: [] for N in WINDOWS}
    per_chip_foot = []
    for _, g in sub.groupby('chiplet', sort=False):
        wss, foot = wss_curve(g['block'].to_numpy(), g['cycle'].to_numpy())
        for N in WINDOWS:
            per_chip[N].append(wss[N])
        per_chip_foot.append(foot)

    curve_mb = [blocks_to_mb(np.mean(per_chip[N])) for N in WINDOWS]
    foot_mb = blocks_to_mb(np.mean(per_chip_foot)) if per_chip_foot else 0.0
    return curve_mb, foot_mb


# ----------------------------------------------------- 벤치마크 1개 처리
def process_benchmark(benchmark):
    trace = f"{TRACE_DIR}/{benchmark}_mesh_{XDIM}x{YDIM}_memory_trace.csv"
    if not os.path.exists(trace):
        return None
    print(f"[{benchmark}] per-data_id WSS 계산...", flush=True)

    cols = ['time', 'addr', 'chiplet', 'type', 'data_id']
    dtype = {'time': 'float64', 'addr': 'uint64', 'chiplet': 'int16',
             'type': 'int8', 'data_id': 'int32'}
    if not INCLUDE_PTW:
        cols.append('is_ptw')
        dtype['is_ptw'] = 'int8'
    try:
        df = pd.read_csv(trace, usecols=cols, dtype=dtype, skipinitialspace=True)
        df = df.drop_duplicates()          # full-row 완전-중복 제거
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

    df['block'] = df['addr'].to_numpy() >> 6
    df['cycle'] = (df['time'].to_numpy() * FREQ_GHZ * 1e9).astype('int64')
    df = df.sort_values('time')                     # 한 번만 정렬 -> subgroup 순서 보존

    curves = []                                     # 플롯/표용 per-data_id 결과
    rows = []
    for data_id, sub in df.groupby('data_id', sort=True):
        curve_mb, foot_mb = data_id_curve(sub)
        knee_n, knee_mb = find_knee(curve_mb)
        curves.append({
            'data_id': int(data_id),
            'curve_mb': curve_mb,
            'footprint_mb': foot_mb,
            'knee_N': knee_n,
            'working_set_mb': knee_mb,
        })
        row = {'benchmark': benchmark, 'data_id': int(data_id),
               'n_chiplets': int(sub['chiplet'].nunique()),
               'footprint_mb': round(foot_mb, 4),
               'knee_N': knee_n, 'working_set_mb': round(knee_mb, 4),
               '%_of_footprint': (round(knee_mb / foot_mb * 100, 1) if foot_mb else 0.0),
               'fits_L2': knee_mb <= L2_CAPACITY_MB,
               'thrash_x': round(knee_mb / L2_CAPACITY_MB, 2)}
        for N in WINDOWS:
            row[f'wss_N{N}_mb'] = round(curve_mb[WINDOWS.index(N)], 4)
        rows.append(row)

    # footprint 큰 순 정렬. --top 이면 상위 N개만 플롯.
    curves.sort(key=lambda c: c['footprint_mb'], reverse=True)
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
        label = f"data_id {c['data_id']} ({c['footprint_mb']:.2g} MB)"
        plt.plot(WINDOWS, c['curve_mb'], marker='o', ms=4, lw=1.2,
                 color=color, label=label)
        plt.scatter([c['knee_N']], [c['working_set_mb']],
                    marker='x', s=55, c='k', zorder=5)
    plt.axhline(L2_CAPACITY_MB, color='red', ls='--', lw=1.4,
                label=f'L2 = {L2_CAPACITY_MB:g} MB (above = thrashing)')
    plt.xscale('log', base=10)
    plt.yscale('log', base=10)
    plt.xlabel('window size N (cycles, log10)')
    plt.ylabel('WSS (MB, per-chiplet mean, log10)')
    plt.title(f'{benchmark}: working-set size per data_id  '
              f'(x = knee = working set, red = L2 capacity)')
    plt.grid(True, which='both', alpha=0.3)
    plt.legend(fontsize=8, ncol=2)
    plt.tight_layout()
    out_png = os.path.join(OUT_DIR, f"{benchmark}.png")
    plt.savefig(out_png, dpi=400)
    plt.close()
    print(f"[{benchmark}] 플롯 저장: {out_png}", flush=True)


if __name__ == "__main__":
    ap = argparse.ArgumentParser(
        description="data_id(버퍼)별 cycle-window working-set 크기 + 벤치마크당 figure.")
    ap.add_argument('run_id', nargs='?', default='mesh-6x6',
                    help="run-id (_paths.run_root 가 해석. 기본 mesh-6x6 최신)")
    ap.add_argument('--windows', type=str, default=None,
                    help="window 크기 목록(cycle), 예: 1K,16K,256K (기본 256..4M)")
    ap.add_argument('--freq-ghz', type=float, default=1.0,
                    help="cycle<->time 변환 클럭 (기본 1.0 GHz: 1 cycle = 1 ns)")
    ap.add_argument('--top', type=int, default=0,
                    help="벤치마크당 footprint 상위 N개 data_id만 플롯 (0=전부)")
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
    if args.windows:
        WINDOWS = [_parse_window(x) for x in args.windows.split(',')]
    FREQ_GHZ = args.freq_ghz
    INCLUDE_PTW = args.include_ptw
    INCLUDE_UNMATCHED = args.include_unmatched
    TOP_N = args.top

    OUT_DIR = os.path.abspath(os.path.join(
        os.path.dirname(__file__), "..", "_out", "working_set",
        f"{config}-{ts}", "wss-per-data"))
    os.makedirs(OUT_DIR, exist_ok=True)

    print(f"Run:     {config}/{ts}")
    print(f"Trace:   {TRACE_DIR}")
    print(f"Out:     {OUT_DIR}")
    print(f"Grid:    {XDIM}x{YDIM}   include_ptw={INCLUDE_PTW}   "
          f"include_unmatched={INCLUDE_UNMATCHED}   top={TOP_N or 'all'}")
    print(f"Windows: {WINDOWS}")
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
        ['benchmark', 'footprint_mb'], ascending=[True, False])
    xlsx = os.path.join(OUT_DIR, "working_set_per_data_id.xlsx")
    df.to_excel(xlsx, index=False)

    summary = ['benchmark', 'data_id', 'footprint_mb', 'working_set_mb',
               '%_of_footprint', 'fits_L2', 'thrash_x']
    print("\n" + "=" * 78)
    print(df[summary].to_string(index=False))
    print("=" * 78)
    print("thrash_x = working_set / 4MB.  >1 이면 그 버퍼만으로 L2 thrashing.")
    print(f"전체 표: {xlsx}")
