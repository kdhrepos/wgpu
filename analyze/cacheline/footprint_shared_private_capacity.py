"""벤치마크별 shared / private capacity(WSS) — footprint 툴의 windowed WSS +
stack-distance 기반, 둘 다 정리.

memory trace 대신 mgpusim/cmd/footprint(host-side address-stream replay)가 만든
summary.json 을 읽는다. 두 종류의 WSS를 뽑는다:

  window 기반 (footprint/window.go, Denning bucketed WSS)
    windows[] 의 각 window_size 마다 shared_mean_mb / private_mean_mb(전 칩렛
    평균) — shared_private_capacity.py 와 동일한 컬럼(shared_capacity_mb,
    avg_private_capacity_mb, total_capacity_mb)으로 맞춰 window_size 별 long-form.

  stack-distance 기반 (footprint/stackdist.go, Mattson/Olken exact reuse distance)
    stack_distance.summary[] 의 각 threshold_mb 마다 cross-chiplet mean/median/
    min/max hit rate. 이건 칩렛별 "own reference stream" 하나라 shared/private로
    쪼갤 수 없음(recorder 주석 참고) — 대신 hit-rate 목표(90/95/99/99.9%)를
    만족하는 최소 threshold_mb 를 "stack-distance WSS" 로 뽑는다.

출력 (~/mgvm/figures/shared-private-capacity/footprint-<config>-<tag>/):
  window_wss.csv                     benchmark x window_size long-form
  stackdist_hitrate.csv               benchmark x threshold_mb long-form (mean/median/min/max hit rate)
  stackdist_wss.csv                   benchmark x hit-rate 목표 -> 최소 threshold_mb
  window<N>/shared_private_stacked.png / per_benchmark.csv
                           (window_size 마다 하나씩, shared_private_capacity.py 와
                           동일한 2-way stacked bar — 기본 동작, --window all)

사용:
  analyze/run.sh cacheline/footprint_shared_private_capacity.py <run-id>              # 모든 window_size 각각 plot
  analyze/run.sh cacheline/footprint_shared_private_capacity.py <run-id> --window 10000000  # 그 하나만
  analyze/run.sh cacheline/footprint_shared_private_capacity.py <run-id> --window none      # plot 생략, 표만
"""

from __future__ import annotations

import argparse
import json
import sys
from pathlib import Path

import pandas as pd

from _paths import footprint_dir, run_config, run_tag
from shared_private_capacity import plot_stacked

FIG_ROOT = Path.home() / "mgvm" / "figures" / "shared-private-capacity"
HIT_RATE_TARGETS = (0.90, 0.95, 0.99, 0.999)

# footprint window 는 aggregate-per-bucket 값(전 chiplet 합) 을 낸다.
# shared_private_capacity.py 의 avg_* (per-chiplet mean) 와 축이 다르므로
# STACK 을 여기서 따로 정의 — 컬럼명도 shared_capacity_mb / avg_private_capacity_mb
# (예전 이름 유지, footprint 만의 aggregate semantic).
STACK_2WAY_FOOTPRINT = [
    {"col": "shared_capacity_mb", "label": "Shared (agg)",
     "color": "darkgray", "hatch": None},
    {"col": "avg_private_capacity_mb", "label": "Private (agg)",
     "color": "white", "hatch": "///"},
]


def _load_summaries(run_id: str) -> list[tuple[str, dict]]:
    fdir = footprint_dir(run_id)
    out = []
    for summary_path in sorted(fdir.glob("*/summary.json")):
        bench = summary_path.parent.name.rsplit("_", 1)[0]
        out.append((bench, json.loads(summary_path.read_text())))
    return out


def load_windows(run_id: str) -> pd.DataFrame:
    """benchmark x window_size long-form, shared_private_capacity.py 와 동일 컬럼.

    num_windows==0(전체 tick 수가 그 window_size 한 버킷도 못 채움 — TouchN 위주
    replay는 total_accesses가 커도 tick 수는 훨씬 작을 수 있음, window.go 참고)인
    행은 0.0MB가 아니라 NaN으로 표시 — "footprint 없음"과 "이 window에서는 잴
    표본이 아예 없음"을 구분."""
    rows = []
    for bench, s in _load_summaries(run_id):
        for w in s.get("windows", []):
            no_sample = w["num_windows"] == 0
            rows.append({
                "benchmark": bench,
                "window_size": w["window_size"],
                "num_windows": w["num_windows"],
                "shared_capacity_mb": float("nan") if no_sample else w["shared_mean_mb"],
                "avg_private_capacity_mb": float("nan") if no_sample else w["private_mean_mb"],
            })
    if not rows:
        return pd.DataFrame()
    df = pd.DataFrame(rows)
    df["total_capacity_mb"] = df["shared_capacity_mb"] + df["avg_private_capacity_mb"]
    return df.sort_values(["benchmark", "window_size"]).reset_index(drop=True)


