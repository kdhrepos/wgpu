"""Miss Ratio Curve (MRC) per benchmark via LRU stack distance.

정의:
    각 cache 크기 C(라인 단위)에 대해:
        miss_rate_warm(C)  = |{ warm ref : SD >= C }| / (warm refs)
        miss_rate_total(C) = (warm misses + cold refs) / (all refs)
    이는 fully-associative LRU 캐시 크기 C에서의 이론적 miss rate.

- 스트림: **per-chiplet** 시간순 (wss_capacity.py와 동일 축).
    각 칩렛에서 miss rate 계산 → 칩렛 개수로 산술평균.
- 라인 단위: 64B.
- shared: 전 실행 중 서로 다른 칩렛 ≥ 2가 접근한 라인.
- 필터: type 6/7/8/9 제외.
- 캐시 크기 격자: 0.25, 0.5, 1, 2, 4, 8, 16, 32, 64, 128, 256 MB.

출력:
    {benchmark}_mrc.csv         : long. cache_size_MB × category(all/shared/private).
                                  칩렛 평균 miss_rate_warm/miss_rate_total.
    {benchmark}_mrc_by_chip.csv : 칩렛별 상세 (분포 확인용).
    _all_mrc.csv                : 전 벤치마크 concat (motivation figure용).

실행:
    analyze/run.sh memory-trace/mrc.py mesh-6x6/latest
    analyze/run.sh memory-trace/mrc.py <run-id> --workers 8
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

LINE_SHIFT = 6
LINE_MB = 64.0 / (1024.0 * 1024.0)

# 캐시 크기 격자 (MB) → 라인 수
CACHE_SIZES_MB = [0.25, 0.5, 1, 2, 4, 8, 16, 32, 64, 128, 256]
CACHE_SIZES_LINES = np.array(
    [int(round(s / LINE_MB)) for s in CACHE_SIZES_MB], dtype=np.int64)

TRACE_DIR = None
XDIM = YDIM = 6
OUT_DIR = None

CATEGORIES = ('all', 'shared', 'private')


# --------------------- BIT-based LRU stack distance -------------------------
def compute_stack_distance(blocks: np.ndarray) -> np.ndarray:
    """LRU stack distance per access. cold = -1. O(N log N) Fenwick tree."""
    n = len(blocks)
    distances = np.full(n, -1, dtype=np.int64)
    size = n + 2
    bit = np.zeros(size, dtype=np.int64)
    last_pos = {}

    def upd(i, v):
        i += 1
        while i < size:
            bit[i] += v
            i += i & -i

    def qry(i):
        i += 1
        s = 0
        while i > 0:
            s += bit[i]
            i -= i & -i
        return s

    for i in range(n):
        b = int(blocks[i])
        if b in last_pos:
            prev = last_pos[b]
            distances[i] = qry(i - 1) - qry(prev)
            upd(prev, -1)
        upd(i, 1)
        last_pos[b] = i
    return distances


# ---------------- 한 스트림의 miss-rate curve ------------------------------
def curve_for_stream(sd_arr: np.ndarray, cold_cnt: int):
    """sd_arr = warm SD (>=0). cold_cnt = 그 스트림의 cold miss 수.
    returns dict[size_MB] = (miss_rate_warm, miss_rate_total)."""
    warm_n = int(len(sd_arr))
    total_n = warm_n + cold_cnt
    if total_n == 0:
        return None
    if warm_n == 0:
        return {mb: (float('nan'), 1.0) for mb in CACHE_SIZES_MB}
    sorted_sd = np.sort(sd_arr)
    out = {}
    for mb, cl in zip(CACHE_SIZES_MB, CACHE_SIZES_LINES):
        # SD < cl → hit. SD >= cl → miss.
        hits_warm = int(np.searchsorted(sorted_sd, cl, side='left'))
        miss_warm = warm_n - hits_warm
        miss_rate_warm = miss_warm / warm_n
        miss_rate_total = (miss_warm + cold_cnt) / total_n
        out[mb] = (miss_rate_warm, miss_rate_total)
    return out


# ----------------------------- 벤치마크 1개 ---------------------------------
def process_benchmark(benchmark):
    trace = f"{TRACE_DIR}/{benchmark}_mesh_{XDIM}x{YDIM}_memory_trace.csv"
    if not os.path.exists(trace):
        return f"[{benchmark}] no trace"

    out_csv = f"{OUT_DIR}/{benchmark}_mrc.csv"
    out_chip_csv = f"{OUT_DIR}/{benchmark}_mrc_by_chip.csv"

    print(f"[{benchmark}] load...", flush=True)
    cols = ['time', 'addr', 'pc', 'type', 'chiplet']
    dtype = {'time': 'float64', 'addr': 'uint64', 'pc': 'uint64',
             'type': 'int8', 'chiplet': 'int16'}
    try:
        df = pd.read_csv(trace, usecols=cols, dtype=dtype,
                         skipinitialspace=True)
    except Exception as e:
        return f"[{benchmark}] read fail: {e}"

    df = df[~df['type'].isin([6, 7, 8, 9])]
    if df.empty:
        return f"[{benchmark}] empty after type filter"

    df = df.sort_values('time', kind='mergesort').reset_index(drop=True)
    df['block'] = df['addr'].to_numpy() >> LINE_SHIFT

    # shared 라벨 (전 실행 기준)
    line_chip_cnt = df.groupby('block')['chiplet'].nunique()
    shared_line_idx = line_chip_cnt.index[line_chip_cnt >= 2]
    df['is_shared'] = df['block'].isin(shared_line_idx)

    # per-chiplet SD
    print(f"[{benchmark}] per-chiplet SD "
          f"({len(df):,} access, {df['chiplet'].nunique()} chiplets)...",
          flush=True)
    sd_all = np.full(len(df), -1, dtype=np.int64)
    for _, g in df.groupby('chiplet', sort=False):
        idx = g.index.to_numpy()
        sd_all[idx] = compute_stack_distance(g['block'].to_numpy())
    df['sd'] = sd_all

    # per-chiplet × per-category curve
    print(f"[{benchmark}] curves...", flush=True)
    chip_rows = []
    for chip, g in df.groupby('chiplet', sort=True):
        warm_mask = g['sd'] >= 0
        for cat in CATEGORIES:
            if cat == 'all':
                sel = g
            elif cat == 'shared':
                sel = g[g['is_shared']]
            else:
                sel = g[~g['is_shared']]
            wm = sel['sd'] >= 0
            sd_warm = sel.loc[wm, 'sd'].to_numpy()
            cold_cnt = int((~wm).sum())
            curve = curve_for_stream(sd_warm, cold_cnt)
            if curve is None:
                continue
            for mb, (mr_w, mr_t) in curve.items():
                chip_rows.append({
                    'chiplet': int(chip),
                    'category': cat,
                    'cache_size_MB': mb,
                    'miss_rate_warm': mr_w,
                    'miss_rate_total': mr_t,
                    'ref_count': int(len(sel)),
                })
    chip_df = pd.DataFrame(chip_rows)
    chip_df.to_csv(out_chip_csv, index=False)

    # 칩렛 평균 (ref_count > 0인 칩렛만)
    agg = chip_df[chip_df['ref_count'] > 0].groupby(
        ['category', 'cache_size_MB'], as_index=False).agg(
            miss_rate_warm=('miss_rate_warm', 'mean'),
            miss_rate_total=('miss_rate_total', 'mean'),
            chiplets=('chiplet', 'nunique'),
        )
    agg.insert(0, 'benchmark', benchmark)
    agg.to_csv(out_csv, index=False)

    # 리포트: 4MB, 144MB 지점 all-category miss rate
    def _at(cat, mb, col):
        m = agg[(agg['category'] == cat) & (agg['cache_size_MB'] == mb)]
        return float(m[col].iloc[0]) if len(m) else float('nan')

    return (f"[{benchmark}] "
            f"all@4MB={_at('all', 4, 'miss_rate_total'):.3f} "
            f"all@128MB={_at('all', 128, 'miss_rate_total'):.3f} "
            f"shared@4MB={_at('shared', 4, 'miss_rate_total'):.3f}")


# ---------------------------------- main -----------------------------------
if __name__ == "__main__":
    ap = argparse.ArgumentParser()
    ap.add_argument('run_id', nargs='?', default='mesh-6x6')
    ap.add_argument('--workers', type=int,
                    default=int(os.environ.get("MRC_WORKERS", "12")))
    args = ap.parse_args()

    root = run_root(args.run_id)
    TRACE_DIR = str(memory_trace_dir(args.run_id))
    config, ts = run_config(args.run_id), run_tag(args.run_id)
    m = re.search(r'(\d+)x(\d+)', config)
    if m:
        XDIM, YDIM = int(m.group(1)), int(m.group(2))

    OUT_DIR = os.path.abspath(os.path.join(
        os.path.dirname(__file__), "..", "_out", "mrc",
        f"{config}-{ts}"))
    os.makedirs(OUT_DIR, exist_ok=True)

    print(f"Run:   {config}/{ts}")
    print(f"Trace: {TRACE_DIR}")
    print(f"Out:   {OUT_DIR}")
    print(f"Grid:  {XDIM}x{YDIM}  workers={args.workers}")
    print(f"Sizes: {CACHE_SIZES_MB} MB")

    with ProcessPoolExecutor(max_workers=args.workers) as ex:
        for msg in ex.map(process_benchmark, benchmarks):
            print(msg, flush=True)

    rows = []
    for b in benchmarks:
        f = f"{OUT_DIR}/{b}_mrc.csv"
        if os.path.exists(f):
            rows.append(pd.read_csv(f))
    if rows:
        allsum = pd.concat(rows, ignore_index=True)
        out_all = f"{OUT_DIR}/_all_mrc.csv"
        allsum.to_csv(out_all, index=False)
        print(f"\nWrote {out_all} ({allsum['benchmark'].nunique()} benchmarks, "
              f"{len(allsum)} rows)")
    else:
        print("\nNo per-benchmark _mrc.csv produced.")
