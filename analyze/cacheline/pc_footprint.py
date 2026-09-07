"""per-PC per-chiplet footprint (exclusive owner per (chip, line), then avg over chiplets).

각 (chiplet, cacheline) 을 정확히 하나의 PC 에 귀속:
    owner_pc(chip, line) = argmax_pc access_count[pc, line, chip]
    동률 시 pc 값 작은 쪽 (tie-break deterministic).

그러면 각 chiplet 안에서:
    Σ_pc owned_lines(chip, pc) = 그 chiplet 의 L2 가 실제 만진 unique line 수
그리고 chiplet 평균을 내면:
    Σ_pc avg_owned_lines[pc] = per-chiplet 평균 unique footprint
                             = shared_private_capacity.py 의 avg_total_capacity_mb
                             = avg_shared_capacity_mb + avg_private_capacity_mb

즉 PC 별 avg_footprint_MB 를 다 더하면 벤치마크 하나의 "칩렛 하나가 lifetime
동안 쥐는 평균 footprint" 와 정확히 일치.

정의:
    line          = addr >> 6 (64B cacheline)
    demand access = type ∈ {0..5}. (is_ptw는 dead, USECOLS에 없음)
    전역 shared   = 서로 다른 chiplet ≥ 2 가 접근한 line (전역 라벨)
    전역 private  = 정확히 1 chiplet 만 접근한 line

    owned_lines(c, p)  = chiplet c 에서 pc p 가 owner 인 라인 개수
    shared_lines(c, p) = 그 중 전역 shared 라벨인 것
    footprint_MB(c, p) = owned_lines × 64B

    avg_footprint_MB[p]  = mean_c footprint_MB(c, p)   (n_chiplets 로 나눔,
                                                       접근 없는 chiplet=0 취급)
    total_footprint_MB[p] = sum_c footprint_MB(c, p)   (shared 라인이 여러 chiplet
                                                       에 걸쳐 카운트됨)

출력:
    {bench}_pc_footprint.csv : PC 당 1행 (avg_footprint_MB 내림차순).
    {bench}_pc_footprint_per_chiplet.csv : (pc, chip) 상세 (분포 확인용).
    _all_pc_footprint.csv    : 전 벤치마크 concat.

컬럼:
    benchmark, pc_hex, pc,
    access_count           = 이 PC 의 총 접근 수 전 chiplet 합 (owner 여부 무관)
    avg_access_count_owned = mean_c (owner 라인에 대한 접근 수 on chiplet c)
    avg_owned_lines        = mean_c owned_lines(c, p)     (= footprint / 64B)
    avg_shared_lines, avg_private_lines
    avg_footprint_MB       = avg_owned_lines × 64B          <- 짝 컬럼(1)
    avg_shared_footprint_MB, avg_private_footprint_MB
    total_owned_lines      = sum_c owned_lines(c, p)      <- 짝 컬럼(2, 총량)
    total_footprint_MB     = total_owned_lines × 64B
    shared_line_ratio      = avg_shared_lines / avg_owned_lines

사용:
    analyze/run.sh cacheline/pc_footprint.py <run-id>
    analyze/run.sh cacheline/pc_footprint.py <run-id> -j 8
"""

from __future__ import annotations

import argparse
import os
import sys
from concurrent.futures import ProcessPoolExecutor
from itertools import repeat
from pathlib import Path

import numpy as np
import pandas as pd

from _paths import memory_trace_dir, run_config, run_tag
from motivation_common import file_suffix_for_config, trace_chunks
from pc_hotness import mesh_dims_from_config
from params import benchmarks

DEMAND_TYPES = np.array([0, 1, 2, 3, 4, 5], dtype=np.int8)
USECOLS = ['addr', 'pc', 'type', 'chiplet']   # is_ptw / time / owner 불필요
LINE_BYTES = 64
LINE_MB = LINE_BYTES / (1024.0 ** 2)


# ── 청크 축적: (pc,line,chip) 카운트 + (line,chip) unique + (pc,chip) 총 접근
def _agg(trace_file: str):
    count_parts = []      # per chunk: (pc, line, chip) -> count
    line_chip_parts = []  # per chunk: unique (line, chip)  — 전역 nchip 판정
    pc_chip_total_parts = []  # per chunk: (pc, chip) -> access count
    for chunk in trace_chunks(trace_file, usecols=USECOLS):
        chunk = chunk.drop_duplicates()
        t = chunk['type'].to_numpy(dtype=np.int8)
        m = np.isin(t, DEMAND_TYPES)
        if not m.any():
            continue
        addr = chunk['addr'].to_numpy(dtype=np.uint64)[m]
        pc = chunk['pc'].to_numpy(dtype=np.uint64)[m]
        chip = chunk['chiplet'].to_numpy(dtype=np.int32)[m]
        line = addr >> np.uint64(6)

        df = pd.DataFrame({'pc': pc, 'line': line, 'chip': chip})
        count_parts.append(df.groupby(['pc', 'line', 'chip']).size())
        line_chip_parts.append(df[['line', 'chip']].drop_duplicates())
        pc_chip_total_parts.append(df.groupby(['pc', 'chip']).size())

    if not count_parts:
        return None

    pc_line_chip_count = (pd.concat(count_parts)
                          .groupby(level=[0, 1, 2]).sum()
                          .rename('access_count_pc_line_chip')
                          .reset_index())
    line_chip = pd.concat(line_chip_parts, ignore_index=True).drop_duplicates()
    pc_chip_total = (pd.concat(pc_chip_total_parts)
                     .groupby(level=[0, 1]).sum()
                     .rename('access_count'))
    return pc_line_chip_count, line_chip, pc_chip_total


