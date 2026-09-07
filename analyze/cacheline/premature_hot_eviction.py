"""Motivation 2 — hot 데이터 조기축출 (premature eviction).

nearfetch 가 복제는 하지만 다시 쓸 hot 데이터를 무분별하게 내쫓는다는 걸 정량화.
단순히 "hot 라인이 evict 된다"만 보이면 "쓸 만큼 쓰고 나간 것 아니냐"는 반박을
받으므로, evict 된 뒤 곧 다시 요청된다(조기축출=낭비)는 걸 access-count window 로 증명.

지표:
  hot 라인   = 최소 1개의 hot 분류 PC 가 touch 한 캐시라인 (OR 규칙).  hot 판정은
               common/pc_hotness.py = pctable.go tier 분류.  기본 hot tier={Hot,Global}.
  premature  = victim(type 8/9)이 축출된 뒤, 같은 L2(칩렛)에서 W 접근 이내에 같은
               캐시라인이 다시 demand 요청됨 → 아직 쓸 거였는데 쫓아냄 = 낭비.
  dead       = W 이내 재요청 없음 = 정당한 축출.
  window 단위 = 사이클이 아니라 **접근 카운트**(벤치 간 공정 비교).  칩렛별 demand
               접근을 시계로 삼아, victim 축출 시점 이후 next-access gap 을 잰다.

그림:
  벤치마크별 막대 4개(window 100 / 1K / 10K / 100K), 각 막대를 premature(빨강)/dead(회색)
  로 stack(비율).  빨강이 지배 = 조기축출 심함.  제일 짧은 100 에서도 높으면 보수적으로
  봐도 문제.  run-id 를 2개 이상 주면(nearfetch vs dynamic) 비교 막대도 뽑는다.

입력:  memory-traces/<bench>_<config>_memory_trace.csv  (victim type 8/9 필요)
출력:  _out/cacheline/premature-eviction/<config>/
         <bench>_premature.csv, premature_stack.png
       (여러 run-id → .../compare/premature_compare.png)

사용:  analyze/run.sh cacheline/premature_hot_eviction.py <run-id>
       analyze/run.sh cacheline/premature_hot_eviction.py nearfetch-6x6 dynamic-dist-6x6
"""

from __future__ import annotations

import argparse
import os
import sys
from concurrent.futures import ProcessPoolExecutor
from itertools import repeat

import numpy as np
import pandas as pd
import matplotlib.pyplot as plt

from _paths import memory_trace_dir, run_config, run_tag, domain_out_dir
from motivation_common import file_suffix_for_config, trace_chunks, ALL_TRACE_COLS
from plot_style import apply_paper_style, save_fig, SIZE_WIDE
from pc_hotness import (
    MeshGeom, mesh_dims_from_config, PCAccum, classify_signals,
    hot_pcs, DEFAULT_HOT_TIERS,
)
from params import benchmarks

DEMAND_TYPES = {0, 1, 2, 3, 4, 5}
VICTIM_TYPES = {8, 9}
DEFAULT_WINDOWS = [100, 1000, 10000, 100000]
PREMATURE_COLOR = "#d73027"
DEAD_COLOR = "#cfcfcf"

USECOLS = ALL_TRACE_COLS       # full-row 읽어 완전-중복 제거(subset dedup 은 reuse 붕괴)


def _hot_lines(trace_file: str, geom: MeshGeom, hot_tiers,
               footprint_clamp: bool) -> set:
    """Pass 1 (청크) — PC 분류 후 hot PC 가 touch 한 캐시라인 집합 (OR 규칙)."""
    acc = PCAccum(geom)
    for chunk in trace_chunks(trace_file, usecols=USECOLS):
        chunk = chunk.drop_duplicates()          # full-row 완전-중복 제거
        dem = chunk[(chunk["type"].isin(DEMAND_TYPES))
                    & (chunk["is_ptw"] == 0) & (chunk["pc"] != 0)]
        if dem.empty:
            continue
        cl = dem["addr"].to_numpy(dtype=np.uint64) >> 6
        acc.update(dem["pc"].to_numpy(dtype=np.uint64), cl,
                   dem["chiplet"].to_numpy(dtype=np.int64),
                   dem["owner"].to_numpy(dtype=np.int64))
    sig = acc.finalize()
    if sig.empty:
        return set()
    cls = classify_signals(sig, geom, footprint_clamp=footprint_clamp)
    return acc.hot_cachelines(hot_pcs(cls, hot_tiers))


