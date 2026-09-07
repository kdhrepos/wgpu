"""sliding-window WSS, tier 별로 쪼갬 — sliding_wss.py x pc_hotness_per_time.py.

sliding_wss.py 는 chiplet 하나의 L2 관점에서 "지금부터 뒤로 W cycle 안에 접근된
distinct 캐시라인 수"(WSS)를 시간-가중 평균으로 잰다. per-PC 분리는 이미 있지만
(그 라인을 owner PC 기준으로 귀속), hotness tier 별 분리는 없었다.

여기선 pc_hotness_per_time.py 의 windowed classify()(pctable.go Decay() 재분류를
그대로 이식 — 그 시점까지 누적된 reuse/sharing 으로 매 classification-window 마다
재분류, minReqCount 워밍업 게이트도 그 시점 기준)로 "이 PC 가 시각 t 에 어떤
tier 였는가"를 구하고, WSS 적분을 그 시점의 tier 로 라우팅한다.

핵심: WSS 적분(sliding_wss.py 의 pc_contrib 누적과 동일한 change-detection 방식,
pc_wss[pc] 가 바뀔 때만 flush)은 종종 classification window 경계를 넘나드는 긴
구간을 한 번에 flush 한다(그 PC 가 오래 window 에 안 나갔다 안 들어왔다 하면).
그 구간을 통짜로 "flush 시점의 tier" 에 몰아주면 부정확 — 그래서 flush 할 때마다
구간을 classification window 경계로 쪼개서, 각 조각을 그 조각이 속한 window 의
tier 로 정확히 분배한다(_accumulate_tier). 단순히 "PC 하나 = tier 하나"로 정하고
WSS 전체를 몰아주면, PC 의 reuse 비율이 시간에 따라 단조증가하는 특성상 Global
로 과대평가된다는 걸 이미 확인했음(pc_hotness_footprint_windowed.py 를 만든
이유와 동일) — 여기서도 같은 이유로 시간-정확한 라우팅이 필요.

NonShared(Warm_NS/Hot_NS)/Streaming 은 Cold 로 접는다(이전 결정과 동일 규약).
RDMA 요청이 아예 없는 PC(순수 로컬)는 전 구간 Cold 로 기본값(실제로도 PCTable
엔트리가 없으니 Cold).

sliding_wss.py 와 pc_hotness_per_time.py 는 각자 다른 t0(윈도 원점)로 windowing
한다 — RDMA-미스 이벤트 집합(classification 용)은 전체 demand 이벤트 집합(WSS 용)
의 부분집합이라 첫 이벤트 시각이 다르다. 두 계산의 window index 가 어긋나면 안
되므로, 전체 demand 이벤트의 최솟값 시각을 공통 원점으로 pc_hotness_per_time._load
에 넘긴다(그 함수에 t0 파라미터 추가함, 옵션이라 기존 호출자엔 영향 없음).

출력 (~/mgvm/figures/sliding-wss-hotness/<config>-<tag>/):
  per_benchmark.csv  — benchmark × window_cycles → mean_wss_mb_{Cold,Warm,Hot,Global}
                        (chiplet 평균) + mean_wss_mb_total(합 검산용, 4개 tier 합과 일치)
  per_chiplet.csv    — 상세
  wss_by_tier_stacked.png — window_cycles 마다 4-tier stacked bar

사용:
  analyze/run.sh memory-trace/sliding_wss_hotness.py <run-id>
  analyze/run.sh memory-trace/sliding_wss_hotness.py <run-id> --windows 10000,50000 \
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
from sliding_wss import _load as _load_full_demand, _prescan, LINE_MB, CYCLE_TO_SEC, _parse_windows
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

DEFAULT_WINDOW_CYCLES = [10_000, 30_000, 50_000, 70_000, 100_000]
DEFAULT_CLASS_WINDOW_NS = 50_000
DEFAULT_WS_WINDOW = 7
DEFAULT_MIN_REQ = 100

FIG_ROOT = Path.home() / "mgvm" / "figures" / "sliding-wss-hotness"
FIG_FORMATS = ("png",)


# ── numba: 구간 [t_start,t_end) x value 를 classification window 경계로 쪼개서
#    각 조각을 그 window 의 tier 에 배분 ────────────────────────────────────
@njit(cache=True)
def _accumulate_tier(pc_tier_contrib, pc, t_start, t_end, value,
                     pc_tier_win, win_size_sec, num_windows):
    if value == 0.0 or t_end <= t_start:
        return
    w = int(t_start // win_size_sec)
    if w < 0:
        w = 0
    if w >= num_windows:
        w = num_windows - 1
    t = t_start
    while t < t_end:
        win_end_t = (w + 1) * win_size_sec
        seg_end = t_end
        if win_end_t < seg_end:
            seg_end = win_end_t
        dur = seg_end - t
        tier = pc_tier_win[pc, w]
        pc_tier_contrib[pc, tier] += value * dur
        t = seg_end
        w += 1
        if w >= num_windows and t < t_end:
            tier = pc_tier_win[pc, num_windows - 1]
            pc_tier_contrib[pc, tier] += value * (t_end - t)
            break


@njit(cache=True)
def _slide_one_tier(times, lines, owner_pcs, window_size,
                    pc_tier_win, win_size_sec, num_windows, M, K):
    """sliding_wss._slide_one 과 동일한 two-pointer + change-detection 적분이지만,
    pc_contrib(스칼라, PC 당 하나) 대신 pc_tier_contrib[K,4](PC x tier)로 라우팅."""
    n = len(times)
    pc_tier_contrib = np.zeros((K, 4), dtype=np.float64)
    if n < 2:
        return pc_tier_contrib, 0.0, 0

    line_count = np.zeros(M, dtype=np.int32)
    pc_wss = np.zeros(K, dtype=np.int32)
    pc_last_change = np.full(K, times[0], dtype=np.float64)

    wss_total = 0
    max_total = 0
    sum_total = 0.0

    L = 0
    for R in range(n):
        tR = times[R]
        cutoff = tR - window_size
        while L < R and times[L] < cutoff:
            l = lines[L]
            line_count[l] -= 1
            if line_count[l] == 0:
                wss_total -= 1
                pc = owner_pcs[l]
                if pc >= 0:
                    _accumulate_tier(pc_tier_contrib, pc, pc_last_change[pc], tR,
                                     np.float64(pc_wss[pc]), pc_tier_win, win_size_sec, num_windows)
                    pc_wss[pc] -= 1
                    pc_last_change[pc] = tR
            L += 1
        l = lines[R]
        if line_count[l] == 0:
            wss_total += 1
            pc = owner_pcs[l]
            if pc >= 0:
                _accumulate_tier(pc_tier_contrib, pc, pc_last_change[pc], tR,
                                 np.float64(pc_wss[pc]), pc_tier_win, win_size_sec, num_windows)
                pc_wss[pc] += 1
                pc_last_change[pc] = tR
        line_count[l] += 1
        if wss_total > max_total:
            max_total = wss_total
        if R < n - 1:
            sum_total += wss_total * (times[R + 1] - tR)

    t_end = times[n - 1]
    for k in range(K):
        v = np.float64(pc_wss[k])
        if v != 0.0:
            _accumulate_tier(pc_tier_contrib, k, pc_last_change[k], t_end, v,
                             pc_tier_win, win_size_sec, num_windows)

    total_t = times[n - 1] - times[0]
    mean_total = sum_total / total_t if total_t > 0 else 0.0
    return pc_tier_contrib, mean_total, max_total


# ── per-benchmark ────────────────────────────────────────────────────────────
def process_benchmark(bench: str, trace_dir: str, suffix: str, n_side: int, n_chiplets: int,
                      window_cycles_list: list[int], class_window_ns: int,
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
        if len(ch_times) < 2:
            continue

        unique_lines, ch_lines_compact = np.unique(ch_lines, return_inverse=True)
        M = len(unique_lines)
        owner_arr = np.full(M, -1, dtype=np.int32)
        for cid, raw_line in enumerate(unique_lines):
            p = owner_pc_map.get((c, int(raw_line)), None)
            if p is not None:
                owner_arr[cid] = pc_compact[p]
        ch_lines_compact = ch_lines_compact.astype(np.int32)

        total_t = ch_times[-1] - ch_times[0]
        for W_cycles in window_cycles_list:
            W_sec = W_cycles * CYCLE_TO_SEC
            pc_tier_contrib, mean_total, _max_total = _slide_one_tier(
                ch_times, ch_lines_compact, owner_arr, W_sec,
                pc_tier_win, class_window_sec, num_windows, M, K)
            tier_mean = (pc_tier_contrib.sum(axis=0) / total_t) if total_t > 0 else np.zeros(4)
            row = {"benchmark": bench, "chip": c, "window_cycles": W_cycles,
                  "mean_wss_mb_total": mean_total * LINE_MB}
            for ti, name in enumerate(TIER_ORDER):
                row[f"mean_wss_mb_{name}"] = tier_mean[ti] * LINE_MB
            per_chip_rows.append(row)

    if not per_chip_rows:
        return None

    per_chip_df = pd.DataFrame(per_chip_rows)
    agg = per_chip_df.groupby("window_cycles").mean(numeric_only=True).drop(columns=["chip"]).reset_index()
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
                    default=",".join(str(w) for w in DEFAULT_WINDOW_CYCLES),
                    help=f"sliding WSS window sizes in cycles (default {DEFAULT_WINDOW_CYCLES})")
    ap.add_argument("--class-window-ns", type=int, default=DEFAULT_CLASS_WINDOW_NS,
                    help="hotness 재분류 주기 (pctable.go samplingPeriod, cycles 단위), "
                         f"기본 {DEFAULT_CLASS_WINDOW_NS}")
    ap.add_argument("--ws-window", type=int, default=DEFAULT_WS_WINDOW,
                    help=f"footprint clamp 용 trailing window 수, 기본 {DEFAULT_WS_WINDOW}")
    ap.add_argument("--min-req", type=int, default=DEFAULT_MIN_REQ,
                    help=f"minReqCountForClassification, 기본 {DEFAULT_MIN_REQ}")
    args = ap.parse_args()
    window_cycles_list = _parse_windows(args.windows)

    if not HAS_NUMBA:
        print("WARNING: numba 없음 — 순수 Python fallback 은 대형 trace 에서 매우 느림. "
              "'pip install numba' 강력 권장.", file=sys.stderr)

    trace_dir = str(memory_trace_dir(args.run_id))
    config = run_config(args.run_id)
    suffix = file_suffix_for_config(config)
    n_side, n_chiplets = mesh_dims_from_config(config)
    tag = run_tag(args.run_id)

    if args.workers <= 1:
        raw = [process_benchmark(b, trace_dir, suffix, n_side, n_chiplets, window_cycles_list,
                                 args.class_window_ns, args.ws_window, args.min_req)
              for b in benchmarks]
    else:
        with ProcessPoolExecutor(max_workers=args.workers) as ex:
            raw = list(ex.map(process_benchmark, benchmarks,
                              repeat(trace_dir), repeat(suffix), repeat(n_side), repeat(n_chiplets),
                              repeat(window_cycles_list), repeat(args.class_window_ns),
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

    for W_cycles in window_cycles_list:
        sub = per_bench[per_bench["window_cycles"] == W_cycles].copy()
        if sub.empty:
            continue
        sub = sub.sort_values("mean_wss_mb_total").set_index("benchmark")
        wout = out / f"window{W_cycles}"
        wout.mkdir(parents=True, exist_ok=True)
        sub.to_csv(wout / "per_benchmark.csv")
        plot_stacked(sub, STACK_TIER, wout, "wss_by_tier_stacked")

    print(f"\n[{config}] wrote {out}")
    print(per_bench.to_string(index=False))


if __name__ == "__main__":
    main()
