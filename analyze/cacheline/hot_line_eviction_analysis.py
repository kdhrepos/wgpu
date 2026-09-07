"""Hot-PC 캐시라인 eviction 분석 (Motivation 지표 2종).

질문:
  1) Hot PC 가 만든 캐시라인 중 **evict 되는 비율** — 얼마나 많이 쫓겨나나?
  2) evict 된 hot 라인의 **re-reference interval** — 얼마나 빨리 다시 접근되나?

정의:
  hot 라인   = 하나 이상의 hot 분류 PC 가 touch 한 캐시라인 (OR 규칙, common/pc_hotness).
               tier ∈ {Hot, Global} 이 기본 (DEFAULT_HOT_TIERS).
  created    = trace 에 등장한 서로 다른 (chip, hot 라인) 쌍 수 = hot 라인 install 총량.
  evicted    = 그중 victim(type 8/9)으로 한 번이라도 등장한 (chip, 라인) 쌍 수.
  eviction   = 발생 이벤트(type 8/9) 총 수 vs demand 이벤트(type 0-5) 총 수.
  clock      = **칩렛별 demand 접근 카운트** — window 단위, 벤치 간 공정 비교(pctable.go
               windowed WS 개념과 동형).  cycle 단위는 벤치 규모에 종속 → 사용 안함.
  gap        = victim 축출 시점 clock 부터 같은 (chip, 라인) 의 다음 demand clock 까지 차이.
               1 이 최소.  없으면 열림 상태로 남긴다(=제외).

계산 (2-pass streaming, memory bounded):
  Pass 1  ── PCAccum + classify_signals → hot PC 집합 → hot 라인 집합 (OR).
  Pass 2  ── 청크마다 (a) chiplet 별 누적 demand clock 갱신, (b) hot 라인 한정 demand+victim
             이벤트 (chip, line, clock, is_victim) 수집, (c) install-uniq set 갱신.
             종료 후 (chip, line) 별 이벤트 정렬 → victim → next demand gap 을
             merge_asof(direction='forward', allow_exact_matches=False) 로 뽑는다.

지표 요약:
  line_evict_ratio  = |evicted (chip,line)| / |created (chip,line)|         [0..1]
  event_evict_ratio = |victim events|       / |demand events on hot lines| [0..1]
  interval CDF, mean, median, p90 (access-count 단위).

그림 (논문 300dpi, PNG+PDF):
  Fig-A `hot_line_eviction_ratio` — 벤치별 이중 막대(line, event 비율)
  Fig-B `hot_line_reref_interval_cdf` — 벤치별 CDF (log-x, 세로 = 재요청 누적 확률)
        위 축은 각 벤치의 p50/p90 을 그리드 참조선으로 표시
  Fig-C `hot_line_reref_summary` — 벤치별 mean/median/p90 3-column stat bar (log-y)

출력:
  ~/mgvm/figures/hot-line-eviction/<config>-<run_tag>/
      <fig>.png, <fig>.pdf, per_benchmark.csv, per_interval.csv (compressed)

사용:
  analyze/run.sh cacheline/hot_line_eviction_analysis.py <run-id>
  analyze/run.sh cacheline/hot_line_eviction_analysis.py <run-id> \\
      --hot-tiers Warm,Hot,Global --footprint-clamp
  analyze/run.sh cacheline/hot_line_eviction_analysis.py <run-id-a> <run-id-b> \\
      --compare                                     # 플랫폼 비교 그림 추가
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
import matplotlib.pyplot as plt

from _paths import memory_trace_dir, run_config, run_tag
from motivation_common import file_suffix_for_config, trace_chunks, ALL_TRACE_COLS
from plot_style import apply_paper_style, color_for, save_fig, SIZE_WIDE, SIZE_DOUBLE
from pc_hotness import (
    MeshGeom, mesh_dims_from_config, PCAccum, classify_signals,
    hot_pcs, DEFAULT_HOT_TIERS,
)
from params import benchmarks

DEMAND_TYPES = {0, 1, 2, 3, 4, 5}
VICTIM_TYPES = {8, 9}

USECOLS = ALL_TRACE_COLS       # full-row 읽어 완전-중복 제거 (subset dedup 은 reuse 붕괴)

RATIO_COLOR_LINE  = "#0072B2"  # 파랑 = 라인 단위 비율
RATIO_COLOR_EVENT = "#D55E00"  # 주황 = 이벤트 단위 비율

FIG_ROOT = Path.home() / "mgvm" / "figures" / "hot-line-eviction"
FIG_FORMATS = ("png",)


# ── Pass 1 : hot 라인 집합 ────────────────────────────────────────────────────
def _hot_lines(trace_file: str, geom: MeshGeom, hot_tiers,
               footprint_clamp: bool) -> set:
    acc = PCAccum(geom)
    for chunk in trace_chunks(trace_file, usecols=USECOLS):
        chunk = chunk.drop_duplicates()
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


# ── Pass 2 : hot 라인 event 수집 + gap 계산 ──────────────────────────────────
def _collect_events(trace_file: str, hot_lines: set) -> dict:
    """chiplet clock 을 유지하며 hot 라인 demand/victim 이벤트만 축적."""
    base: dict[int, int] = {}
    dem_parts: list[pd.DataFrame] = []
    vic_parts: list[pd.DataFrame] = []
    hot_arr = pd.Index(sorted(hot_lines))

    created_pairs: set[tuple[int, int]] = set()   # (chip, cl) — install 유일 쌍
    total_hot_demand = 0
    total_hot_victim = 0

    for chunk in trace_chunks(trace_file, usecols=USECOLS):
        chunk = chunk.drop_duplicates()
        cl = chunk["addr"].to_numpy(dtype=np.uint64) >> 6
        dm = ((chunk["type"].isin(DEMAND_TYPES)) & (chunk["is_ptw"] == 0)).to_numpy()
        vm = chunk["type"].isin(VICTIM_TYPES).to_numpy()
        chip = chunk["chiplet"].to_numpy(dtype=np.int64)

        tmp = pd.DataFrame({"chip": chip, "dm": dm.astype(np.int64)})
        cum = tmp.groupby("chip")["dm"].cumsum().to_numpy(dtype=np.int64)
        # map→NaN→fillna 로 float 승격 방지: int64 캐스팅.  키 dtype 일관 보장.
        base_off = tmp["chip"].map(base).fillna(0).to_numpy(dtype=np.int64)
        clock = base_off + cum          # int64

        hot_mask = (pd.Index(cl).isin(hot_arr) if len(hot_arr)
                    else np.zeros(len(cl), bool))
        d_sel = dm & hot_mask
        v_sel = vm & hot_mask

        if d_sel.any():
            sub = pd.DataFrame({
                "chip":  chip[d_sel].astype(np.int64, copy=False),
                "cl":    cl[d_sel].astype(np.int64, copy=False),
                "clock": clock[d_sel].astype(np.int64, copy=False),
            })
            dem_parts.append(sub)
            total_hot_demand += int(d_sel.sum())
            created_pairs.update(zip(sub["chip"].to_numpy(),
                                     sub["cl"].to_numpy()))
        if v_sel.any():
            vic_parts.append(pd.DataFrame({
                "chip":  chip[v_sel].astype(np.int64, copy=False),
                "cl":    cl[v_sel].astype(np.int64, copy=False),
                "clock": clock[v_sel].astype(np.int64, copy=False),
            }))
            total_hot_victim += int(v_sel.sum())

        tot = tmp.groupby("chip")["dm"].sum()
        for c, v in tot.items():
            base[c] = base.get(c, 0) + int(v)

    if not dem_parts:
        return {"gaps": np.array([], dtype=float), "created": 0, "evicted": 0,
                "demand_events": 0, "victim_events": 0}
    demand = pd.concat(dem_parts, ignore_index=True)
    demand["next_clock"] = demand["clock"]
    demand = demand.sort_values("clock", kind="stable")

    evicted_pairs: set[tuple[int, int]] = set()
    gaps = np.array([], dtype=float)
    if vic_parts:
        victim = pd.concat(vic_parts, ignore_index=True)
        evicted_pairs.update(zip(victim["chip"].to_numpy(),
                                 victim["cl"].to_numpy()))
        victim = victim.sort_values("clock", kind="stable")
        m = pd.merge_asof(victim, demand[["clock", "chip", "cl", "next_clock"]],
                          on="clock", by=["chip", "cl"],
                          direction="forward", allow_exact_matches=False)
        gaps = (m["next_clock"] - m["clock"]).to_numpy(dtype=float)
        gaps = gaps[np.isfinite(gaps)]

    return {
        "gaps": gaps,
        "created": len(created_pairs),
        "evicted": len(evicted_pairs & created_pairs),
        "demand_events": total_hot_demand,
        "victim_events": total_hot_victim,
    }


# ── per-benchmark worker ─────────────────────────────────────────────────────
def process_benchmark(bench: str, trace_dir: str, suffix: str, geom: MeshGeom,
                      hot_tiers, footprint_clamp: bool):
    trace_file = f"{trace_dir}/{bench}_{suffix}_memory_trace.csv"
    if not os.path.exists(trace_file):
        return None
    print(f"[{bench}] hot-line eviction ...", flush=True)
    try:
        hot_lines = _hot_lines(trace_file, geom, hot_tiers, footprint_clamp)
        if not hot_lines:
            print(f"[{bench}] no hot lines — skip")
            return None
        stats = _collect_events(trace_file, hot_lines)
        gaps = stats["gaps"]
        row = {
            "benchmark": bench,
            "n_hot_lines": len(hot_lines),
            "created_pairs": stats["created"],
            "evicted_pairs": stats["evicted"],
            "demand_events": stats["demand_events"],
            "victim_events": stats["victim_events"],
            "line_evict_ratio":  (stats["evicted"] / stats["created"]
                                  if stats["created"] else 0.0),
            "event_evict_ratio": (stats["victim_events"]
                                  / max(1, stats["demand_events"])),
            "n_gaps": int(gaps.size),
            "gap_mean":   float(np.mean(gaps))   if gaps.size else np.nan,
            "gap_median": float(np.median(gaps)) if gaps.size else np.nan,
            "gap_p90":    float(np.percentile(gaps, 90)) if gaps.size else np.nan,
            "gap_p99":    float(np.percentile(gaps, 99)) if gaps.size else np.nan,
        }
        return row, gaps
    except Exception as e:  # noqa: BLE001
        print(f"[{bench}] error: {e}")
        return None


# ── 단일 run 처리 ────────────────────────────────────────────────────────────
def run_one(run_id: str, hot_tiers, footprint_clamp: bool, workers: int):
    trace_dir = str(memory_trace_dir(run_id))
    config = run_config(run_id)
    suffix = file_suffix_for_config(config)
    n_side, num = mesh_dims_from_config(config)
    geom = MeshGeom(n_side=n_side, num=num)

    if workers <= 1:
        raw = [process_benchmark(b, trace_dir, suffix, geom, hot_tiers,
                                 footprint_clamp) for b in benchmarks]
    else:
        with ProcessPoolExecutor(max_workers=workers) as ex:
            raw = list(ex.map(process_benchmark, benchmarks,
                              repeat(trace_dir), repeat(suffix), repeat(geom),
                              repeat(hot_tiers), repeat(footprint_clamp)))

    rows: list[dict] = []
    gap_map: dict[str, np.ndarray] = {}
    for r in raw:
        if r is None:
            continue
        row, gaps = r
        rows.append(row)
        gap_map[row["benchmark"]] = gaps
    if not rows:
        return config, None, {}
    df = pd.DataFrame(rows).set_index("benchmark")
    return config, df, gap_map


# ── 그림 A: 벤치별 이중 막대 (line vs event eviction ratio) ──────────────────
def plot_ratio_bars(df: pd.DataFrame, out: Path, config: str) -> None:
    apply_paper_style()
    benches = list(df.index)
    n = len(benches)
    fig, ax = plt.subplots(figsize=SIZE_WIDE)

    group_w = 0.8
    bar_w = group_w / 2
    x0 = np.arange(n)
    line_vals  = df["line_evict_ratio"].to_numpy()
    event_vals = df["event_evict_ratio"].to_numpy()

    ax.bar(x0 - bar_w / 2, line_vals, bar_w, color=RATIO_COLOR_LINE,
           label="line eviction ratio  = evicted lines / created lines",
           edgecolor="white", linewidth=0.4)
    ax.bar(x0 + bar_w / 2, event_vals, bar_w, color=RATIO_COLOR_EVENT,
           label="event eviction ratio = victim events / demand events",
           edgecolor="white", linewidth=0.4)

    for x, v in zip(x0 - bar_w / 2, line_vals):
        ax.text(x, v + 0.01, f"{v:.2f}", ha="center", va="bottom",
                fontsize=5.5, color="#333333")
    for x, v in zip(x0 + bar_w / 2, event_vals):
        ax.text(x, v + 0.01, f"{v:.2f}", ha="center", va="bottom",
                fontsize=5.5, color="#333333")

    ax.set_xticks(x0)
    ax.set_xticklabels(benches, rotation=30, ha="right", fontsize=7)
    ax.set_ylabel("eviction ratio")
    ymax = float(max(1.0, np.nanmax(np.r_[line_vals, event_vals]) * 1.15))
    ax.set_ylim(0, ymax)
    ax.set_title(f"Hot-PC cache-line eviction — {config}")
    ax.legend(frameon=False, ncol=1, fontsize=6.5, loc="upper right")
    ax.grid(axis="y", alpha=0.35)
    save_fig(fig, out, "hot_line_eviction_ratio", formats=FIG_FORMATS)


# ── 그림 B: 벤치별 re-reference interval CDF ─────────────────────────────────
def plot_reref_cdf(gap_map: dict[str, np.ndarray], df: pd.DataFrame,
                   out: Path, config: str) -> None:
    apply_paper_style()
    fig, ax = plt.subplots(figsize=SIZE_DOUBLE)

    plotted = 0
    for i, bench in enumerate(df.index):
        g = gap_map.get(bench, np.array([], dtype=float))
        if g.size == 0:
            continue
        g = np.sort(g[g > 0])
        if g.size == 0:
            continue
        y = np.arange(1, g.size + 1) / g.size
        ax.plot(g, y, color=color_for(i), lw=1.2, label=bench)
        plotted += 1

    if plotted == 0:
        plt.close(fig)
        return

    ax.set_xscale("log")
    ax.set_xlabel("re-reference interval  (chiplet-local access-count units)")
    ax.set_ylabel("cumulative fraction of evicted hot lines")
    ax.set_ylim(0, 1.02)
    ax.set_yticks(np.arange(0, 1.01, 0.25))
    ax.axhline(0.5,  color="#888888", lw=0.5, ls=":", zorder=0)
    ax.axhline(0.9,  color="#888888", lw=0.5, ls=":", zorder=0)
    ax.set_title(f"Re-reference interval of evicted hot lines — {config}")
    ncol = 2 if plotted <= 6 else 3
    ax.legend(frameon=False, ncol=ncol, fontsize=6, loc="lower right")
    ax.grid(True, which="both", alpha=0.3)
    save_fig(fig, out, "hot_line_reref_interval_cdf", formats=FIG_FORMATS)


# ── 그림 C: 벤치별 mean/median/p90 요약 막대 ────────────────────────────────
def plot_reref_summary(df: pd.DataFrame, out: Path, config: str) -> None:
    apply_paper_style()
    stats_cols = ["gap_median", "gap_mean", "gap_p90"]
    labels = ["median", "mean", "p90"]
    colors = ["#0072B2", "#009E73", "#D55E00"]

    d = df.dropna(subset=stats_cols, how="all")
    if d.empty:
        return
    benches = list(d.index)
    n = len(benches)

    fig, ax = plt.subplots(figsize=SIZE_WIDE)
    group_w = 0.82
    bar_w = group_w / len(stats_cols)
    x0 = np.arange(n)
    for j, (col, lab, cc) in enumerate(zip(stats_cols, labels, colors)):
        vals = d[col].to_numpy()
        xpos = x0 - group_w / 2 + bar_w * (j + 0.5)
        ax.bar(xpos, vals, bar_w, color=cc, label=lab,
               edgecolor="white", linewidth=0.4)
    ax.set_xticks(x0)
    ax.set_xticklabels(benches, rotation=30, ha="right", fontsize=7)
    ax.set_ylabel("re-reference interval  (access-count units)")
    ax.set_yscale("log")
    ax.set_title(f"Re-reference interval statistics — {config}")
    ax.legend(frameon=False, ncol=3, fontsize=7, loc="upper right")
    ax.grid(axis="y", which="both", alpha=0.3)
    save_fig(fig, out, "hot_line_reref_summary", formats=FIG_FORMATS)


# ── 그림 D (선택): 여러 config 비교 — line/event ratio grouped ───────────────
def plot_compare(frames: dict[str, pd.DataFrame], out: Path,
                 metric: str = "line_evict_ratio") -> None:
    apply_paper_style()
    configs = list(frames.keys())
    benches = sorted(set().union(*[set(f.index) for f in frames.values()]))
    n, nc = len(benches), len(configs)
    fig, ax = plt.subplots(figsize=SIZE_WIDE)

    group_w = 0.82
    bar_w = group_w / nc
    x0 = np.arange(n)
    for j, cfg in enumerate(configs):
        f = frames[cfg]
        vals = [f.loc[b, metric] if b in f.index else np.nan for b in benches]
        xpos = x0 - group_w / 2 + bar_w * (j + 0.5)
        ax.bar(xpos, vals, bar_w, color=color_for(j), label=cfg,
               edgecolor="white", linewidth=0.4)

    ax.set_xticks(x0)
    ax.set_xticklabels(benches, rotation=30, ha="right", fontsize=7)
    ax.set_ylabel(metric.replace("_", " "))
    ax.set_ylim(0, 1.0)
    ax.set_title(f"{metric.replace('_', ' ')} — platform comparison")
    ax.legend(frameon=False, ncol=min(nc, 3), fontsize=6.5, loc="upper right")
    ax.grid(axis="y", alpha=0.3)
    save_fig(fig, out, f"compare_{metric}", formats=FIG_FORMATS)


# ── 메인 ─────────────────────────────────────────────────────────────────────
def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("run_ids", nargs="*", default=["latest"],
                    help="run-id 1개 이상. 2개 이상이고 --compare 면 비교 그림도 생성.")
    ap.add_argument("--hot-tiers", type=lambda s: frozenset(x.strip() for x in s.split(",")),
                    default=DEFAULT_HOT_TIERS,
                    help="hot tier 집합 (기본 Hot,Global). 예: Warm,Hot,Global")
    ap.add_argument("--footprint-clamp", action="store_true",
                    help="pctable 용량 clamp (기본 OFF)")
    ap.add_argument("--compare", action="store_true",
                    help="run-id 2개 이상일 때 config 비교 막대 그림 생성.")
    ap.add_argument("-j", "--workers", type=int, default=4,
                    help="벤치마크 병렬 워커 수 (기본 4, 1=순차).  각 워커가 청크 pandas "
                    "로 trace 를 스트리밍해 메모리 상한이 워커 수에 비례한다.")
    args = ap.parse_args()

    frames: dict[str, pd.DataFrame] = {}
    cmp_out: Path | None = None

    for rid in args.run_ids:
        config, df, gap_map = run_one(rid, args.hot_tiers, args.footprint_clamp,
                                      args.workers)
        if df is None:
            print(f"[{rid}] no usable benchmarks — skip", file=sys.stderr)
            continue

        out = FIG_ROOT / f"{config}-{run_tag(rid)}"
        out.mkdir(parents=True, exist_ok=True)

        df.to_csv(out / "per_benchmark.csv")
        _dump_gaps(gap_map, out / "per_interval.csv.gz")

        plot_ratio_bars(df, out, config)
        plot_reref_cdf(gap_map, df, out, config)
        plot_reref_summary(df, out, config)

        print(f"\n[{config}] wrote {out}")
        print(df[["created_pairs", "evicted_pairs", "line_evict_ratio",
                  "event_evict_ratio", "gap_median", "gap_mean",
                  "gap_p90"]].to_string())

        frames[f"{config}/{run_tag(rid)}"] = df
        cmp_out = out.parent

    if args.compare and len(frames) > 1 and cmp_out is not None:
        cmp = cmp_out / "compare"
        cmp.mkdir(parents=True, exist_ok=True)
        plot_compare(frames, cmp, metric="line_evict_ratio")
        plot_compare(frames, cmp, metric="event_evict_ratio")
        print(f"\ncompare figures -> {cmp}")


def _dump_gaps(gap_map: dict[str, np.ndarray], path: Path) -> None:
    """gap distribution 을 (benchmark, gap) long-form 으로 gz 저장."""
    if not gap_map:
        return
    parts = []
    for b, g in gap_map.items():
        if g.size == 0:
            continue
        parts.append(pd.DataFrame({"benchmark": b, "gap": g.astype(np.int64)}))
    if parts:
        pd.concat(parts, ignore_index=True).to_csv(path, index=False,
                                                    compression="gzip")


if __name__ == "__main__":
    main()
