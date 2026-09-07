"""캐시라인별 hotness WSS — reuse(분류 신호)와 WSS(출력)를 둘 다 window로 잰다.

이전 버전은 reuse만 window로 평균내고 출력은 footprint(라인 union count,
lifetime 내내 한 번이라도 그 tier로 분류된 라인 수)로 내서 WSS 취지에 안 맞았음
— footprint는 시간이 지날수록 계속 커지기만 하는 lifetime 누적값이지 "지금 이
순간 얼마나 상주하나"가 아님. 고침: 분류(tier)는 그대로 windowed-reuse 기반으로
고정하되, 출력은 매 window(tumbling, 겹치지 않음)마다 "그 tier 라인이 몇 개
동시에 활성이었나"를 세서 window 평균 낸 WSS로 낸다 — window.go/sliding_wss.py
의 WSS 개념 그대로, 다만 sliding(연속) 대신 tumbling(고정 구간 나열)이라 더
가볍다.

신호(분류용, tier 하나로 고정):
  reuse   = mean_{active window w} (그 라인이 window w 안에서 접근된 횟수).
            "active" = 그 window 안에서 최소 1번 만져진 window만 평균에 포함.
            burst 강도를 재는 것 — chip 은 안 가리고 전체 pooled.
  sharing = 그 라인을 만진 distinct chiplet 수, 전체 트레이스 기준(lifetime).
  score, tier = decoupled-cluster-size scoreClassify(hop 없음, 0.5/0.5),
                pc_hotness_from_stats.score_classify 재사용.

출력(WSS, tier 별):
  각 (chiplet, window) 쌍에서 "그 chiplet 이 그 window 안에 만진, tier=T인
  distinct 라인 수"를 센다 → window 축으로 평균(0인 window 도 분모에 포함,
  sliding_wss.py 의 시간-가중 평균과 동일 관례) → chiplet 축으로 평균.
  = "이 chiplet 의 L2 가 tier=T 데이터를 위해 평상시 몇 MB 쥐고 있어야 하나".

sharing/score/분류 로직은 이전 버전과 동일 — 바뀐 건 "출력 집계 방식"뿐.

출력 (~/mgvm/figures/cacheline-hotness-windowed-reuse/<config>-<run_tag>/):
  per_benchmark.csv    — benchmark × window_ns → mean_wss_mb_{Cold,Warm,Hot,Global}
                          (+ max_wss_mb_*, 칩렛 평균) + mean_wss_mb_total(검산용)
  per_chiplet.csv      — benchmark × chip × window_ns 상세
  window<N>/wss_by_tier_stacked.png + per_benchmark.csv

사용:
  analyze/run.sh cacheline/cacheline_hotness_windowed_reuse.py <run-id>
  analyze/run.sh cacheline/cacheline_hotness_windowed_reuse.py <run-id> --windows 10,100,1000
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
from motivation_common import file_suffix_for_config, trace_chunks, ALL_TRACE_COLS
from pc_hotness import MeshGeom, mesh_dims_from_config, manhattan_hop, TIER_COLOR
from pc_hotness_from_stats import score_classify, SCORE_W_NOHOP, TIER_ORDER
from shared_private_capacity import plot_stacked, LINE_BYTES
from sliding_wss import _parse_windows
from params import benchmarks

DEMAND_TYPES = np.array([0, 1, 2, 3, 4, 5], dtype=np.int8)
USECOLS = ALL_TRACE_COLS
CYCLE_TO_SEC = 1e-9
DEFAULT_WINDOWS_NS = [10_000, 30_000, 50_000, 70_000, 100_000]

FIG_ROOT = Path.home() / "mgvm" / "figures" / "cacheline-hotness-windowed-reuse"
FIG_FORMATS = ("png",)

TIER_HATCH = {"Cold": None, "Warm": "///", "Hot": "...", "Global": "xxx"}


# ── 단일 청크 패스: raw (line,chip,owner,time) 이벤트 배열 + (line,chip,owner) uniq ──
def _load(trace_file: str):
    line_parts, chip_parts, owner_parts, time_parts = [], [], [], []
    uniq_parts: list[pd.DataFrame] = []
    for chunk in trace_chunks(trace_file, usecols=USECOLS):
        chunk = chunk.drop_duplicates()                # 완전-중복 로그행만 제거(reuse 보존)
        types = chunk["type"].to_numpy(dtype=np.int8)
        is_ptw = chunk["is_ptw"].to_numpy(dtype=np.int8)
        is_d = np.isin(types, DEMAND_TYPES) & (is_ptw == 0)
        if not is_d.any():
            continue
        line = chunk["addr"].to_numpy(dtype=np.uint64)[is_d] >> np.uint64(6)
        chip = chunk["chiplet"].to_numpy(dtype=np.int64)[is_d]
        owner = chunk["owner"].to_numpy(dtype=np.int64)[is_d]
        time = chunk["time"].to_numpy(dtype=np.float64)[is_d]

        line_parts.append(line)
        chip_parts.append(chip)
        owner_parts.append(owner)
        time_parts.append(time)
        uniq_parts.append(pd.DataFrame({"line": line, "chip": chip, "owner": owner})
                          .drop_duplicates())

    if not line_parts:
        return None

    lines = np.concatenate(line_parts)
    chips = np.concatenate(chip_parts)
    owners = np.concatenate(owner_parts)
    times = np.concatenate(time_parts)
    uniq = pd.concat(uniq_parts, ignore_index=True).drop_duplicates()
    return lines, chips, owners, times, uniq


def _windowed_tables(lines: np.ndarray, chips: np.ndarray, times: np.ndarray, window_sec: float):
    """in-memory 재-버킷팅(파일 재읽기 없음).
    반환: reuse_per_line(분류용, chip pooled, active window 평균),
          presence(line,chip,win) 유니크 표(WSS 집계용), num_windows(전역).
    """
    t0 = float(times.min())
    t_max = float(times.max())
    win = ((times - t0) // window_sec).astype(np.int64)

    win_counts = pd.DataFrame({"line": lines, "win": win}).groupby(["line", "win"]).size()
    reuse_per_line = win_counts.groupby(level=0).mean()   # active window 만 평균(burst 강도)

    presence = pd.DataFrame({"line": lines, "chip": chips, "win": win}).drop_duplicates()
    num_windows = max(1, int(np.ceil((t_max - t0) / window_sec)) + 1)
    return reuse_per_line, presence, num_windows


def _line_signals(reuse_per_line: pd.Series, uniq: pd.DataFrame, geom: MeshGeom,
                  sharing: pd.Series, hop_avg: pd.Series) -> pd.DataFrame:
    """라인별 (reuse=windowed 평균, sharing=lifetime, hop_norm=참고용)."""
    lines = sharing.index
    diam = geom.diameter if geom.diameter > 1 else 2
    sig = pd.DataFrame(index=lines)
    sig["reuse"] = reuse_per_line.reindex(lines, fill_value=0.0).astype(np.float64)
    sig["sharing"] = sharing.astype(np.int64)
    sig["hop_norm"] = np.clip((hop_avg.reindex(lines, fill_value=1.0) - 1) / (diam - 1), 0, 1)
    return sig


# ── (chiplet,window) 별 tier=T distinct 라인 수 → window 평균(0-window 포함) → 칩렛 평균 ──
def _wss_by_tier(presence: pd.DataFrame, tier_of_line: pd.Series, n_chiplets: int, num_windows: int):
    p = presence.copy()
    p["tier"] = p["line"].map(tier_of_line)
    per_chip_win_tier = p.groupby(["chip", "win", "tier"]).size()   # (chip,win,tier) -> distinct 라인 수

    chip_idx = pd.RangeIndex(n_chiplets, name="chip")
    mean_by_tier: dict[str, pd.Series] = {}
    max_by_tier: dict[str, pd.Series] = {}
    tiers_present = per_chip_win_tier.index.get_level_values("tier").unique() if len(per_chip_win_tier) else []
    for tier in TIER_ORDER:
        if tier in tiers_present:
            s = per_chip_win_tier.xs(tier, level="tier")           # index=(chip,win)
            chip_sum = s.groupby(level="chip").sum().reindex(chip_idx, fill_value=0)
            chip_max = s.groupby(level="chip").max().reindex(chip_idx, fill_value=0)
        else:
            chip_sum = pd.Series(0, index=chip_idx)
            chip_max = pd.Series(0, index=chip_idx)
        mean_by_tier[tier] = chip_sum / num_windows   # 0-count window 도 분모에 포함(시간-가중 평균과 동일 관례)
        max_by_tier[tier] = chip_max
    return mean_by_tier, max_by_tier


def _classify_and_aggregate(bench: str, window_ns: int, sig: pd.DataFrame,
                            presence: pd.DataFrame, num_windows: int, geom: MeshGeom):
    tier_arr, score_arr = score_classify(
        sig["reuse"].to_numpy(dtype=np.float64),
        sig["sharing"].to_numpy(dtype=np.float64),
        np.zeros(len(sig)), SCORE_W_NOHOP)
    tier_of_line = pd.Series(tier_arr, index=sig.index)

    mean_by_tier, max_by_tier = _wss_by_tier(presence, tier_of_line, geom.num, num_windows)

    row = {"benchmark": bench, "window_ns": window_ns,
          "mean_reuse_windowed": float(sig["reuse"].mean()),
          "mean_sharing": float(sig["sharing"].mean()),
          "mean_score": float(score_arr.mean())}
    per_chip_cols = {}
    for tier in TIER_ORDER:
        mb = mean_by_tier[tier] * LINE_BYTES / (1024.0 ** 2)
        per_chip_cols[f"{tier.lower()}_wss_mb"] = mb
        row[f"mean_wss_mb_{tier}"] = float(mb.mean())
        row[f"max_wss_mb_{tier}"] = float((max_by_tier[tier] * LINE_BYTES / (1024.0 ** 2)).mean())
    row["mean_wss_mb_total"] = sum(row[f"mean_wss_mb_{t}"] for t in TIER_ORDER)

    per_chip_df = pd.DataFrame(per_chip_cols)
    per_chip_df["benchmark"] = bench
    per_chip_df["window_ns"] = window_ns
    return row, per_chip_df.reset_index()


# ── per-benchmark worker: 트레이스 한 번 로드, window 크기마다 재-버킷팅만 반복 ──
def process_benchmark(bench: str, trace_dir: str, suffix: str, geom: MeshGeom,
                      windows_ns: list[int]):
    trace_file = f"{trace_dir}/{bench}_{suffix}_memory_trace.csv"
    if not os.path.exists(trace_file):
        return None
    print(f"[{bench}] cacheline hotness WSS (windowed reuse) ...", flush=True)
    try:
        loaded = _load(trace_file)
        if loaded is None:
            return None
        lines, chips, owners, times, uniq = loaded
        if uniq.empty:
            return None

        # window 크기 무관 신호 — 한 번만 계산.
        sharing = uniq.groupby("line")["chip"].nunique()
        hop = manhattan_hop(uniq["owner"].to_numpy(), uniq["chip"].to_numpy(), geom.n_side)
        hop_avg = pd.Series(hop, index=uniq.index).groupby(uniq["line"]).mean()

        rows, per_chip_frames = [], []
        for window_ns in windows_ns:
            window_sec = window_ns * CYCLE_TO_SEC
            reuse_per_line, presence, num_windows = _windowed_tables(lines, chips, times, window_sec)
            sig = _line_signals(reuse_per_line, uniq, geom, sharing, hop_avg)
            row, per_chip_df = _classify_and_aggregate(bench, window_ns, sig, presence, num_windows, geom)
            rows.append(row)
            per_chip_frames.append(per_chip_df)

        return rows, pd.concat(per_chip_frames, ignore_index=True)
    except Exception as e:  # noqa: BLE001
        print(f"[{bench}] error: {e}")
        return None


def run_one(run_id: str, workers: int, windows_ns: list[int]):
    trace_dir = str(memory_trace_dir(run_id))
    config = run_config(run_id)
    suffix = file_suffix_for_config(config)
    n_side, n_chiplets = mesh_dims_from_config(config)
    geom = MeshGeom(n_side=n_side, num=n_chiplets)

    if workers <= 1:
        raw = [process_benchmark(b, trace_dir, suffix, geom, windows_ns) for b in benchmarks]
    else:
        with ProcessPoolExecutor(max_workers=workers) as ex:
            raw = list(ex.map(process_benchmark, benchmarks,
                              repeat(trace_dir), repeat(suffix), repeat(geom), repeat(windows_ns)))

    all_rows, per_chip_frames = [], []
    for r in raw:
        if r is None:
            continue
        rows, per_chip_df = r
        all_rows.extend(rows)
        per_chip_frames.append(per_chip_df)
    if not all_rows:
        return config, None, None

    df = pd.DataFrame(all_rows)
    per_chip_df = pd.concat(per_chip_frames, ignore_index=True)
    return config, df, per_chip_df


STACK_WSS = [
    {"col": f"mean_wss_mb_{t}", "label": t, "color": TIER_COLOR[t], "hatch": TIER_HATCH[t]}
    for t in TIER_ORDER
]


def main():
    ap = argparse.ArgumentParser(description=__doc__,
                                 formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("run_id", nargs="?", default="mesh-6x6/latest")
    ap.add_argument("-j", "--workers", type=int, default=4,
                    help="benchmark parallel workers (default 4, 1=sequential)")
    ap.add_argument("--windows", type=str,
                    default=",".join(str(w) for w in DEFAULT_WINDOWS_NS),
                    help=f"comma-separated window sizes in cycles (default {DEFAULT_WINDOWS_NS})")
    args = ap.parse_args()
    windows_ns = _parse_windows(args.windows)

    config, df, per_chip_df = run_one(args.run_id, args.workers, windows_ns)
    if df is None:
        print("no usable benchmarks", file=sys.stderr)
        sys.exit(1)

    tag = run_tag(args.run_id)
    out = FIG_ROOT / f"{config}-{tag}"
    out.mkdir(parents=True, exist_ok=True)
    df.to_csv(out / "per_benchmark.csv", index=False)
    per_chip_df.to_csv(out / "per_chiplet.csv", index=False)

    for window_ns in windows_ns:
        sub = df[df["window_ns"] == window_ns].copy()
        if sub.empty:
            continue
        sub = sub.sort_values("mean_wss_mb_total").set_index("benchmark")
        wout = out / f"window{window_ns}"
        wout.mkdir(parents=True, exist_ok=True)
        sub.to_csv(wout / "per_benchmark.csv")
        plot_stacked(sub, STACK_WSS, wout, "wss_by_tier_stacked")

    print(f"\n[{config}] wrote {out}")
    show = ["benchmark", "window_ns"] + [f"mean_wss_mb_{t}" for t in TIER_ORDER] + ["mean_wss_mb_total"]
    print(df[show].sort_values(["window_ns", "mean_wss_mb_total"]).to_string(index=False))


if __name__ == "__main__":
    main()
