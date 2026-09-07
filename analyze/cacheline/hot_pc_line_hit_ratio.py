"""Hot-PC 캐시라인의 hit / miss 비율.

hot 으로 분류된 PC 가 touch 한 캐시라인(OR 규칙)만 모아, 그 라인들의 hit / miss 비율.
X = 벤치마크, Y = 실제 비율 값.  "모든 캐시라인에 대한 평균" = hot 라인마다 자기
hit rate 를 구해 라인들끼리 단순 평균(라인 평균).  참고로 접근-가중 aggregate 도 CSV.

hit / miss 정의 (memory_trace type):
  hit  = {0,1,3,4}  (read/write hit + mshr-hit; 새 메모리 요청 안 냄)
  miss = {2,5}      (read/write miss; 메모리 fetch 발생)

hot 라인이 hit 이 낮고 miss 가 높으면 = 캐시가 hot 데이터를 못 잡고 있다.

입력:  memory-traces/<bench>_<config>_memory_trace.csv
출력:  _out/cacheline/hot-pc-hitrate/<run_tag>/
         hitrate.csv, hot_pc_hit_ratio.png

사용:  analyze/run.sh cacheline/hot_pc_line_hit_ratio.py <run-id>
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

from _paths import memory_trace_dir, run_config, domain_out_dir
from motivation_common import file_suffix_for_config, trace_chunks, ALL_TRACE_COLS
from plot_style import apply_paper_style, save_fig, SIZE_WIDE
from pc_hotness import (
    MeshGeom, mesh_dims_from_config, PCAccum, classify_signals,
    hot_pcs, DEFAULT_HOT_TIERS,
)
from params import benchmarks

DEMAND_TYPES = {0, 1, 2, 3, 4, 5}
HIT_TYPES = [0, 1, 3, 4]
MISS_TYPES = [2, 5]
USECOLS = ALL_TRACE_COLS       # full-row 읽어 완전-중복 제거(subset dedup 은 reuse 붕괴)
HIT_COLOR = "#2ca02c"
MISS_COLOR = "#d73027"


def _hot_lines(trace_file: str, geom: MeshGeom, hot_tiers, footprint_clamp):
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


def _line_hit_miss(trace_file: str, hot_lines: set) -> pd.DataFrame | None:
    """hot 라인 한정으로 라인별 (hit, miss) 카운트."""
    hot_idx = pd.Index(sorted(hot_lines))
    hit_parts, miss_parts = [], []
    for chunk in trace_chunks(trace_file, usecols=USECOLS):
        chunk = chunk.drop_duplicates()          # full-row 완전-중복 제거
        line = chunk["addr"].to_numpy(dtype=np.uint64) >> 6
        typ = chunk["type"].to_numpy()
        isptw0 = (chunk["is_ptw"] == 0).to_numpy()
        inhot = pd.Index(line).isin(hot_idx)
        base = isptw0 & inhot
        hmask = base & np.isin(typ, HIT_TYPES)
        mmask = base & np.isin(typ, MISS_TYPES)
        if hmask.any():
            hit_parts.append(pd.Series(line[hmask]).value_counts())
        if mmask.any():
            miss_parts.append(pd.Series(line[mmask]).value_counts())

    def _reduce(parts):
        if not parts:
            return pd.Series(dtype="int64")
        return pd.concat(parts).groupby(level=0).sum()

    hits = _reduce(hit_parts)
    misses = _reduce(miss_parts)
    idx = hits.index.union(misses.index)
    if len(idx) == 0:
        return None
    per = pd.DataFrame(index=idx)
    per["hit"] = hits.reindex(idx).fillna(0)
    per["miss"] = misses.reindex(idx).fillna(0)
    per = per[(per["hit"] + per["miss"]) > 0]
    return per.astype(np.int64)


def process_benchmark(bench, trace_dir, suffix, geom, hot_tiers, footprint_clamp):
    trace_file = f"{trace_dir}/{bench}_{suffix}_memory_trace.csv"
    if not os.path.exists(trace_file):
        return None
    print(f"[{bench}] hot-pc hit/miss ...", flush=True)
    try:
        hot_lines = _hot_lines(trace_file, geom, hot_tiers, footprint_clamp)
        if not hot_lines:
            print(f"[{bench}] no hot lines — skip")
            return None
        per = _line_hit_miss(trace_file, hot_lines)
        if per is None or per.empty:
            return None

        rate = per["hit"] / (per["hit"] + per["miss"])
        line_mean_hit = float(rate.mean())                 # 라인 평균
        h, m = int(per["hit"].sum()), int(per["miss"].sum())
        agg_hit = h / (h + m) if (h + m) else 0.0          # 접근 가중
        return pd.DataFrame([dict(
            benchmark=bench,
            n_hot_lines=len(per),
            line_mean_hit_rate=round(line_mean_hit, 4),
            line_mean_miss_rate=round(1 - line_mean_hit, 4),
            agg_hit_rate=round(agg_hit, 4),
            agg_miss_rate=round(1 - agg_hit, 4),
        )])
    except Exception as e:  # noqa: BLE001
        print(f"[{bench}] error: {e}")
        return None


def plot_hit_ratio(df: pd.DataFrame, out) -> None:
    apply_paper_style()
    benches = list(df.index)
    x = np.arange(len(benches))
    hit = df["line_mean_hit_rate"].to_numpy()
    miss = df["line_mean_miss_rate"].to_numpy()
    fig, ax = plt.subplots(figsize=SIZE_WIDE)
    ax.bar(x, hit, 0.6, color=HIT_COLOR, label="hit", edgecolor="white", linewidth=0.3)
    ax.bar(x, miss, 0.6, bottom=hit, color=MISS_COLOR, label="miss",
           edgecolor="white", linewidth=0.3)
    for xi, hv in zip(x, hit):
        ax.text(xi, hv, f"{hv:.2f}", ha="center", va="center", fontsize=5,
                color="white")
    ax.set_xticks(x)
    ax.set_xticklabels(benches, rotation=35, ha="right", fontsize=6)
    ax.set_ylabel("hit / miss ratio  (mean over hot lines)")
    ax.set_ylim(0, 1.0)
    ax.set_title("Hit/miss ratio of hot-PC cache lines")
    ax.legend(frameon=False, ncol=2, fontsize=6, loc="lower right")
    ax.grid(axis="y", alpha=0.3)
    save_fig(fig, out, "hot_pc_hit_ratio")


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("run_id", nargs="?", default="latest")
    ap.add_argument("--hot-tiers",
                    type=lambda s: frozenset(x.strip() for x in s.split(",")),
                    default=DEFAULT_HOT_TIERS,
                    help="hot 으로 칠 tier (기본 Hot,Global)")
    ap.add_argument("--footprint-clamp", action="store_true")
    args = ap.parse_args()

    trace_dir = str(memory_trace_dir(args.run_id))
    config = run_config(args.run_id)
    suffix = file_suffix_for_config(config)
    n_side, num = mesh_dims_from_config(config)
    geom = MeshGeom(n_side=n_side, num=num)
    out = domain_out_dir("cacheline", "hot-pc-hitrate", args.run_id)
    out.mkdir(parents=True, exist_ok=True)

    with ProcessPoolExecutor() as ex:
        raw = list(ex.map(process_benchmark, benchmarks,
                          repeat(trace_dir), repeat(suffix), repeat(geom),
                          repeat(args.hot_tiers), repeat(args.footprint_clamp)))
    parts = [r for r in raw if r is not None]
    if not parts:
        print("no usable benchmarks.", file=sys.stderr)
        sys.exit(1)

    df = pd.concat(parts, ignore_index=True).set_index("benchmark")
    df.to_csv(out / "hitrate.csv")
    plot_hit_ratio(df, out)

    print("\n" + "=" * 72)
    print(df.to_string())
    print("=" * 72)
    print(f"wrote {out}/hot_pc_hit_ratio.png + hitrate.csv")


if __name__ == "__main__":
    main()
