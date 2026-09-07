"""sliding-window WSS per chiplet — memory trace, event-count window.

sliding_wss.py 의 event-count 버전. 다른 조건 완전 동일:
  - shared/private = 전 trace pre-label (nchip[line]>=2 → shared)
  - per-PC owner = pc_footprint.py argmax 방식 (chip 안에서 라인 1개 = PC 1개)
  - chiplet 별 독립 계산 → chiplet 간 평균
  - stride = 1 event (매 이벤트마다 sample)
  - max = 전 이벤트에서 WSS 최댓값
  - mean, max 두 개만 (percentile 없음)

차이:
  - window 단위 = 이벤트 개수 (10K / 30K / 50K / 70K / 100K events)
  - 각 이벤트 R 에서 WSS = 최근 W 이벤트 (index [R-W+1, R]) 안 unique line 수
  - 시간 대신 이벤트 축으로 sampling → mean = 이벤트 균등 평균 (Σ WSS(R) / N)
    (sliding_wss.py 의 시간 가중 대신 이벤트 균등)
  - 각 이벤트가 하나의 sample, sub-cycle sampling 이슈 없음
  - 접근이 촘촘한 phase 는 짧은 시간이라도 sample 밀도 높게 잡힘 → 시간 가중과
    다른 관점 (event-load 가중)

_load / _prescan 은 sliding_wss.py 에서 그대로 import — trace 로드 & shared/owner
라벨은 동일 계산.

출력 (~/mgvm/figures/sliding-wss-events/<config>-<tag>/):
  per_benchmark.csv    — benchmark × window_events → chiplet 평균 mean_*_mb, max_*_mb
  per_chiplet.csv      — benchmark × chip × window_events 상세
  per_pc.csv           — benchmark × pc × window_events chiplet 평균
  window<N>/wss_stacked.png + per_benchmark.csv — event window 별 shared+private stacked bar

사용:
  analyze/run.sh memory-trace/sliding_wss_events.py <run-id>
  analyze/run.sh memory-trace/sliding_wss_events.py <run-id> -j 2
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
from motivation_common import file_suffix_for_config
from pc_hotness import mesh_dims_from_config
from shared_private_capacity import plot_stacked
from sliding_wss import _load, _prescan, STACK_SP, HAS_NUMBA
from params import benchmarks

try:
    from numba import njit
except ImportError:
    def njit(**_kw):
        def dec(f):
            return f
        return dec

DEFAULT_WINDOW_EVENTS = [10_000, 30_000, 50_000, 70_000, 100_000]
LINE_BYTES = 64
LINE_MB = LINE_BYTES / (1024.0 ** 2)

FIG_ROOT = Path.home() / "mgvm" / "figures" / "sliding-wss-events"
FIG_FORMATS = ("png",)


# ── numba event-count sliding: 이벤트 index 기준 two-pointer ──────────────
@njit(cache=True)
def _slide_events_one(lines, owner_pcs, is_shared, window_events, M, K):
    """
    lines        : int32[n]    per-chiplet compact line id (0..M-1), 시간 순 정렬
    owner_pcs    : int32[M]    per-chiplet compact line → 전역 compact PC id (-1 없음)
    is_shared    : bool[M]     per-chiplet compact line → shared 여부
    window_events: int64       window 크기 (이벤트 개수)
    M, K         : line / PC compact 크기
    """
    n = len(lines)
    pc_mean = np.zeros(K, dtype=np.float64)
    pc_max = np.zeros(K, dtype=np.int32)
    if n < 1:
        return (0.0, 0, 0.0, 0.0, 0, 0, pc_mean, pc_max)

    line_count = np.zeros(M, dtype=np.int32)
    pc_wss = np.zeros(K, dtype=np.int32)
    pc_contrib = np.zeros(K, dtype=np.int64)
    pc_last_change = np.zeros(K, dtype=np.int64)

    wss_total = 0
    wss_shared = 0
    wss_private = 0
    max_total = 0
    max_shared = 0
    max_private = 0
    sum_total = 0
    sum_shared = 0
    sum_private = 0

    for R in range(n):
        # 오래된 이벤트 (R - window_events) 제거
        if R >= window_events:
            L_out = R - window_events
            l = lines[L_out]
            line_count[l] -= 1
            if line_count[l] == 0:
                wss_total -= 1
                if is_shared[l]:
                    wss_shared -= 1
                else:
                    wss_private -= 1
                pc = owner_pcs[l]
                if pc >= 0:
                    pc_contrib[pc] += pc_wss[pc] * (R - pc_last_change[pc])
                    pc_wss[pc] -= 1
                    pc_last_change[pc] = R
        # 이벤트 R 추가
        l = lines[R]
        if line_count[l] == 0:
            wss_total += 1
            if is_shared[l]:
                wss_shared += 1
            else:
                wss_private += 1
            pc = owner_pcs[l]
            if pc >= 0:
                pc_contrib[pc] += pc_wss[pc] * (R - pc_last_change[pc])
                pc_wss[pc] += 1
                pc_last_change[pc] = R
                if pc_wss[pc] > pc_max[pc]:
                    pc_max[pc] = pc_wss[pc]
        line_count[l] += 1
        # max
        if wss_total > max_total:
            max_total = wss_total
        if wss_shared > max_shared:
            max_shared = wss_shared
        if wss_private > max_private:
            max_private = wss_private
        # sample 이벤트 R (균등 가중)
        sum_total += wss_total
        sum_shared += wss_shared
        sum_private += wss_private

    # per-PC 최종 flush (index n 까지)
    for k in range(K):
        pc_contrib[k] += pc_wss[k] * (n - pc_last_change[k])

    if n == 0:
        return (0.0, max_total, 0.0, 0.0, max_shared, max_private,
                pc_mean, pc_max)

    mean_total = sum_total / n
    mean_shared = sum_shared / n
    mean_private = sum_private / n
    pc_mean = pc_contrib.astype(np.float64) / n
    return (mean_total, max_total, mean_shared, mean_private,
            max_shared, max_private, pc_mean, pc_max)


# ── per-benchmark: sliding_wss.py.process_benchmark 와 거의 동일 ───────────
def process_benchmark(bench: str, trace_dir: str, suffix: str, n_chiplets: int,
                      window_events_list=None):
    if window_events_list is None:
        window_events_list = DEFAULT_WINDOW_EVENTS
    trace_file = f"{trace_dir}/{bench}_{suffix}_memory_trace.csv"
    if not os.path.exists(trace_file):
        return None
    print(f"[{bench}] load trace ...", flush=True)
    loaded = _load(trace_file)
    if loaded is None:
        return None
    times_all, lines_all, pcs_all, chips_all = loaded
    print(f"[{bench}] loaded {len(times_all):,} demand events, prescan ...",
          flush=True)

    is_shared_map, owner_pc_map = _prescan(lines_all, pcs_all, chips_all)

    unique_pcs = np.unique(pcs_all)
    pc_compact = {int(p): i for i, p in enumerate(unique_pcs)}
    K = len(pc_compact)

    per_chip_rows = []
    per_pc_rows = []

    for c in range(n_chiplets):
        m = chips_all == c
        if not m.any():
            continue
        ch_times = times_all[m]
        ch_lines = lines_all[m]
        order = np.argsort(ch_times, kind='mergesort')
        ch_lines = ch_lines[order]              # time 은 순서에만 쓰고 버림

        unique_lines, ch_lines_compact = np.unique(ch_lines, return_inverse=True)
        M = len(unique_lines)
        owner_arr = np.full(M, -1, dtype=np.int32)
        is_shared_arr = np.zeros(M, dtype=np.bool_)
        for cid, raw_line in enumerate(unique_lines):
            raw = int(raw_line)
            p = owner_pc_map.get((c, raw), None)
            if p is not None:
                owner_arr[cid] = pc_compact[p]
            is_shared_arr[cid] = bool(is_shared_map.get(raw, False))

        ch_lines_compact = ch_lines_compact.astype(np.int32)

        for W in window_events_list:
            (m_tot, x_tot, m_sh, m_pr, x_sh, x_pr,
             pc_mean, pc_max) = _slide_events_one(
                ch_lines_compact, owner_arr, is_shared_arr, W, M, K)
            per_chip_rows.append({
                'benchmark': bench, 'chip': c, 'window_events': W,
                'mean_wss_mb': m_tot * LINE_MB,
                'max_wss_mb': x_tot * LINE_MB,
                'mean_shared_wss_mb': m_sh * LINE_MB,
                'mean_private_wss_mb': m_pr * LINE_MB,
                'max_shared_wss_mb': x_sh * LINE_MB,
                'max_private_wss_mb': x_pr * LINE_MB,
            })
            for pid, p_raw in enumerate(unique_pcs):
                if pc_mean[pid] == 0 and pc_max[pid] == 0:
                    continue
                per_pc_rows.append({
                    'benchmark': bench, 'chip': c, 'window_events': W,
                    'pc_hex': f"0x{int(p_raw):016x}", 'pc': int(p_raw),
                    'mean_wss_mb': pc_mean[pid] * LINE_MB,
                    'max_wss_mb': int(pc_max[pid]) * LINE_MB,
                })

    if not per_chip_rows:
        return None

    per_chip_df = pd.DataFrame(per_chip_rows)
    per_pc_df = pd.DataFrame(per_pc_rows)

    agg = per_chip_df.groupby('window_events').mean(numeric_only=True)
    agg = agg.drop(columns=['chip']).reset_index()
    agg.insert(0, 'benchmark', bench)
    per_bench_rows = agg.to_dict('records')

    if not per_pc_df.empty:
        pc_agg = (per_pc_df.groupby(['window_events', 'pc_hex', 'pc'])
                  .mean(numeric_only=True).reset_index())
        pc_agg.insert(0, 'benchmark', bench)
        pc_agg = pc_agg.drop(columns=['chip'])
    else:
        pc_agg = pd.DataFrame()

    return per_bench_rows, per_chip_df, pc_agg


def _parse_windows(s: str) -> list[int]:
    out = []
    for p in s.split(","):
        p = p.strip()
        if not p:
            continue
        n = int(float(p))   # "1e5" 같은 표기도 허용
        if n <= 0:
            raise argparse.ArgumentTypeError(f"window size must be > 0, got {p!r}")
        out.append(n)
    if not out:
        raise argparse.ArgumentTypeError("empty --windows")
    return out


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("run_id", nargs="?", default="mesh-6x6/latest")
    ap.add_argument("-j", "--workers", type=int, default=2,
                    help="벤치마크 병렬 워커 수 (trace 로드가 GB 급, 기본 2)")
    ap.add_argument("--windows", type=str,
                    default=",".join(str(w) for w in DEFAULT_WINDOW_EVENTS),
                    help="comma-separated window sizes in access(event) count "
                         f"(default {DEFAULT_WINDOW_EVENTS})")
    args = ap.parse_args()
    window_events_list = _parse_windows(args.windows)

    if not HAS_NUMBA:
        print("WARNING: numba 없음 — 순수 Python fallback 은 대형 trace 에서 몇 시간 걸림. "
              "'pip install numba' 강력 권장.", file=sys.stderr)

    trace_dir = str(memory_trace_dir(args.run_id))
    config = run_config(args.run_id)
    suffix = file_suffix_for_config(config)
    _, n_chiplets = mesh_dims_from_config(config)
    tag = run_tag(args.run_id)

    if args.workers <= 1:
        raw = [process_benchmark(b, trace_dir, suffix, n_chiplets, window_events_list)
               for b in benchmarks]
    else:
        with ProcessPoolExecutor(max_workers=args.workers) as ex:
            raw = list(ex.map(process_benchmark, benchmarks,
                              repeat(trace_dir), repeat(suffix), repeat(n_chiplets),
                              repeat(window_events_list)))

    bench_rows, chip_frames, pc_frames = [], [], []
    for r in raw:
        if r is None:
            continue
        rows, chip_df, pc_df = r
        bench_rows.extend(rows)
        chip_frames.append(chip_df)
        if not pc_df.empty:
            pc_frames.append(pc_df)

    if not bench_rows:
        print("no usable benchmarks", file=sys.stderr)
        sys.exit(1)

    per_bench = pd.DataFrame(bench_rows)
    per_chip = pd.concat(chip_frames, ignore_index=True)
    per_pc = pd.concat(pc_frames, ignore_index=True) if pc_frames else pd.DataFrame()

    out = FIG_ROOT / f"{config}-{tag}"
    out.mkdir(parents=True, exist_ok=True)
    per_bench.to_csv(out / "per_benchmark.csv", index=False)
    per_chip.to_csv(out / "per_chiplet.csv", index=False)
    if not per_pc.empty:
        per_pc.to_csv(out / "per_pc.csv", index=False)

    for W in window_events_list:
        sub = per_bench[per_bench['window_events'] == W].copy()
        if sub.empty:
            continue
        sub['total_mb'] = sub['mean_shared_wss_mb'] + sub['mean_private_wss_mb']
        sub = sub.sort_values('total_mb').set_index('benchmark')
        wout = out / f"window{W}"
        wout.mkdir(parents=True, exist_ok=True)
        sub.to_csv(wout / "per_benchmark.csv")
        plot_stacked(sub, STACK_SP, wout, "wss_stacked")

    print(f"\n[{config}] wrote {out}")
    print(per_bench.to_string(index=False))


if __name__ == "__main__":
    main()
