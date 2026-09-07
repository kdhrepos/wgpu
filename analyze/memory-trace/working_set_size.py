"""벤치마크별 / 칩렛별 working-set 크기(WSS)를 메모리 트레이스에서 계산.

핵심 개념 (한 줄):
    WSS(N) = "N 사이클 동안 한 칩렛이 건드리는 서로 다른 cacheline 개수의 평균".

직관:
  - 시간축을 N 사이클짜리 조각(window)으로 자른다.
  - 각 window 안에서 touch된 고유 cacheline 수를 센다 (= 그 순간 캐시에 동시에
    살아있어야 하는 데이터 양).
  - 모든 window에 대해 평균낸 값이 WSS(N).
  - N을 키우면 WSS도 커지다가 어느 지점부터 평평해진다.
        평평해지는 지점(knee) = 그 벤치마크의 진짜 working set 크기.
        knee 너머로도 계속 커지면 = streaming (재사용 없이 새 데이터만 훑음).

thrashing 판정:
  - 칩렛당 L2 = 4 MB (16-way, 128KB x 16bank). builders/common.go:466.
  - WSS(knee) > 4 MB  ->  working set이 L2에 안 들어감 = capacity thrashing.
  - 플롯의 빨간 점선이 4 MB 기준선.

window 단위:
  - 기본 1 GHz 가정 -> 1 cycle = 1 ns. 트레이스 time(초) * 1e9 = cycle.
  - 모든 칩렛이 같은 시간축을 공유하므로 "같은 시간 조각에 동시에 살아있는 양"을
    재는 것 = 하드웨어/캐시 관점의 working set.

집계 단위 = chiplet 컬럼 (= 그 주소에 요청을 보낸 칩렛, 요청자 관점).
  owner(=home node) 가 아니라 chiplet(=요청자) 기준으로 본다.

실행:
    analyze/run.sh memory-trace/working_set_size.py mesh-6x6/latest
    analyze/run.sh memory-trace/working_set_size.py <run-id> --data-only
    analyze/run.sh memory-trace/working_set_size.py <run-id> --windows 1K,16K,256K
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

LINE_BYTES = 64                       # cacheline = 64 B
L2_CAPACITY_MB = 4.0                  # 칩렛당 L2 용량 (thrashing 기준선)

# 스윕할 window 크기들 (cycle). 1GHz면 256ns ~ 4ms.
DEFAULT_WINDOWS = [1 << e for e in (8, 10, 12, 14, 16, 18, 20, 22)]

# __main__ 에서 세팅, fork 로 워커에 상속.
TRACE_DIR = None
XDIM = YDIM = 6
WINDOWS = DEFAULT_WINDOWS
DATA_ONLY = False
FREQ_GHZ = 1.0


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
    """한 칩렛의 (cacheline 시퀀스, cycle 시퀀스) -> window별 평균/최대 WSS + footprint.

    blocks, cycles : 시간순 1D 배열.
    return         : ({N: (평균_WSS, 최대_WSS)}, footprint)
                     단위는 '고유 cacheline 개수' (MB 변환은 호출부에서).

    footprint = 전체 실행 동안의 고유 cacheline 수 (window -> 무한대 극한).
    각 window N 마다:
      - 시간축을 N 사이클 단위로 자르고 (window_id = cycle // N)
      - 같은 window 안의 고유 block 수를 센 뒤
      - 모든 window에 대해 평균(.mean) / 최대(.max)를 취함.
    """
    n_access = len(blocks)
    footprint = int(np.unique(blocks).size) if n_access else 0

    out = {}
    for N in WINDOWS:
        if n_access == 0:
            out[N] = (0.0, 0.0)
            continue
        # 전체 실행이 window 하나에 다 들어가면 WSS == footprint.
        if cycles[-1] // N == cycles[0] // N:
            out[N] = (float(footprint), float(footprint))
            continue
        window_id = cycles // N
        unique_per_window = pd.Series(blocks).groupby(window_id).nunique()
        out[N] = (float(unique_per_window.mean()),
                  float(unique_per_window.max()))
    return out, footprint


def find_knee(wss_mb_per_window, grow_threshold=0.10):
    """WSS 곡선이 '평평해지는' 첫 지점 = working set 크기.

    다음 window로 넘어갈 때 WSS 증가율이 grow_threshold(기본 10%) 미만이면
    그 지점을 knee 로 본다. 끝까지 계속 자라면(streaming) 마지막 window 반환.
    return: (knee_window, knee_wss_mb)
    """
    for i in range(len(WINDOWS) - 1):
        cur, nxt = wss_mb_per_window[i], wss_mb_per_window[i + 1]
        if cur > 0 and (nxt - cur) / cur < grow_threshold:
            return WINDOWS[i], cur
    return WINDOWS[-1], wss_mb_per_window[-1]


# ----------------------------------------------------- 벤치마크 1개 처리
def process_benchmark(benchmark):
    trace = f"{TRACE_DIR}/{benchmark}_mesh_{XDIM}x{YDIM}_memory_trace.csv"
    if not os.path.exists(trace):
        return None
    print(f"[{benchmark}] WSS 계산...", flush=True)

    cols = ['time', 'addr', 'chiplet', 'type'] + (['is_ptw'] if DATA_ONLY else [])
    dtype = {'time': 'float64', 'addr': 'uint64',
             'chiplet': 'int16', 'type': 'int8'}
    if DATA_ONLY:
        dtype['is_ptw'] = 'int8'
    try:
        df = pd.read_csv(trace, usecols=cols, dtype=dtype, skipinitialspace=True)
        df = df.drop_duplicates()          # full-row 완전-중복 제거
    except Exception as e:
        print(f"[{benchmark}] 읽기 실패: {e}")
        return None
    if df.empty:
        return None

    # demand access 만: type 6(evict)/7(overwrite) 는 캐시 내부 이벤트라 제외.
    df = df[~df['type'].isin([6, 7, 8, 9])]
    if DATA_ONLY:                         # PTE walk 접근 제거 -> 순수 데이터 WSS
        df = df[df['is_ptw'] == 0]
    if df.empty:
        return None

    df['block'] = df['addr'].to_numpy() >> 6                  # addr -> cacheline
    df['cycle'] = (df['time'].to_numpy() * FREQ_GHZ * 1e9).astype('int64')

    # 칩렛별로 따로 WSS 곡선을 구한 뒤 칩렛 평균을 낸다.
    per_chip_mean = {N: [] for N in WINDOWS}
    per_chip_peak = {N: [] for N in WINDOWS}
    per_chip_foot = []
    for _, g in df.groupby('chiplet', sort=False):
        g = g.sort_values('time')
        wss, foot = wss_curve(g['block'].to_numpy(), g['cycle'].to_numpy())
        for N in WINDOWS:
            per_chip_mean[N].append(wss[N][0])
            per_chip_peak[N].append(wss[N][1])
        per_chip_foot.append(foot)

    row = {'benchmark': benchmark, 'n_chiplets': len(per_chip_foot)}
    mean_curve_mb = []
    for N in WINDOWS:
        m = blocks_to_mb(np.mean(per_chip_mean[N]))
        mean_curve_mb.append(m)
        row[f'wss_N{N}_mb'] = round(m, 4)
        row[f'wss_peak_N{N}_mb'] = round(blocks_to_mb(np.mean(per_chip_peak[N])), 4)
    row['footprint_mb'] = round(blocks_to_mb(np.mean(per_chip_foot)), 4)

    knee_n, knee_mb = find_knee(mean_curve_mb)
    row['knee_N'] = knee_n
    row['working_set_mb'] = round(knee_mb, 4)
    row['%_of_footprint'] = (round(knee_mb / row['footprint_mb'] * 100, 1)
                             if row['footprint_mb'] else 0.0)
    # working set 이 L2(4MB)에 들어가는가 -> thrashing 여부.
    row['fits_L2'] = knee_mb <= L2_CAPACITY_MB
    row['thrash_x'] = round(knee_mb / L2_CAPACITY_MB, 2)      # >1 이면 thrashing
    return row


# ------------------------------------------------------------------- 플롯
def plot_curves(rows, out_png):
    try:
        import matplotlib
        matplotlib.use('Agg')
        import matplotlib.pyplot as plt
    except Exception as e:
        print(f"플롯 생략 ({e})")
        return
    plt.figure(figsize=(11, 7))
    for r in rows:
        ys = [r[f'wss_N{N}_mb'] for N in WINDOWS]
        plt.plot(WINDOWS, ys, marker='o', ms=4, lw=1.2, label=r['benchmark'])
        plt.scatter([r['knee_N']], [r['working_set_mb']],
                    marker='x', s=55, c='k', zorder=5)
    plt.axhline(L2_CAPACITY_MB, color='red', ls='--', lw=1.4,
                label=f'L2 = {L2_CAPACITY_MB:g} MB (above = thrashing)')
    plt.xscale('log', base=10)
    plt.yscale('log', base=10)
    plt.xlabel('window size N (cycles, log10)')
    plt.ylabel('WSS (MB, per-chiplet mean, log10)')
    plt.title('Working-set size vs window  (x = knee = working set, red = L2 capacity)')
    plt.grid(True, which='both', alpha=0.3)
    plt.legend(fontsize=8, ncol=2)
    plt.tight_layout()
    plt.savefig(out_png, dpi=400)
    plt.close()
    print(f"플롯 저장: {out_png}")


if __name__ == "__main__":
    ap = argparse.ArgumentParser(
        description="벤치마크별/칩렛별 cycle-window working-set 크기.")
    ap.add_argument('run_id', nargs='?', default='mesh-6x6',
                    help="run-id (_paths.run_root 가 해석. 기본 mesh-6x6 최신)")
    ap.add_argument('--windows', type=str, default=None,
                    help="window 크기 목록(cycle), 예: 1K,16K,256K (기본 256..4M)")
    ap.add_argument('--freq-ghz', type=float, default=1.0,
                    help="cycle<->time 변환 클럭 (기본 1.0 GHz: 1 cycle = 1 ns)")
    ap.add_argument('--data-only', action='store_true',
                    help="PTE 접근(is_ptw==1) 제거 -> 순수 데이터 working set")
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
    DATA_ONLY = args.data_only
    FREQ_GHZ = args.freq_ghz

    print(f"Run:     {config}/{ts}")
    print(f"Trace:   {TRACE_DIR}")
    print(f"Grid:    {XDIM}x{YDIM}   data-only={DATA_ONLY}   freq={FREQ_GHZ} GHz")
    print(f"Windows: {WINDOWS}")
    print(f"Workers: {args.workers}")

    results = []
    with ProcessPoolExecutor(max_workers=args.workers) as ex:
        for r in ex.map(process_benchmark, benchmarks):
            if r is not None:
                results.append(r)
    if not results:
        print("결과 없음.")
        sys.exit(0)
    results.sort(key=lambda r: r['benchmark'])

    out_dir = os.path.abspath(os.path.join(
        os.path.dirname(__file__), "..", "_out", "working_set", f"{config}-{ts}"))
    os.makedirs(out_dir, exist_ok=True)
    suffix = "_data" if DATA_ONLY else ""
    xlsx = os.path.join(out_dir, f"working_set_size{suffix}.xlsx")
    png = os.path.join(out_dir, f"working_set_size{suffix}.png")

    df = pd.DataFrame(results)
    df.to_excel(xlsx, index=False)
    plot_curves(results, png)

    summary = ['benchmark', 'footprint_mb', 'working_set_mb',
               '%_of_footprint', 'fits_L2', 'thrash_x']
    print("\n" + "=" * 78)
    print(df[summary].to_string(index=False))
    print("=" * 78)
    print("thrash_x = working_set / 4MB.  >1 이면 L2 thrashing.")
    print(f"전체 표: {xlsx}")