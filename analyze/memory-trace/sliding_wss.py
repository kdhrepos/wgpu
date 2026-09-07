"""sliding-window WSS per chiplet — memory trace, time-weighted mean.

각 chiplet 의 L2 관점에서, "현재 시점부터 뒤로 W cycle 안에 접근된 서로 다른
cacheline 개수" 를 sliding window 로 잰다. window 는 이벤트 사이에만 값이
바뀌므로 이벤트 경계에서 sampling + 시간 가중 = 진짜 1-cycle stride 결과와 수학적
으로 동일.

컨셉:
  window(t) = [t - W, t]                    (W 는 cycle)
  wss(t)    = window(t) 안에 접근된 unique cacheline 수
  mean_wss  = ∫ wss(t) dt  / total_sim_time  (시간 가중)
  max_wss   = max_t wss(t)

  각 chiplet 별로 스트림 독립 계산 → chiplet 평균 = 한 GPM 이 lifetime 동안
  W-cycle 뒤로 볼 때의 평균 WSS.

  shared/private 분리:
    is_shared[line] = (전 trace 기준) 라인을 만진 서로 다른 chiplet ≥ 2
    wss_shared(t) = window 안 unique shared line 수
    wss_private(t) = window 안 unique private line 수
    wss_shared + wss_private = wss_total (partition)

  per-PC 분리 (pc_footprint.py 방식 그대로):
    owner_pc[chip, line] = argmax_pc access_count[pc, line, chip]   (동률 시 pc 작은 쪽)
    → 라인 하나가 chiplet 안에서 정확히 1 PC 에 귀속
    wss_pc[k] = window 안 unique line 중 owner=k 인 것
    Σ_k wss_pc[k] = wss_total (partition)

window sizes: 10K, 30K, 50K, 70K, 100K cycles (사용자 지정, 코드 상단 상수).
1 cycle = 1e-9 s (GPU freq 1 GHz, mgpusim/builders/mesh.go WithFreq(1*akita.GHz)).

성능:
  two-pointer O(N) per chiplet per window. per-PC 는 sparse accumulator 로 O(1)
  per event (amortized). 대충 (K PCs + 3) × N × #windows 단순 곱셈은 아니지만
  N=10^8, window 5개, chiplet 36 이면 numba 로 벤치마크당 수 분.
  numba 없으면 순수 python 이라 매우 느림 → 강력 권장.

출력 (~/mgvm/figures/sliding-wss/<config>-<tag>/):
  per_benchmark.csv    — benchmark × window_cycles → chiplet 평균 mean_*_mb, max_*_mb
  per_chiplet.csv      — benchmark × chip × window_cycles → 상세
  per_pc.csv           — benchmark × pc × window_cycles → chiplet 평균 mean_wss_mb
  wss_stacked.png      — window_cycles 하나마다 shared+private stacked bar

사용:
  analyze/run.sh memory-trace/sliding_wss.py <run-id>
  analyze/run.sh memory-trace/sliding_wss.py <run-id> -j 4
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
from shared_private_capacity import plot_stacked
from params import benchmarks

try:
    from numba import njit
    HAS_NUMBA = True
except ImportError:
    HAS_NUMBA = False

    def njit(**_kw):                       # noqa: D401
        def dec(f):
            return f
        return dec

DEMAND_TYPES = np.array([0, 1, 2, 3, 4, 5], dtype=np.int8)
USECOLS = ['time', 'addr', 'pc', 'type', 'chiplet']
CYCLE_TO_SEC = 1e-9                        # GPU 1 GHz (mgpusim default)
DEFAULT_WINDOW_CYCLES = [10_000, 30_000, 50_000, 70_000, 100_000]
LINE_BYTES = 64
LINE_MB = LINE_BYTES / (1024.0 ** 2)

FIG_ROOT = Path.home() / "mgvm" / "figures" / "sliding-wss"
FIG_FORMATS = ("png",)


# ── trace 로드 (demand 이벤트만, 필요 컬럼만) ─────────────────────────────
def _load(trace_file: str):
    ts, ln, pcs, chs = [], [], [], []
    for chunk in trace_chunks(trace_file, usecols=USECOLS):
        chunk = chunk.drop_duplicates()
        t = chunk['type'].to_numpy(dtype=np.int8)
        m = np.isin(t, DEMAND_TYPES)
        if not m.any():
            continue
        ts.append(chunk['time'].to_numpy(dtype=np.float64)[m])
        ln.append(chunk['addr'].to_numpy(dtype=np.uint64)[m] >> np.uint64(6))
        pcs.append(chunk['pc'].to_numpy(dtype=np.uint64)[m])
        chs.append(chunk['chiplet'].to_numpy(dtype=np.int16)[m])
    if not ts:
        return None
    return (np.concatenate(ts), np.concatenate(ln),
            np.concatenate(pcs), np.concatenate(chs))


# ── prescan: is_shared[line] + owner_pc[chip, line] (pc_footprint.py 방식) ──
def _prescan(lines, pcs, chips):
    """전 trace 기준. 반환:
      is_shared_map  : dict raw_line -> bool
      owner_pc_map   : dict (chip, raw_line) -> raw_pc  (argmax count, tie=smaller pc)
    """
    lc = pd.DataFrame({'line': lines, 'chip': chips}).drop_duplicates()
    nchip = lc.groupby('line')['chip'].nunique()
    is_shared_map = (nchip >= 2).to_dict()

    plc = (pd.DataFrame({'chip': chips, 'line': lines, 'pc': pcs})
           .groupby(['chip', 'line', 'pc']).size().rename('cnt').reset_index())
    plc = plc.sort_values(['chip', 'line', 'cnt', 'pc'],
                          ascending=[True, True, False, True], kind='mergesort')
    owner = plc.drop_duplicates(subset=['chip', 'line'], keep='first')
    owner_pc_map = {(int(r.chip), int(r.line)): int(r.pc)
                    for r in owner.itertuples(index=False)}
    return is_shared_map, owner_pc_map


# ── numba sliding-window: two-pointer, sparse per-PC accumulator ─────────
@njit(cache=True)
def _slide_one(times, lines, owner_pcs, is_shared, window_size, M, K):
    """
    times        : float64[n]  chiplet 이벤트 시간 (초), sorted
    lines        : int32[n]    per-chiplet compact line id (0..M-1)
    owner_pcs    : int32[M]    per-chiplet compact-line → 전역 compact PC id (-1 없음)
    is_shared    : bool[M]     per-chiplet compact-line → 전역 shared 라벨
    window_size  : float64     초
    M, K         : line / PC compact 크기
    """
    n = len(times)
    pc_mean = np.zeros(K, dtype=np.float64)
    pc_max = np.zeros(K, dtype=np.int32)
    if n < 2:
        return (0.0, 0, 0.0, 0.0, 0, 0, pc_mean, pc_max)

    line_count = np.zeros(M, dtype=np.int32)
    pc_wss = np.zeros(K, dtype=np.int32)
    pc_contrib = np.zeros(K, dtype=np.float64)
    pc_last_change = np.full(K, times[0], dtype=np.float64)

    wss_total = 0
    wss_shared = 0
    wss_private = 0
    max_total = 0
    max_shared = 0
    max_private = 0
    sum_total = 0.0
    sum_shared = 0.0
    sum_private = 0.0

    L = 0
    for R in range(n):
        tR = times[R]
        cutoff = tR - window_size
        # advance L — 오래된 이벤트 제거
        while L < R and times[L] < cutoff:
            l = lines[L]
            line_count[l] -= 1
            if line_count[l] == 0:
                wss_total -= 1
                if is_shared[l]:
                    wss_shared -= 1
                else:
                    wss_private -= 1
                pc = owner_pcs[l]
                if pc >= 0:
                    pc_contrib[pc] += pc_wss[pc] * (tR - pc_last_change[pc])
                    pc_wss[pc] -= 1
                    pc_last_change[pc] = tR
            L += 1
        # 현재 이벤트 R 추가
        l = lines[R]
        if line_count[l] == 0:
            wss_total += 1
            if is_shared[l]:
                wss_shared += 1
            else:
                wss_private += 1
            pc = owner_pcs[l]
            if pc >= 0:
                pc_contrib[pc] += pc_wss[pc] * (tR - pc_last_change[pc])
                pc_wss[pc] += 1
                pc_last_change[pc] = tR
                if pc_wss[pc] > pc_max[pc]:
                    pc_max[pc] = pc_wss[pc]
        line_count[l] += 1
        # max 갱신
        if wss_total > max_total:
            max_total = wss_total
        if wss_shared > max_shared:
            max_shared = wss_shared
        if wss_private > max_private:
            max_private = wss_private
        # 시간 가중 (총합/shared/private) — [t_R, t_{R+1}] 동안 WSS 상수
        if R < n - 1:
            dt = times[R + 1] - tR
            sum_total += wss_total * dt
            sum_shared += wss_shared * dt
            sum_private += wss_private * dt

    # per-PC 마지막 flush
    t_end = times[n - 1]
    for k in range(K):
        pc_contrib[k] += pc_wss[k] * (t_end - pc_last_change[k])

    total_t = times[n - 1] - times[0]
    if total_t <= 0:
        return (0.0, max_total, 0.0, 0.0, max_shared, max_private,
                pc_mean, pc_max)

    mean_total = sum_total / total_t
    mean_shared = sum_shared / total_t
    mean_private = sum_private / total_t
    pc_mean = pc_contrib / total_t
    return (mean_total, max_total, mean_shared, mean_private,
            max_shared, max_private, pc_mean, pc_max)


# ── per-benchmark: 로드 → prescan → per-chiplet slide × windows × PCs ─────
def process_benchmark(bench: str, trace_dir: str, suffix: str, n_chiplets: int,
                      window_cycles_list=None):
    if window_cycles_list is None:
        window_cycles_list = DEFAULT_WINDOW_CYCLES
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

    # 전역 PC compact id (모든 chiplet 공용)
    unique_pcs = np.unique(pcs_all)
    pc_compact = {int(p): i for i, p in enumerate(unique_pcs)}
    K = len(pc_compact)

    per_bench_rows = []
    per_chip_rows = []
    per_pc_rows = []

    for c in range(n_chiplets):
        m = chips_all == c
        if not m.any():
            continue
        ch_times = times_all[m]
        ch_lines = lines_all[m]
        order = np.argsort(ch_times, kind='mergesort')
        ch_times = ch_times[order]
        ch_lines = ch_lines[order]

        # per-chiplet compact line id
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

        for W_cycles in window_cycles_list:
            W_sec = W_cycles * CYCLE_TO_SEC
            (m_tot, x_tot, m_sh, m_pr, x_sh, x_pr,
             pc_mean, pc_max) = _slide_one(ch_times, ch_lines_compact,
                                           owner_arr, is_shared_arr,
                                           W_sec, M, K)
            per_chip_rows.append({
                'benchmark': bench, 'chip': c, 'window_cycles': W_cycles,
                'mean_wss_mb': m_tot * LINE_MB,
                'max_wss_mb': x_tot * LINE_MB,
                'mean_shared_wss_mb': m_sh * LINE_MB,
                'mean_private_wss_mb': m_pr * LINE_MB,
                'max_shared_wss_mb': x_sh * LINE_MB,
                'max_private_wss_mb': x_pr * LINE_MB,
            })
            # per-PC row for this (chip, window)
            for pid, p_raw in enumerate(unique_pcs):
                if pc_mean[pid] == 0 and pc_max[pid] == 0:
                    continue
                per_pc_rows.append({
                    'benchmark': bench, 'chip': c, 'window_cycles': W_cycles,
                    'pc_hex': f"0x{int(p_raw):016x}", 'pc': int(p_raw),
                    'mean_wss_mb': pc_mean[pid] * LINE_MB,
                    'max_wss_mb': int(pc_max[pid]) * LINE_MB,
                })

    if not per_chip_rows:
        return None

    per_chip_df = pd.DataFrame(per_chip_rows)
    per_pc_df = pd.DataFrame(per_pc_rows)

    # chiplet 평균 → per-benchmark row
    agg = per_chip_df.groupby('window_cycles').mean(numeric_only=True)
    agg = agg.drop(columns=['chip']).reset_index()
    agg.insert(0, 'benchmark', bench)
    per_bench_rows = agg.to_dict('records')

    # per-PC 도 chiplet 평균 (같은 pc 를 여러 chiplet 에서 봤을 때 평균)
    if not per_pc_df.empty:
        pc_agg = (per_pc_df.groupby(['window_cycles', 'pc_hex', 'pc'])
                  .mean(numeric_only=True).reset_index())
        pc_agg.insert(0, 'benchmark', bench)
        pc_agg = pc_agg.drop(columns=['chip'])
    else:
        pc_agg = pd.DataFrame()

    return per_bench_rows, per_chip_df, pc_agg


# ── STACK spec (shared_private_capacity.plot_stacked 재활용) ──────────────
STACK_SP = [
    {"col": "mean_shared_wss_mb", "label": "Shared",
     "color": "darkgray", "hatch": None},
    {"col": "mean_private_wss_mb", "label": "Private",
     "color": "white", "hatch": "///"},
]


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
                    help="벤치마크 병렬 워커 수 (trace 로드가 GB 급이라 낮게, 기본 2)")
    ap.add_argument("--windows", type=str,
                    default=",".join(str(w) for w in DEFAULT_WINDOW_CYCLES),
                    help="comma-separated window sizes in cycles "
                         f"(default {DEFAULT_WINDOW_CYCLES})")
    args = ap.parse_args()
    window_cycles_list = _parse_windows(args.windows)

    if not HAS_NUMBA:
        print("WARNING: numba 없음 — 순수 Python fallback 은 대형 trace 에서 몇 시간 걸림. "
              "'pip install numba' 강력 권장.", file=sys.stderr)

    trace_dir = str(memory_trace_dir(args.run_id))
    config = run_config(args.run_id)
    suffix = file_suffix_for_config(config)
    _, n_chiplets = mesh_dims_from_config(config)
    tag = run_tag(args.run_id)

    if args.workers <= 1:
        raw = [process_benchmark(b, trace_dir, suffix, n_chiplets, window_cycles_list)
               for b in benchmarks]
    else:
        with ProcessPoolExecutor(max_workers=args.workers) as ex:
            raw = list(ex.map(process_benchmark, benchmarks,
                              repeat(trace_dir), repeat(suffix), repeat(n_chiplets),
                              repeat(window_cycles_list)))

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

    # window 마다 shared/private stacked bar
    for W_cycles in window_cycles_list:
        sub = per_bench[per_bench['window_cycles'] == W_cycles].copy()
        if sub.empty:
            continue
        sub['total_mb'] = sub['mean_shared_wss_mb'] + sub['mean_private_wss_mb']
        sub = sub.sort_values('total_mb').set_index('benchmark')
        wout = out / f"window{W_cycles}"
        wout.mkdir(parents=True, exist_ok=True)
        sub.to_csv(wout / "per_benchmark.csv")
        plot_stacked(sub, STACK_SP, wout, "wss_stacked")

    print(f"\n[{config}] wrote {out}")
    print(per_bench.to_string(index=False))


if __name__ == "__main__":
    main()