def _eviction_gaps(trace_file: str, hot_lines: set) -> np.ndarray:
    """Pass 2 (청크) — hot 라인 한정으로 demand/victim 이벤트를 칩렛별 access-clock 과
    함께 모아, 각 victim 이후 같은-라인 next demand 까지의 gap 을 계산.

    clock 은 칩렛별 **전체** demand 접근 카운트(청크 경계 넘어 누적).  window 는 이
    clock 단위.  hot 라인 이벤트만 저장하므로 메모리는 hot subset 크기로 bounded.
    """
    base: dict[int, int] = {}                       # chip -> 청크 이전까지 demand 수
    dem_parts: list[pd.DataFrame] = []
    vic_parts: list[pd.DataFrame] = []
    hot_arr = pd.Index(sorted(hot_lines))

    for chunk in trace_chunks(trace_file, usecols=USECOLS):
        chunk = chunk.drop_duplicates()          # full-row 완전-중복 제거
        cl = chunk["addr"].to_numpy(dtype=np.uint64) >> 6
        dm = ((chunk["type"].isin(DEMAND_TYPES)) & (chunk["is_ptw"] == 0)).to_numpy()
        vm = chunk["type"].isin(VICTIM_TYPES).to_numpy()
        chip = chunk["chiplet"].to_numpy(dtype=np.int64)

        # 칩렛별 청크-내 누적 demand (file 순서 보존) + 청크 이전 base
        tmp = pd.DataFrame({"chip": chip, "dm": dm.astype(np.int64)})
        cum = tmp.groupby("chip")["dm"].cumsum().to_numpy()
        base_off = tmp["chip"].map(base).fillna(0).to_numpy()
        clock = base_off + cum                       # demand 행=1-based, victim 행=이전 demand 수

        hot_mask = (pd.Index(cl).isin(hot_arr) if len(hot_arr)
                    else np.zeros(len(cl), bool))
        d_sel = dm & hot_mask
        v_sel = vm & hot_mask
        if d_sel.any():
            dem_parts.append(pd.DataFrame({"chip": chip[d_sel], "cl": cl[d_sel],
                                           "clock": clock[d_sel]}))
        if v_sel.any():
            vic_parts.append(pd.DataFrame({"chip": chip[v_sel], "cl": cl[v_sel],
                                           "clock": clock[v_sel]}))
        # base 갱신
        tot = tmp.groupby("chip")["dm"].sum()
        for c, v in tot.items():
            base[c] = base.get(c, 0) + int(v)

    if not vic_parts or not dem_parts:
        return np.array([], dtype=float)
    demand = pd.concat(dem_parts, ignore_index=True)
    victim = pd.concat(vic_parts, ignore_index=True)
    demand["next_clock"] = demand["clock"]
    demand = demand.sort_values("clock", kind="stable")
    victim = victim.sort_values("clock", kind="stable")

    m = pd.merge_asof(victim, demand[["clock", "chip", "cl", "next_clock"]],
                      on="clock", by=["chip", "cl"],
                      direction="forward", allow_exact_matches=False)
    return (m["next_clock"] - m["clock"]).to_numpy(dtype=float)


def process_benchmark(bench: str, trace_dir: str, suffix: str, geom: MeshGeom,
                      windows: list[int], hot_tiers, footprint_clamp: bool):
    trace_file = f"{trace_dir}/{bench}_{suffix}_memory_trace.csv"
    if not os.path.exists(trace_file):
        return None
    print(f"[{bench}] premature-eviction ...", flush=True)
    try:
        hot_lines = _hot_lines(trace_file, geom, hot_tiers, footprint_clamp)
        if not hot_lines:
            print(f"[{bench}] no hot lines — skip")
            return None

        gaps = _eviction_gaps(trace_file, hot_lines)
        total = len(gaps)
        if total == 0:
            print(f"[{bench}] no hot victims — skip")
            return None

        row = {"benchmark": bench, "total_hot_evictions": int(total),
               "n_hot_lines": len(hot_lines)}
        for w in windows:
            prem = int(np.nansum(gaps <= w))
            row[f"premature_w{w}"] = prem
            row[f"dead_w{w}"] = int(total - prem)
            row[f"premature_frac_w{w}"] = round(prem / total, 4)
        return pd.DataFrame([row])
    except Exception as e:  # noqa: BLE001
        print(f"[{bench}] error: {e}")
        return None


def run_one(run_id: str, windows: list[int], hot_tiers, footprint_clamp: bool):
    """단일 run-id 처리 → (config, DataFrame[benchmark×window])."""
    trace_dir = str(memory_trace_dir(run_id))
    config = run_config(run_id)
    suffix = file_suffix_for_config(config)
    n_side, num = mesh_dims_from_config(config)
    geom = MeshGeom(n_side=n_side, num=num)

    with ProcessPoolExecutor() as ex:
        raw = list(ex.map(process_benchmark, benchmarks,
                          repeat(trace_dir), repeat(suffix), repeat(geom),
                          repeat(windows), repeat(hot_tiers),
                          repeat(footprint_clamp)))
    parts = [r for r in raw if r is not None]
    if not parts:
        return config, None
    return config, pd.concat(parts, ignore_index=True).set_index("benchmark")


