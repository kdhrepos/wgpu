"""Working set size per benchmark via LRU stack distance.

정의:
    WSS(percentile p) = "이 벤치마크 참조의 상위 p%를 hit로 잡으려면 필요한
    캐시 용량" = np.percentile(stack_distance_of_warm_refs, p) × 64B.

- 스트림: **per-chiplet** 시간순 (각 칩렛의 access stream 독립). 단일 L2가
    4MB이므로 각 칩렛에서 그 칩렛만의 접근 순서로 SD를 뽑아야 축이 맞음.
    벤치마크 요약값 = 각 칩렛에서 뽑은 percentile을 칩렛 개수로 산술평균.
- 라인 단위: 64B (addr >> 6).
- shared 정의: 전 실행 동안 접근한 서로 다른 칩렛 수가 ≥ 2인 라인.
    한 번이라도 다른 칩렛이 건드리면 shared.
- 필터: type 6/7 (evictor), 8/9 (victim)은 요청 아님 → 제외.
- 로드: usecols로 필요한 5개 컬럼만 read (dedup 없음).

출력:
    {benchmark}_wss.csv         : 1행. 칩렛 평균 P50/P90/P95/P99/max WSS(MB)를
                                  all/shared/private 3카테고리로.
    {benchmark}_wss_by_chip.csv : 칩렛 × 카테고리 상세 percentile (분포 확인용).
    {benchmark}_wss_by_pc.csv   : PC당 access_count, shared_ratio, WSS percentile
                                  (SD는 per-chiplet 스트림에서 계산된 값을 pool).
    _all_wss.csv                : 전 벤치마크 _wss.csv 세로 concat (motivation figure용).

실행:
    analyze/run.sh memory-trace/wss_capacity.py mesh-6x6/latest
    analyze/run.sh memory-trace/wss_capacity.py <run-id> --workers 8
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

TRACE_DIR = None
XDIM = YDIM = 6
OUT_DIR = None


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


# ------------------------- 요약 헬퍼 ---------------------------------------
def _summarize(sd_arr: np.ndarray, prefix: str) -> dict:
    out = {f'{prefix}_n': int(len(sd_arr))}
    if len(sd_arr) == 0:
        for q in (50, 90, 95, 99):
            out[f'{prefix}_p{q}_MB'] = 0.0
        out[f'{prefix}_max_MB'] = 0.0
        return out
    pcts = np.percentile(sd_arr, [50, 90, 95, 99])
    for q, v in zip((50, 90, 95, 99), pcts):
        out[f'{prefix}_p{q}_MB'] = float(v) * LINE_MB
    out[f'{prefix}_max_MB'] = float(sd_arr.max()) * LINE_MB
    return out


# ----------------------------- 벤치마크 1개 ---------------------------------
def process_benchmark(benchmark):
    trace = f"{TRACE_DIR}/{benchmark}_mesh_{XDIM}x{YDIM}_memory_trace.csv"
    if not os.path.exists(trace):
        return f"[{benchmark}] no trace"

    out_csv = f"{OUT_DIR}/{benchmark}_wss.csv"
    out_chip_csv = f"{OUT_DIR}/{benchmark}_wss_by_chip.csv"
    out_pc_csv = f"{OUT_DIR}/{benchmark}_wss_by_pc.csv"

    # (1) 필요한 컬럼만 로드
    print(f"[{benchmark}] load...", flush=True)
    cols = ['time', 'addr', 'pc', 'type', 'chiplet']
    dtype = {'time': 'float64', 'addr': 'uint64', 'pc': 'uint64',
             'type': 'int8', 'chiplet': 'int16'}
    try:
        df = pd.read_csv(trace, usecols=cols, dtype=dtype,
                         skipinitialspace=True)
    except Exception as e:
        return f"[{benchmark}] read fail: {e}"

    # (2) 필터: evictor/victim 제외
    df = df[~df['type'].isin([6, 7, 8, 9])]
    if df.empty:
        return f"[{benchmark}] empty after type filter"

    df = df.sort_values('time', kind='mergesort').reset_index(drop=True)
    df['block'] = df['addr'].to_numpy() >> LINE_SHIFT

    # (3) shared/private 라벨 (전 실행 기준)
    line_chip_cnt = df.groupby('block')['chiplet'].nunique()
    shared_line_idx = line_chip_cnt.index[line_chip_cnt >= 2]
    df['is_shared'] = df['block'].isin(shared_line_idx)

    # (4) per-chiplet 스트림별 SD (각 칩렛 access stream 독립)
    print(f"[{benchmark}] per-chiplet SD "
          f"({len(df):,} access, {df['chiplet'].nunique()} chiplets)...",
          flush=True)
    sd_all = np.full(len(df), -1, dtype=np.int64)
    for _, g in df.groupby('chiplet', sort=False):
        idx = g.index.to_numpy()
        sd_all[idx] = compute_stack_distance(g['block'].to_numpy())
    df['sd'] = sd_all

    warm = df[df['sd'] >= 0]
    unique_lines = int(line_chip_cnt.shape[0])
    shared_cnt = int((line_chip_cnt >= 2).sum())
    private_cnt = unique_lines - shared_cnt

    # (5) 칩렛별 percentile → 산술평균
    chip_rows = []
    for chip, g in warm.groupby('chiplet', sort=True):
        r = {'chiplet': int(chip),
             'access_warm': int(len(g)),
             'shared_access': int(g['is_shared'].sum()),
             'private_access': int((~g['is_shared']).sum())}
        r.update(_summarize(g['sd'].to_numpy(), 'wss_all'))
        r.update(_summarize(g.loc[g['is_shared'], 'sd'].to_numpy(), 'wss_shared'))
        r.update(_summarize(g.loc[~g['is_shared'], 'sd'].to_numpy(), 'wss_private'))
        chip_rows.append(r)
    chip_df = pd.DataFrame(chip_rows)
    chip_df.to_csv(out_chip_csv, index=False)

    # (6) 벤치마크 요약 1행 = 칩렛 평균
    row = {
        'benchmark': benchmark,
        'chiplets': int(len(chip_df)),
        'access_total': int(len(df)),
        'access_warm': int(len(warm)),
        'cold_count': int((df['sd'] < 0).sum()),
        'unique_lines': unique_lines,
        'shared_lines': shared_cnt,
        'private_lines': private_cnt,
        'unique_footprint_MB': unique_lines * LINE_MB,
        'shared_footprint_MB': shared_cnt * LINE_MB,
        'private_footprint_MB': private_cnt * LINE_MB,
    }
    pct_cols = [c for c in chip_df.columns
                if c.startswith(('wss_all_', 'wss_shared_', 'wss_private_'))
                and c.endswith('_MB')]
    for c in pct_cols:
        if c.startswith('wss_shared_'):
            mask = chip_df['shared_access'] > 0
        elif c.startswith('wss_private_'):
            mask = chip_df['private_access'] > 0
        else:
            mask = chip_df['access_warm'] > 0
        sub = chip_df.loc[mask, c]
        row[c] = float(sub.mean()) if len(sub) else 0.0
    row['chiplets_with_shared'] = int((chip_df['shared_access'] > 0).sum())
    row['chiplets_with_private'] = int((chip_df['private_access'] > 0).sum())
    pd.DataFrame([row]).to_csv(out_csv, index=False)

    # (8) per-PC 요약
    print(f"[{benchmark}] per-PC ({warm['pc'].nunique():,} PCs)...", flush=True)
    pc_stats = warm.groupby('pc').agg(
        access_count=('sd', 'size'),
        shared_ratio=('is_shared', 'mean'),
        sd_p50=('sd', lambda s: float(np.percentile(s, 50))),
        sd_p90=('sd', lambda s: float(np.percentile(s, 90))),
        sd_p95=('sd', lambda s: float(np.percentile(s, 95))),
        sd_p99=('sd', lambda s: float(np.percentile(s, 99))),
        sd_max=('sd', 'max'),
    ).reset_index()
    for src, dst in [('sd_p50', 'wss_p50_MB'), ('sd_p90', 'wss_p90_MB'),
                     ('sd_p95', 'wss_p95_MB'), ('sd_p99', 'wss_p99_MB'),
                     ('sd_max', 'wss_max_MB')]:
        pc_stats[dst] = pc_stats[src] * LINE_MB
    pc_stats = pc_stats.drop(columns=['sd_p50', 'sd_p90', 'sd_p95',
                                      'sd_p99', 'sd_max'])
    pc_stats = pc_stats.sort_values('wss_p90_MB', ascending=False)
    pc_stats.to_csv(out_pc_csv, index=False)

    return (f"[{benchmark}] all_p90={row['wss_all_p90_MB']:.2f}MB "
            f"shared_p90={row['wss_shared_p90_MB']:.2f}MB "
            f"private_p90={row['wss_private_p90_MB']:.2f}MB "
            f"footprint={row['unique_footprint_MB']:.1f}MB "
            f"pcs={len(pc_stats):,}")


# ---------------------------------- main -----------------------------------
if __name__ == "__main__":
    ap = argparse.ArgumentParser()
    ap.add_argument('run_id', nargs='?', default='mesh-6x6')
    ap.add_argument('--workers', type=int,
                    default=int(os.environ.get("WSS_WORKERS", "12")))
    args = ap.parse_args()

    root = run_root(args.run_id)
    TRACE_DIR = str(memory_trace_dir(args.run_id))
    config, ts = run_config(args.run_id), run_tag(args.run_id)
    m = re.search(r'(\d+)x(\d+)', config)
    if m:
        XDIM, YDIM = int(m.group(1)), int(m.group(2))

    OUT_DIR = os.path.abspath(os.path.join(
        os.path.dirname(__file__), "..", "_out", "wss-capacity",
        f"{config}-{ts}"))
    os.makedirs(OUT_DIR, exist_ok=True)

    print(f"Run:   {config}/{ts}")
    print(f"Trace: {TRACE_DIR}")
    print(f"Out:   {OUT_DIR}")
    print(f"Grid:  {XDIM}x{YDIM}  workers={args.workers}")

    with ProcessPoolExecutor(max_workers=args.workers) as ex:
        for msg in ex.map(process_benchmark, benchmarks):
            print(msg, flush=True)

    rows = []
    for b in benchmarks:
        f = f"{OUT_DIR}/{b}_wss.csv"
        if os.path.exists(f):
            rows.append(pd.read_csv(f))
    if rows:
        allsum = pd.concat(rows, ignore_index=True)
        out_all = f"{OUT_DIR}/_all_wss.csv"
        allsum.to_csv(out_all, index=False)
        print(f"\nWrote {out_all} ({len(allsum)} benchmarks)")
    else:
        print("\nNo per-benchmark _wss.csv produced.")
