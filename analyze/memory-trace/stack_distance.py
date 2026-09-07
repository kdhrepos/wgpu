"""벤치마크별 LRU stack distance 계산.

정의:
    stack distance(현재 접근 a_i) = a_i 이후로 한 번 더 a_i가 등장할 때까지
    그 사이에 접근된 *서로 다른* cacheline 개수.
    cold(처음 접근)는 inf로 분류 — 통계에선 mean/median 계산에서 제외하고
    별도 cold_count로만 셈.

블록 단위 = 64B cacheline (addr >> 6).
스트림 = 전체 시간순 (요청자 무관). 분석 목적상 "데이터 reuse" 관점.

PTW(is_ptw==1)나 evict/overwrite(type 6,7)는 제외.

집계 키 = (pc, addr).
출력 컬럼:
    pc, addr, access_count, stack_distance_mean, stack_distance_median,
    stack_distance_max, cold_count, shared_degree, owner.

shared_degree는 addr별 unique chiplet 수 (요청자 기준).
owner는 첫 등장 row 기준.

실행:
    analyze/run.sh memory-trace/stack_distance.py mesh-6x6/latest
    analyze/run.sh memory-trace/stack_distance.py <run-id> --workers 8
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
from paddr_map import l2_bank_vec, l2_set_vec, NUM_WAYS

LINE_SHIFT = 6  # 64B cacheline

TRACE_DIR = None
XDIM = YDIM = 6
OUT_DIR = None


# ------------------------- BIT-based LRU stack distance ----------------------
def compute_stack_distance(blocks: np.ndarray) -> np.ndarray:
    """LRU stack distance per access. cold = -1.
    O(N log N) via Fenwick tree.
    """
    n = len(blocks)
    distances = np.full(n, -1, dtype=np.int64)
    last_pos = {}
    size = n + 2
    bit = np.zeros(size, dtype=np.int64)

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
            sd = qry(i - 1) - qry(prev)
            distances[i] = sd
            upd(prev, -1)
        upd(i, 1)
        last_pos[b] = i
    return distances


# ----------------------------- benchmark 1개 --------------------------------
def process_benchmark(benchmark):
    trace = f"{TRACE_DIR}/{benchmark}_mesh_{XDIM}x{YDIM}_memory_trace.csv"
    if not os.path.exists(trace):
        return f"[{benchmark}] no trace"

    out_csv = f"{OUT_DIR}/{benchmark}_stack_distance.csv"

    print(f"[{benchmark}] load...", flush=True)
    cols = ['time', 'addr', 'pc', 'owner', 'type', 'chiplet']
    dtype = {'time': 'float64', 'addr': 'uint64', 'pc': 'uint64',
             'owner': 'int16', 'type': 'int8', 'chiplet': 'int16'}
    try:
        df = pd.read_csv(trace, usecols=cols, dtype=dtype, skipinitialspace=True)
    except Exception as e:
        return f"[{benchmark}] read fail: {e}"

    # 전체 row 기준으로 먼저 중복 제거 후 type 필터링
    df = df.drop_duplicates()
    df = df[~df['type'].isin([6, 7, 8, 9])]
    if df.empty:
        return f"[{benchmark}] empty after filter"

    df = df.sort_values('time', kind='mergesort').reset_index(drop=True)
    df['block'] = (df['addr'].to_numpy() >> LINE_SHIFT)
    df['l2_bank'] = l2_bank_vec(df['addr'].to_numpy())
    df['l2_set'] = l2_set_vec(df['addr'].to_numpy())

    # (chiplet, bank, set) stream별로 SD 계산. set 안에서의 SD는
    # 해당 set associativity와 직접 비교 가능 (SD < ways → hit 후보).
    print(f"[{benchmark}] stack distance ({len(df):,} access, "
          f"{df['chiplet'].nunique()} chiplets, ways={NUM_WAYS})...",
          flush=True)
    sd_all = np.full(len(df), -1, dtype=np.int64)
    for _, g in df.groupby(['chiplet', 'l2_bank', 'l2_set'], sort=False):
        idx = g.index.to_numpy()
        sd_all[idx] = compute_stack_distance(g['block'].to_numpy())
    df['stack_distance'] = sd_all

    # cold 분리
    cold_mask = df['stack_distance'] < 0
    df_warm = df[~cold_mask]

    # per (pc, addr) 집계
    agg = df_warm.groupby(['pc', 'addr'], as_index=False).agg(
        access_count_warm=('stack_distance', 'size'),
        stack_distance_mean=('stack_distance', 'mean'),
        stack_distance_median=('stack_distance', 'median'),
        stack_distance_max=('stack_distance', 'max'),
        stack_distance_min=('stack_distance', 'min'),
    )

    total = df.groupby(['pc', 'addr'], as_index=False).agg(
        access_count=('stack_distance', 'size'),
        owner=('owner', 'first'),
        cold_count=('stack_distance', lambda s: int((s < 0).sum())),
    )

    shared = df.groupby('addr', as_index=False).agg(
        shared_degree=('chiplet', 'nunique')
    )

    out = total.merge(agg, on=['pc', 'addr'], how='left')
    out = out.merge(shared, on='addr', how='left')
    out['stack_distance_mean'] = out['stack_distance_mean'].fillna(-1)
    out['stack_distance_median'] = out['stack_distance_median'].fillna(-1)
    out['stack_distance_max'] = out['stack_distance_max'].fillna(-1)
    out['stack_distance_min'] = out['stack_distance_min'].fillna(-1)
    out['access_count_warm'] = out['access_count_warm'].fillna(0).astype('int64')

    out = out[['pc', 'addr', 'owner', 'access_count', 'access_count_warm',
               'cold_count', 'shared_degree',
               'stack_distance_mean', 'stack_distance_median',
               'stack_distance_max', 'stack_distance_min']]
    out.to_csv(out_csv, index=False)

    # per (chiplet, bank, set) summary — set-level CDF에 사용
    warm = df[df['stack_distance'] >= 0]
    if not warm.empty:
        set_sum = warm.groupby(['chiplet', 'l2_bank', 'l2_set'],
                               as_index=False).agg(
            access_count=('stack_distance', 'size'),
            sd_max=('stack_distance', 'max'),
            sd_p95=('stack_distance', lambda s: int(np.percentile(s, 95))),
            sd_p50=('stack_distance', 'median'),
            sd_mean=('stack_distance', 'mean'),
        )
        unique = df.groupby(['chiplet', 'l2_bank', 'l2_set'],
                            as_index=False)['block'].nunique()
        unique = unique.rename(columns={'block': 'unique_blocks'})
        set_sum = set_sum.merge(unique,
                                on=['chiplet', 'l2_bank', 'l2_set'])
        set_csv = f"{OUT_DIR}/{benchmark}_set_summary.csv"
        set_sum.to_csv(set_csv, index=False)

    return f"[{benchmark}] saved {out_csv} ({len(out):,} rows)"


# -------------------------------- main --------------------------------------
if __name__ == "__main__":
    ap = argparse.ArgumentParser()
    ap.add_argument('run_id', nargs='?', default='mesh-6x6')
    ap.add_argument('--workers', type=int,
                    default=int(os.environ.get("STACK_DIST_WORKERS", "8")))
    args = ap.parse_args()

    root = run_root(args.run_id)
    TRACE_DIR = str(memory_trace_dir(args.run_id))
    config, ts = run_config(args.run_id), run_tag(args.run_id)
    m = re.search(r'(\d+)x(\d+)', config)
    if m:
        XDIM, YDIM = int(m.group(1)), int(m.group(2))

    OUT_DIR = os.path.abspath(os.path.join(
        os.path.dirname(__file__), "..", "_out", "stack-distance",
        f"{config}-{ts}"))
    os.makedirs(OUT_DIR, exist_ok=True)

    print(f"Run:   {config}/{ts}")
    print(f"Trace: {TRACE_DIR}")
    print(f"Out:   {OUT_DIR}")
    print(f"Grid:  {XDIM}x{YDIM}")

    with ProcessPoolExecutor(max_workers=args.workers) as ex:
        for msg in ex.map(process_benchmark, benchmarks):
            print(msg, flush=True)