# ── 단일 run: 벤치별 3-window stacked ────────────────────────────────────────
def plot_stack(df: pd.DataFrame, windows: list[int], out, config: str) -> None:
    apply_paper_style()
    benches = list(df.index)
    n = len(benches)
    nw = len(windows)
    fig, ax = plt.subplots(figsize=SIZE_WIDE)

    group_w = 0.8
    bar_w = group_w / nw
    x0 = np.arange(n)
    for j, w in enumerate(windows):
        frac = df[f"premature_frac_w{w}"].to_numpy()
        xpos = x0 - group_w / 2 + bar_w * (j + 0.5)
        ax.bar(xpos, frac, bar_w, color=PREMATURE_COLOR,
               label="premature" if j == 0 else None, edgecolor="white", linewidth=0.3)
        ax.bar(xpos, 1 - frac, bar_w, bottom=frac, color=DEAD_COLOR,
               label="dead" if j == 0 else None, edgecolor="white", linewidth=0.3)
        # window 라벨을 막대 위에 작게
        for xp, fr in zip(xpos, frac):
            ax.text(xp, 1.01, _wlabel(w), ha="center", va="bottom",
                    rotation=90, fontsize=4.5, color="#555555")

    ax.set_xticks(x0)
    ax.set_xticklabels(benches, rotation=35, ha="right", fontsize=6)
    ax.set_ylabel("hot victim fraction")
    ax.set_ylim(0, 1.12)
    ax.set_yticks(np.arange(0, 1.01, 0.25))
    ax.set_title(f"Premature eviction of hot lines — {config} "
                 f"(bars = window {'/'.join(_wlabel(w) for w in windows)})")
    ax.legend(frameon=False, ncol=2, fontsize=6, loc="upper left")
    ax.grid(axis="y", alpha=0.3)
    save_fig(fig, out, "premature_stack")


# ── 여러 run 비교: 고정 window, config 별 premature 비율 ──────────────────────
def plot_compare(frames: dict[str, pd.DataFrame], window: int, out) -> None:
    apply_paper_style()
    configs = list(frames.keys())
    benches = sorted(set().union(*[set(f.index) for f in frames.values()]))
    n = len(benches)
    nc = len(configs)
    fig, ax = plt.subplots(figsize=SIZE_WIDE)

    group_w = 0.8
    bar_w = group_w / nc
    x0 = np.arange(n)
    col = f"premature_frac_w{window}"
    for j, cfg in enumerate(configs):
        f = frames[cfg]
        vals = [f.loc[b, col] if b in f.index else np.nan for b in benches]
        xpos = x0 - group_w / 2 + bar_w * (j + 0.5)
        ax.bar(xpos, vals, bar_w, color=_ci_color(j), label=cfg,
               edgecolor="white", linewidth=0.3)

    ax.set_xticks(x0)
    ax.set_xticklabels(benches, rotation=35, ha="right", fontsize=6)
    ax.set_ylabel(f"premature fraction (W={_wlabel(window)})")
    ax.set_ylim(0, 1.0)
    ax.set_title("Premature hot-eviction rate — platform comparison")
    ax.legend(frameon=False, ncol=min(nc, 3), fontsize=6, loc="upper right")
    ax.grid(axis="y", alpha=0.3)
    save_fig(fig, out, "premature_compare")


def _ci_color(j: int) -> str:
    from plot_style import color_for
    return color_for(j)


def _wlabel(w: int) -> str:
    if w >= 1000 and w % 1000 == 0:
        return f"{w // 1000}K"
    return str(w)


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("run_ids", nargs="*", default=["latest"],
                    help="run-id 1개(단일 그림) 또는 여러개(비교 그림). 첫번째가 기준")
    ap.add_argument("--windows", type=lambda s: [int(x) for x in s.split(",")],
                    default=DEFAULT_WINDOWS, help="access-count window (기본 100,1000,10000,100000)")
    ap.add_argument("--compare-window", type=int, default=100,
                    help="비교 그림에 쓸 단일 window (기본 100)")
    ap.add_argument("--hot-tiers", type=lambda s: frozenset(x.strip() for x in s.split(",")),
                    default=DEFAULT_HOT_TIERS,
                    help="hot 으로 칠 tier (기본 Hot,Global). 예: Warm,Hot,Global")
    ap.add_argument("--footprint-clamp", action="store_true",
                    help="pctable 용량 clamp 적용 (기본 OFF)")
    args = ap.parse_args()

    run_ids = args.run_ids if args.run_ids else ["latest"]

    frames: dict[str, pd.DataFrame] = {}
    for rid in run_ids:
        config, df = run_one(rid, args.windows, args.hot_tiers, args.footprint_clamp)
        if df is None:
            print(f"[{rid}] no usable benchmarks — skip", file=sys.stderr)
            continue
        out = domain_out_dir("cacheline", "premature-eviction", rid)
        df.to_csv(out / "premature.csv")
        plot_stack(df, args.windows, out, config)
        frames[f"{config}/{run_tag(rid)}"] = df
        print(f"\n[{config}] wrote {out}/premature_stack.png")
        print(df[[c for c in df.columns if c.startswith("premature_frac")
                  or c == "total_hot_evictions"]].to_string())

    if not frames:
        sys.exit(1)
    if len(frames) > 1:
        cmp_out = base / "compare"
        cmp_out.mkdir(parents=True, exist_ok=True)
        plot_compare(frames, args.compare_window, cmp_out)
        print(f"\nwrote {cmp_out}/premature_compare.png")


if __name__ == "__main__":
    main()