# ── 벤치마크 1개 ──────────────────────────────────────────────────────────
def process_benchmark(bench: str, trace_dir: str, suffix: str, out_dir: str,
                      n_chiplets: int):
    trace_file = f"{trace_dir}/{bench}_{suffix}_memory_trace.csv"
    if not os.path.exists(trace_file):
        return f"[{bench}] no trace"

    print(f"[{bench}] per-PC per-chiplet footprint ...", flush=True)
    try:
        res = _agg(trace_file)
        if res is None:
            return f"[{bench}] empty"
        pc_line_chip_count, line_chip, pc_chip_total = res

        # owner PC per (chip, line) = argmax count, tie → smaller pc
        pc_line_chip_count = pc_line_chip_count.sort_values(
            ['chip', 'line', 'access_count_pc_line_chip', 'pc'],
            ascending=[True, True, False, True], kind='mergesort')
        owner = pc_line_chip_count.drop_duplicates(
            subset=['chip', 'line'], keep='first').copy()
        # owner: (pc, line, chip, access_count_pc_line_chip) — (chip,line) 당 1행

        # 전역 shared 라벨 (라인 하나에 대해 chiplet 몇 개가 만졌나)
        nchip_per_line = line_chip.groupby('line')['chip'].nunique()
        shared_line_idx = nchip_per_line.index[nchip_per_line >= 2]
        owner['is_shared'] = owner['line'].isin(shared_line_idx)

        # (chip, pc) 별 집계
        per_chip_pc = owner.groupby(['chip', 'pc']).agg(
            owned_lines=('line', 'size'),
            shared_lines=('is_shared', 'sum'),
            access_count_owned=('access_count_pc_line_chip', 'sum'),
        ).reset_index()
        per_chip_pc['private_lines'] = (per_chip_pc['owned_lines']
                                        - per_chip_pc['shared_lines'])
        per_chip_pc['footprint_MB'] = per_chip_pc['owned_lines'] * LINE_MB
        per_chip_pc['shared_footprint_MB'] = per_chip_pc['shared_lines'] * LINE_MB
        per_chip_pc['private_footprint_MB'] = per_chip_pc['private_lines'] * LINE_MB
        per_chip_pc['benchmark'] = bench
        per_chip_pc['pc_hex'] = per_chip_pc['pc'].apply(
            lambda v: f"0x{int(v):016x}")

        out_per_chip = f"{out_dir}/{bench}_pc_footprint_per_chiplet.csv"
        cols_per_chip = ['benchmark', 'chip', 'pc_hex', 'pc',
                         'owned_lines', 'shared_lines', 'private_lines',
                         'access_count_owned',
                         'footprint_MB', 'shared_footprint_MB',
                         'private_footprint_MB']
        (per_chip_pc[cols_per_chip]
         .sort_values(['chip', 'footprint_MB'], ascending=[True, False])
         .to_csv(out_per_chip, index=False))

        # per-PC 요약: n_chiplets 로 나눠 avg (owner 라인 없는 chiplet 은 0)
        pc_stat = per_chip_pc.groupby('pc').agg(
            total_owned_lines=('owned_lines', 'sum'),
            total_shared_lines=('shared_lines', 'sum'),
            total_private_lines=('private_lines', 'sum'),
            total_access_count_owned=('access_count_owned', 'sum'),
        ).reset_index()
        pc_stat['avg_owned_lines'] = pc_stat['total_owned_lines'] / n_chiplets
        pc_stat['avg_shared_lines'] = pc_stat['total_shared_lines'] / n_chiplets
        pc_stat['avg_private_lines'] = pc_stat['total_private_lines'] / n_chiplets
        pc_stat['avg_access_count_owned'] = (pc_stat['total_access_count_owned']
                                             / n_chiplets)
        pc_stat['avg_footprint_MB'] = pc_stat['avg_owned_lines'] * LINE_MB
        pc_stat['avg_shared_footprint_MB'] = pc_stat['avg_shared_lines'] * LINE_MB
        pc_stat['avg_private_footprint_MB'] = (pc_stat['avg_private_lines']
                                               * LINE_MB)
        pc_stat['total_footprint_MB'] = pc_stat['total_owned_lines'] * LINE_MB
        pc_stat['shared_line_ratio'] = np.where(
            pc_stat['avg_owned_lines'] > 0,
            pc_stat['avg_shared_lines'] / pc_stat['avg_owned_lines'],
            0.0)

        # 접근은 있는데 owner 라인 0인 PC 포함
        pc_total_global = pc_chip_total.groupby(level=0).sum().rename('access_count')
        pc_stat = pc_stat.merge(pc_total_global.reset_index(), on='pc', how='outer')
        for col in ['total_owned_lines', 'total_shared_lines', 'total_private_lines',
                    'total_access_count_owned',
                    'avg_owned_lines', 'avg_shared_lines', 'avg_private_lines',
                    'avg_access_count_owned',
                    'avg_footprint_MB', 'avg_shared_footprint_MB',
                    'avg_private_footprint_MB',
                    'total_footprint_MB', 'shared_line_ratio']:
            pc_stat[col] = pc_stat[col].fillna(0)
        pc_stat['access_count'] = pc_stat['access_count'].fillna(0).astype('int64')
        for col in ['total_owned_lines', 'total_shared_lines',
                    'total_private_lines', 'total_access_count_owned']:
            pc_stat[col] = pc_stat[col].astype('int64')

        pc_stat['benchmark'] = bench
        pc_stat['pc_hex'] = pc_stat['pc'].apply(lambda v: f"0x{int(v):016x}")
        pc_stat = pc_stat.sort_values('avg_footprint_MB', ascending=False)
        pc_stat = pc_stat[['benchmark', 'pc_hex', 'pc',
                           'access_count',
                           'avg_owned_lines', 'avg_shared_lines',
                           'avg_private_lines', 'avg_access_count_owned',
                           'avg_footprint_MB', 'avg_shared_footprint_MB',
                           'avg_private_footprint_MB',
                           'total_owned_lines', 'total_footprint_MB',
                           'shared_line_ratio']]

        out = f"{out_dir}/{bench}_pc_footprint.csv"
        pc_stat.to_csv(out, index=False)

        avg_sum = float(pc_stat['avg_footprint_MB'].sum())
        n_owned = int((pc_stat['avg_owned_lines'] > 0).sum())
        return (f"[{bench}] {len(pc_stat):,} PCs "
                f"({n_owned} own lines), sum_avg_footprint={avg_sum:.2f}MB "
                f"(= avg per-chiplet L2 unique footprint) -> {out}")
    except Exception as e:
        import traceback
        traceback.print_exc()
        return f"[{bench}] error: {e}"