def load_stackdist(run_id: str) -> tuple[pd.DataFrame, pd.DataFrame]:
    """(hitrate curve long-form, hit-rate-target -> min capacity) 쌍."""
    curve_rows, wss_rows = [], []
    for bench, s in _load_summaries(run_id):
        sd = s.get("stack_distance")
        if sd is None:
            print(f"[{bench}] no stack_distance in summary.json", file=sys.stderr)
            continue
        summary = sorted(sd["summary"], key=lambda t: t["threshold_mb"])
        for t in summary:
            curve_rows.append({
                "benchmark": bench,
                "threshold_mb": t["threshold_mb"],
                "mean_hit_rate": t["mean_hit_rate"],
                "median_hit_rate": t["median_hit_rate"],
                "min_hit_rate": t["min_hit_rate"],
                "max_hit_rate": t["max_hit_rate"],
            })

        wss = {"benchmark": bench}
        for target in HIT_RATE_TARGETS:
            hit = next((t["threshold_mb"] for t in summary
                        if t["mean_hit_rate"] >= target), None)
            wss[f"wss_mb_at_{target:.3f}".rstrip("0").rstrip(".")] = hit
        wss["max_threshold_mb"] = summary[-1]["threshold_mb"] if summary else None
        wss["max_threshold_hit_rate"] = summary[-1]["mean_hit_rate"] if summary else None
        wss_rows.append(wss)

    curve_df = pd.DataFrame(curve_rows)
    wss_df = pd.DataFrame(wss_rows)
    if not wss_df.empty:
        wss_df = wss_df.set_index("benchmark").sort_index()
    return curve_df, wss_df


def load_single_window(run_id: str, window: int) -> pd.DataFrame:
    df = load_windows(run_id)
    if df.empty:
        return df
    df = df[df["window_size"] == window].drop(columns="window_size")
    if df.empty:
        print(f"no benchmark has window_size={window}", file=sys.stderr)
        return df
    dropped = df.loc[df["num_windows"] == 0, "benchmark"].tolist()
    if dropped:
        print(f"[window={window}] no full bucket for {dropped} (num_windows=0, "
              f"total ticks < window size) — excluded from plot", file=sys.stderr)
    df = df[df["num_windows"] > 0].drop(columns="num_windows")
    if df.empty:
        return df
    return df.set_index("benchmark").sort_values("total_capacity_mb")


def _window_arg(s: str):
    if s in ("all", "none"):
        return s
    return int(s)


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("run_id", nargs="?", default="footprint-6x6/latest")
    ap.add_argument("--window", type=_window_arg, default="all",
                     help="'all'(기본, window_size 마다 하나씩 plot) / 'none'(plot 생략) / "
                          "특정 window_size 정수 하나만")
    args = ap.parse_args()

    config = run_config(args.run_id)
    tag = run_tag(args.run_id)
    out = FIG_ROOT / f"footprint-{config}-{tag}"
    out.mkdir(parents=True, exist_ok=True)

    window_df = load_windows(args.run_id)
    curve_df, wss_df = load_stackdist(args.run_id)

    if window_df.empty and curve_df.empty:
        print("no usable benchmarks", file=sys.stderr)
        sys.exit(1)

    if not window_df.empty:
        window_df.to_csv(out / "window_wss.csv", index=False)
        print(f"\n[window WSS] wrote {out / 'window_wss.csv'}")
        print(window_df.to_string(index=False))

    if not curve_df.empty:
        curve_df.to_csv(out / "stackdist_hitrate.csv", index=False)
        wss_df.to_csv(out / "stackdist_wss.csv")
        print(f"\n[stack-distance WSS] wrote {out / 'stackdist_wss.csv'}")
        print(wss_df.to_string())

    if args.window == "none" or window_df.empty:
        return

    sizes = sorted(window_df["window_size"].unique()) if args.window == "all" else [args.window]
    for size in sizes:
        single = load_single_window(args.run_id, size)
        if single.empty:
            continue
        wout = out / f"window{size}"
        wout.mkdir(parents=True, exist_ok=True)
        single.to_csv(wout / "per_benchmark.csv")
        plot_stacked(single, STACK_2WAY_FOOTPRINT, wout, "shared_private_stacked")
        print(f"\n[{config}] window={size} wrote {wout / 'per_benchmark.csv'}")
        print(single.to_string())


if __name__ == "__main__":
    main()
