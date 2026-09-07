"""sliding-window WSS(event-count 축), tier 별로 쪼갬 — sliding_wss_events.py x
pc_hotness_per_time.py.

sliding_wss_hotness.py 의 event-count 버전. window 축만 다르다(이벤트 개수 vs
cycle) — 개념/정합성 검증은 sliding_wss_hotness.py 와 동일, 차이점만 적는다:

  mean 은 "이벤트당 균등 샘플"(Σ WSS(R)/N, sliding_wss_events.py 와 동일 관례)
  이라, tier 분배도 시간-비례가 아니라 **이벤트 개수**로 쪼개야 sum(tier)==total
  이 정확히 보존된다. 그런데 hotness 재분류는 여전히 실제 cycle(시간) 축으로
  일어나므로(pctable.go Decay() 주기), "이 PC 의 WSS 가 [R_start,R_end) 이벤트
  동안 상수값 v 였다"는 구간을, 그 구간에 속한 각 이벤트의 **실제 타임스탬프**
  로 classification window 를 찾아 이벤트 개수 단위로 쪼갠다 — 이벤트 하나하나
  순회하면 느려질 수 있어서, 이미 시간순 정렬된 그 구간의 times[] 위에서
  classification window 경계 시각을 binary search 해서 "이 window 에 이벤트가
  몇 개 들어가는지" 를 찾는다(_accumulate_tier_events). 시간-비례 근사가 아니라
  실제 이벤트 개수를 정확히 세는 것 — sliding_wss_hotness.py 의 시간-구간
  쪼개기와 대응되는, event-count 버전의 정확한 등가물.

NonShared/Streaming -> Cold 로 접는 것, t0 정렬(전체 demand 이벤트 최솟값 시각을
공통 원점으로 pc_hotness_per_time._load 에 넘기는 것) 등은 sliding_wss_hotness.py
와 동일.

출력 (~/mgvm/figures/sliding-wss-events-hotness/<config>-<tag>/):
  per_benchmark.csv  — benchmark × window_events → mean_wss_mb_{Cold,Warm,Hot,Global}
                        (chiplet 평균) + mean_wss_mb_total(합 검산용)
  per_chiplet.csv    — 상세
  wss_by_tier_stacked.png

사용:
  analyze/run.sh memory-trace/sliding_wss_events_hotness.py <run-id>
  analyze/run.sh memory-trace/sliding_wss_events_hotness.py <run-id> --windows 10000,50000 \
      --class-window-ns 50000 -j 2
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
from pc_hotness import mesh_dims_from_config, TIER_COLOR
from shared_private_capacity import plot_stacked
from sliding_wss import _load as _load_full_demand, _prescan, LINE_MB, CYCLE_TO_SEC
from sliding_wss_events import _parse_windows
from pc_hotness_per_time import (
    _load as _load_windowed, _owner_cumulative, _pc_timeseries,
    STREAMING, COLD, WARM_NS, HOT_NS, WARM, HOT, GLOBAL,
)
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

TIER_ORDER = ["Cold", "Warm", "Hot", "Global"]
TIER_ID_OF_ENUM = {STREAMING: 0, COLD: 0, WARM_NS: 0, HOT_NS: 0, WARM: 1, HOT: 2, GLOBAL: 3}

DEFAULT_WINDOW_EVENTS = [10_000, 30_000, 50_000, 70_000, 100_000]
DEFAULT_CLASS_WINDOW_NS = 50_000
DEFAULT_WS_WINDOW = 7
DEFAULT_MIN_REQ = 100

FIG_ROOT = Path.home() / "mgvm" / "figures" / "sliding-wss-events-hotness"
FIG_FORMATS = ("png",)


# ── 이벤트-index 구간 [r_start,r_end) x value 를, 그 구간의 실제 타임스탬프로
#    classification window 경계를 binary search 해서 "이벤트 개수" 단위로 쪼갬 ──
@njit(cache=True)
def _accumulate_tier_events(pc_tier_contrib, pc, times, r_start, r_end, value,
                            pc_tier_win, win_size_sec, num_windows):
    if value == 0.0 or r_end <= r_start:
        return
    t_start = times[r_start]
    w = int(t_start // win_size_sec)
    if w < 0:
        w = 0
    if w >= num_windows:
        w = num_windows - 1
    pos = r_start
    while pos < r_end:
        win_end_t = (w + 1) * win_size_sec
        # binary search: 첫 index >= pos, < r_end, times[idx] >= win_end_t
        lo, hi = pos, r_end
        while lo < hi:
            mid = (lo + hi) // 2
            if times[mid] < win_end_t:
                lo = mid + 1
            else:
                hi = mid
        seg_end = lo
        cnt = seg_end - pos
        if cnt > 0:
            tier = pc_tier_win[pc, w]
            pc_tier_contrib[pc, tier] += value * cnt
        pos = seg_end
        w += 1
        if w >= num_windows and pos < r_end:
            tier = pc_tier_win[pc, num_windows - 1]
            pc_tier_contrib[pc, tier] += value * (r_end - pos)
            break


@njit(cache=True)
def _slide_events_tier(times, lines, owner_pcs, window_events,
                       pc_tier_win, win_size_sec, num_windows, M, K):
    """sliding_wss_events._slide_events_one 과 동일한 event-count two-pointer +
    change-detection 적분이지만, pc_contrib(PC 당 스칼라) 대신
    pc_tier_contrib[K,4](PC x tier, 이벤트-개수 단위)로 라우팅."""
    n = len(lines)
    pc_tier_contrib = np.zeros((K, 4), dtype=np.float64)
    if n < 1:
        return pc_tier_contrib, 0.0, 0

    line_count = np.zeros(M, dtype=np.int32)
    pc_wss = np.zeros(K, dtype=np.int32)
    pc_last_change = np.zeros(K, dtype=np.int64)   # 이벤트 index (시각 아님)

    wss_total = 0
    max_total = 0
    sum_total = 0

    for R in range(n):
        if R >= window_events:
            L_out = R - window_events
            l = lines[L_out]
            line_count[l] -= 1
            if line_count[l] == 0:
                wss_total -= 1
                pc = owner_pcs[l]
                if pc >= 0:
                    _accumulate_tier_events(pc_tier_contrib, pc, times, pc_last_change[pc], R,
                                            np.float64(pc_wss[pc]), pc_tier_win, win_size_sec, num_windows)
                    pc_wss[pc] -= 1
                    pc_last_change[pc] = R
        l = lines[R]
        if line_count[l] == 0:
            wss_total += 1
            pc = owner_pcs[l]
            if pc >= 0:
                _accumulate_tier_events(pc_tier_contrib, pc, times, pc_last_change[pc], R,
                                        np.float64(pc_wss[pc]), pc_tier_win, win_size_sec, num_windows)
                pc_wss[pc] += 1
                pc_last_change[pc] = R
        line_count[l] += 1
        if wss_total > max_total:
            max_total = wss_total
        sum_total += wss_total

    for k in range(K):
        v = np.float64(pc_wss[k])
        if v != 0.0:
            _accumulate_tier_events(pc_tier_contrib, k, times, pc_last_change[k], n,
                                    v, pc_tier_win, win_size_sec, num_windows)

    mean_total = sum_total / n if n > 0 else 0.0
    return pc_tier_contrib, mean_total, max_total


# ── per-benchmark ────────────────────────────────────────────────────────────
def process_benchmark(bench: str, trace_dir: str, suffix: str, n_side: int, n_chiplets: int,
                      window_events_list: list[int], class_window_ns: int,
                      ws_window: int, min_req: int):
    trace_file = f"{trace_dir}/{bench}_{suffix}_memory_trace.csv"
    if not os.path.exists(trace_file):
        return None
    print(f"[{bench}] load trace (full demand) ...", flush=True)
    loaded = _load_full_demand(trace_file)
    if loaded is None:
        return None
    times_all, lines_all, pcs_all, chips_all = loaded
    t0_global = float(times_all.min())
    t_max = float(times_all.max())

    print(f"[{bench}] windowed hotness classification (RDMA-miss subset) ...", flush=True)
    df_w = _load_windowed(trace_file, n_side, class_window_ns, t0=t0_global)
    hop_ref = max(1, 2 * (n_side - 1))
    if df_w is not None and not df_w.empty:
        base = _owner_cumulative(df_w)
        pc_win = _pc_timeseries(df_w, base, n_side, n_side, ws_window, min_req, hop_ref, class_window_ns)
    else:
        pc_win = pd.DataFrame(columns=["win", "pc", "hotness"])

    _is_shared_map, owner_pc_map = _prescan(lines_all, pcs_all, chips_all)

    unique_pcs = np.unique(pcs_all)
    pc_compact = {int(p): i for i, p in enumerate(unique_pcs)}
    K = len(pc_compact)

    class_window_sec = class_window_ns * CYCLE_TO_SEC
    num_windows = max(1, int(np.ceil((t_max - t0_global) / class_window_sec)) + 1)
    pc_tier_win = np.zeros((K, num_windows), dtype=np.int8)  # 기본 Cold(0)

    if not pc_win.empty:
        tiers = pc_win["hotness"].map(TIER_ID_OF_ENUM).to_numpy()
        for w, pc, tier in zip(pc_win["win"].to_numpy(), pc_win["pc"].to_numpy(), tiers):
            pc = int(pc)
            idx = pc_compact.get(pc)
            if idx is None:
                continue
            wi = int(w)
            if 0 <= wi < num_windows:
                pc_tier_win[idx, wi] = tier

    per_chip_rows = []
    for c in range(n_chiplets):
        m = chips_all == c
        if not m.any():
            continue
        ch_times = times_all[m]
        ch_lines = lines_all[m]
        order = np.argsort(ch_times, kind="mergesort")
        ch_times = ch_times[order]
        ch_lines = ch_lines[order]
        if len(ch_times) < 1:
            continue

        unique_lines, ch_lines_compact = np.unique(ch_lines, return_inverse=True)
        M = len(unique_lines)
        owner_arr = np.full(M, -1, dtype=np.int32)
        for cid, raw_line in enumerate(unique_lines):
            p = owner_pc_map.get((c, int(raw_line)), None)
            if p is not None:
                owner_arr[cid] = pc_compact[p]
        ch_lines_compact = ch_lines_compact.astype(np.int32)

        n_events = len(ch_times)
        for W in window_events_list:
            pc_tier_contrib, mean_total, _max_total = _slide_events_tier(
                ch_times, ch_lines_compact, owner_arr, W,
                pc_tier_win, class_window_sec, num_windows, M, K)
            tier_mean = (pc_tier_contrib.sum(axis=0) / n_events) if n_events > 0 else np.zeros(4)
            row = {"benchmark": bench, "chip": c, "window_events": W,
                  "mean_wss_mb_total": mean_total * LINE_MB}
            for ti, name in enumerate(TIER_ORDER):
                row[f"mean_wss_mb_{name}"] = tier_mean[ti] * LINE_MB
            per_chip_rows.append(row)

    if not per_chip_rows:
        return None

    per_chip_df = pd.DataFrame(per_chip_rows)
    agg = per_chip_df.groupby("window_events").mean(numeric_only=True).drop(columns=["chip"]).reset_index()
    agg.insert(0, "benchmark", bench)
    return agg.to_dict("records"), per_chip_df


STACK_TIER = [
    {"col": f"mean_wss_mb_{t}", "label": t, "color": TIER_COLOR[t], "hatch": h}
    for t, h in zip(TIER_ORDER, (None, "///", "...", "xxx"))
]


def main():
    ap = argparse.ArgumentParser(description=__doc__,
                                 formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("run_id", nargs="?", default="mesh-6x6/latest")
    ap.add_argument("-j", "--workers", type=int, default=2,
                    help="벤치마크 병렬 워커 수 (트레이스를 두 번 읽고 windowed 분류까지 해서 무거움, 기본 2)")
    ap.add_argument("--windows", type=str,
                    default=",".join(str(w) for w in DEFAULT_WINDOW_EVENTS),
                    help=f"sliding WSS window sizes in events (default {DEFAULT_WINDOW_EVENTS})")
    ap.add_argument("--class-window-ns", type=int, default=DEFAULT_CLASS_WINDOW_NS,
                    help="hotness 재분류 주기 (pctable.go samplingPeriod, cycles 단위), "
                         f"기본 {DEFAULT_CLASS_WINDOW_NS}")
    ap.add_argument("--ws-window", type=int, default=DEFAULT_WS_WINDOW,
                    help=f"footprint clamp 용 trailing window 수, 기본 {DEFAULT_WS_WINDOW}")
    ap.add_argument("--min-req", type=int, default=DEFAULT_MIN_REQ,
                    help=f"minReqCountForClassification, 기본 {DEFAULT_MIN_REQ}")
    args = ap.parse_args()
    window_events_list = _parse_windows(args.windows)

    if not HAS_NUMBA:
        print("WARNING: numba 없음 — 순수 Python fallback 은 대형 trace 에서 매우 느림. "
              "'pip install numba' 강력 권장.", file=sys.stderr)

    trace_dir = str(memory_trace_dir(args.run_id))
    config = run_config(args.run_id)
    suffix = file_suffix_for_config(config)
    n_side, n_chiplets = mesh_dims_from_config(config)
    tag = run_tag(args.run_id)

    if args.workers <= 1:
        raw = [process_benchmark(b, trace_dir, suffix, n_side, n_chiplets, window_events_list,
                                 args.class_window_ns, args.ws_window, args.min_req)
              for b in benchmarks]
    else:
        with ProcessPoolExecutor(max_workers=args.workers) as ex:
            raw = list(ex.map(process_benchmark, benchmarks,
                              repeat(trace_dir), repeat(suffix), repeat(n_side), repeat(n_chiplets),
                              repeat(window_events_list), repeat(args.class_window_ns),
                              repeat(args.ws_window), repeat(args.min_req)))

    bench_rows, chip_frames = [], []
    for r in raw:
        if r is None:
            continue
        rows, chip_df = r
        bench_rows.extend(rows)
        chip_frames.append(chip_df)

    if not bench_rows:
        print("no usable benchmarks", file=sys.stderr)
        sys.exit(1)

    per_bench = pd.DataFrame(bench_rows)
    per_chip = pd.concat(chip_frames, ignore_index=True)

    out = FIG_ROOT / f"{config}-{tag}"
    out.mkdir(parents=True, exist_ok=True)
    per_bench.to_csv(out / "per_benchmark.csv", index=False)
    per_chip.to_csv(out / "per_chiplet.csv", index=False)

    for W in window_events_list:
        sub = per_bench[per_bench["window_events"] == W].copy()
        if sub.empty:
            continue
        sub = sub.sort_values("mean_wss_mb_total").set_index("benchmark")
        wout = out / f"window{W}"
        wout.mkdir(parents=True, exist_ok=True)
        sub.to_csv(wout / "per_benchmark.csv")
        plot_stacked(sub, STACK_TIER, wout, "wss_by_tier_stacked")

    print(f"\n[{config}] wrote {out}")
    print(per_bench.to_string(index=False))


if __name__ == "__main__":
    main()