# ── 메인 ──────────────────────────────────────────────────────────────────
def main():
    ap = argparse.ArgumentParser()
    ap.add_argument('run_id', nargs='?', default='mesh-6x6/latest')
    ap.add_argument('-j', '--workers', type=int, default=8)
    args = ap.parse_args()

    trace_dir = str(memory_trace_dir(args.run_id))
    config = run_config(args.run_id)
    suffix = file_suffix_for_config(config)
    tag = run_tag(args.run_id)
    _, n_chiplets = mesh_dims_from_config(config)

    out_dir = os.path.abspath(os.path.join(
        os.path.dirname(__file__), "..", "_out", "pc-footprint",
        f"{config}-{tag}"))
    os.makedirs(out_dir, exist_ok=True)

    print(f"Trace  : {trace_dir}")
    print(f"Out    : {out_dir}")
    print(f"Chiplets: {n_chiplets}")
    print(f"Workers: {args.workers}")

    if args.workers <= 1:
        results = [process_benchmark(b, trace_dir, suffix, out_dir, n_chiplets)
                   for b in benchmarks]
    else:
        with ProcessPoolExecutor(max_workers=args.workers) as ex:
            results = list(ex.map(process_benchmark, benchmarks,
                                  repeat(trace_dir), repeat(suffix),
                                  repeat(out_dir), repeat(n_chiplets)))
    for r in results:
        print(r, flush=True)

    dfs = []
    for b in benchmarks:
        f = f"{out_dir}/{b}_pc_footprint.csv"
        if os.path.exists(f):
            dfs.append(pd.read_csv(f))
    if dfs:
        allsum = pd.concat(dfs, ignore_index=True)
        out_all = f"{out_dir}/_all_pc_footprint.csv"
        allsum.to_csv(out_all, index=False)
        print(f"\nWrote {out_all} "
              f"({allsum['benchmark'].nunique()} benchmarks, {len(allsum):,} rows)")


if __name__ == "__main__":
    main()
