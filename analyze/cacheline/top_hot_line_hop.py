"""벤치마크별 top-N% hot cacheline 의 평균 hop count (hit=0, miss=manhattan).

정의
----
  hot line       = **전체 demand 접근수(hit+miss)** 기준 상위 N% cacheline
                   (chip 무관 순수 line 랭킹)
  demand event   = type ∈ {0..5}, is_ptw==0                       (랭킹용 + hop 집계용)
  miss event     = type ∈ {2, 5}                                  (hop > 0 대상)
  hit event      = demand \\ miss = {0, 1, 3, 4}                   (hop = 0 강제)
  hop            = miss → |chip_x - owner_x| + |chip_y - owner_y| (mesh manhattan)
                   hit  → 0
  per-line avg   = Σ hop / demand 접근수                     (라인 하나당 평균 hop)
  per-bench avg  = Σ per-line avg / (hot line 개수)          (라인 평균의 평균)

파이프라인 (1-pass):
  chunked read → drop_duplicates → demand 이벤트만 필터
    → line 별 (demand_count, hop_sum, hop_event_count) 동시 누적
      · demand_count     = 모든 demand 이벤트 수         (랭킹용)
      · hop_sum          = miss+rdma 이벤트의 hop 합계   (numerator)
      · hop_event_count  = miss+rdma 이벤트 수           (denominator, include_hit=False 때)
    → demand_count 로 정렬 → 상위 N% 컷
    → include_hit=True  : avg = hop_sum / demand_count
       include_hit=False: avg = hop_sum / hop_event_count (0 인 라인 제외)
    → 라인별 평균의 평균 = 벤치 평균

이전판 2-pass 는 파일 I/O 를 두 번 하고 drop_duplicates 를 두 번 걸어 느렸음.
1-pass 로 통합해도 line 별 dict 는 O(unique lines) 로 동일 (bounded).

사용:
  analyze/run.sh cacheline/top_hot_line_hop.py <run-id>
  analyze/run.sh cacheline/top_hot_line_hop.py <run-id> --top-pct 10 -j 8
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

from _paths import memory_trace_dir, run_config, domain_out_dir
from motivation_common import file_suffix_for_config, trace_chunks, ALL_TRACE_COLS
from plot_style import apply_paper_style, save_fig, SIZE_WIDE
from pc_hotness import mesh_dims_from_config
from params import benchmarks

DEMAND_TYPES = np.array([0, 1, 2, 3, 4, 5], dtype=np.int8)
MISS_TYPES   = np.array([2, 5], dtype=np.int8)
USECOLS = ALL_TRACE_COLS
FIG_FORMATS = ("png",)
BAR_COLOR = "#0072B2"


def _agg_one_pass(trace_file: str, n_side: int) -> pd.DataFrame:
    """1-pass — line 별 (demand_count, hop_sum, hop_event_count) 동시 누적.
    demand event   = type ∈ {0..5}, is_ptw==0
    hop_event      = demand ∧ type ∈ {2,5} ∧ rdma == 1     (실제 RDMA)
    hop_sum        = hop_event 의 manhattan hop 합계."""
    n = np.int64(n_side)
    parts: list[pd.DataFrame] = []
    for chunk in trace_chunks(trace_file, usecols=USECOLS):
        chunk = chunk.drop_duplicates()
        types = chunk["type"].to_numpy(dtype=np.int8)
        is_ptw = chunk["is_ptw"].to_numpy(dtype=np.int8)
        rdma = chunk["rdma"].to_numpy(dtype=np.int8)
        is_d = np.isin(types, DEMAND_TYPES) & (is_ptw == 0)
        if not is_d.any():
            continue
        cl = chunk["addr"].to_numpy(dtype=np.uint64)[is_d] >> np.uint64(6)
        chip = chunk["chiplet"].to_numpy(dtype=np.int64)[is_d]
        owner = chunk["owner"].to_numpy(dtype=np.int64)[is_d]
        is_hop_ev = np.isin(types[is_d], MISS_TYPES) & (rdma[is_d] == 1) \
                    & (chip >= 0) & (owner >= 0)
        sx, sy = chip % n, chip // n
        dx, dy = owner % n, owner // n
        hop = np.where(is_hop_ev,
                       np.abs(sx - dx) + np.abs(sy - dy), 0).astype(np.int64)
        sub = pd.DataFrame({
            "line":  cl,
            "hop":   hop,
            "hopev": is_hop_ev.astype(np.int64),
        })
        g = sub.groupby("line", sort=False).agg(
            demand_count=("hop", "size"),
            hop_sum=("hop", "sum"),
            hop_event_count=("hopev", "sum"),
        )
        parts.append(g)
    if not parts:
        return pd.DataFrame(columns=["demand_count", "hop_sum", "hop_event_count"])
    return pd.concat(parts).groupby(level=0).sum()


def process_benchmark(bench: str, trace_dir: str, suffix: str,
                      top_pct: float, n_side: int, include_hit: bool):
    trace_file = f"{trace_dir}/{bench}_{suffix}_memory_trace.csv"
    if not os.path.exists(trace_file):
        return None
    print(f"[{bench}] top-{top_pct:g}% hot-line hop (1-pass) ...", flush=True)
    try:
        df = _agg_one_pass(trace_file, n_side)
        if df.empty:
            return None
        n_lines = int(len(df))
        n_hot = max(1, int(n_lines * top_pct / 100))
        top = df.nlargest(n_hot, "demand_count")

        num = top["hop_sum"].to_numpy(dtype=np.float64)
        if include_hit:
            denom = top["demand_count"].to_numpy(dtype=np.float64)
        else:
            denom = top["hop_event_count"].to_numpy(dtype=np.float64)
        mask = denom > 0
        per_line_avg = num[mask] / denom[mask]
        if per_line_avg.size == 0:
            return None
        return {
            "benchmark":              bench,
            "lines_total":            n_lines,
            "lines_selected":         n_hot,
            "lines_observed":         int(mask.sum()),
            "demand_events_top":      int(top["demand_count"].sum()),
            "hop_events_top":         int(top["hop_event_count"].sum()),
            "hop_sum_top":            int(top["hop_sum"].sum()),
            "avg_hop_per_line":       float(per_line_avg.mean()),
            "avg_hop_per_line_med":   float(np.median(per_line_avg)),
            "avg_hop_per_line_min":   float(per_line_avg.min()),
            "avg_hop_per_line_max":   float(per_line_avg.max()),
        }
    except Exception as e:  # noqa: BLE001
        print(f"[{bench}] error: {e}")
        return None


def plot_bench_avg(df: pd.DataFrame, out: Path, config: str, top_pct: float):
    apply_paper_style()
    benches = list(df.index)
    n = len(benches)
    y = df["avg_hop_per_line"].to_numpy(dtype=np.float64)

    fig, ax = plt.subplots(figsize=SIZE_WIDE)
    x0 = np.arange(n)
    ax.bar(x0, y, 0.7, color=BAR_COLOR, edgecolor="white", linewidth=0.4)

    ypad = (y.max() * 0.01) if y.max() > 0 else 0.02
    for x, v in zip(x0, y):
        ax.text(x, v + ypad, f"{v:.2f}", ha="center", va="bottom",
                fontsize=6, color="#333333")

    ax.set_xticks(x0)
    ax.set_xticklabels(benches, rotation=30, ha="right", fontsize=7)
    ax.set_ylabel("avg hop per line  (hit=0, miss=manhattan)")
    ax.set_title(f"Per-benchmark avg hop of top {top_pct:g}% hot cachelines "
                 f"— {config}  (hit counted as hop=0)")
    ax.grid(axis="y", alpha=0.35)
    ax.set_ylim(0, (y.max() * 1.15) if y.max() > 0 else 1.0)
    save_fig(fig, out, f"top{int(top_pct)}p_hot_line_hop", formats=FIG_FORMATS)


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("run_id", nargs="?", default="latest")
    ap.add_argument("-j", "--workers", type=int, default=4,
                    help="벤치마크 병렬 워커 수 (기본 4, 1=순차)")
    ap.add_argument("--top-pct", type=float, default=10.0,
                    help="전체 demand 접근수 기준 상위 N%% cacheline (기본 10)")
    ap.add_argument("--include-hit", action="store_true",
                    help="hit 도 포함(hop=0 처리). 기본 = 실제 RDMA(miss+rdma) 만.")
    args = ap.parse_args()

    if not (0.0 < args.top_pct <= 100.0):
        ap.error("--top-pct must be in (0, 100]")

    trace_dir = str(memory_trace_dir(args.run_id))
    config = run_config(args.run_id)
    suffix = file_suffix_for_config(config)
    n_side, _ = mesh_dims_from_config(config)

    if args.workers <= 1:
        raw = [process_benchmark(b, trace_dir, suffix, args.top_pct, n_side,
                                 args.include_hit)
               for b in benchmarks]
    else:
        with ProcessPoolExecutor(max_workers=args.workers) as ex:
            raw = list(ex.map(process_benchmark, benchmarks,
                              repeat(trace_dir), repeat(suffix),
                              repeat(args.top_pct), repeat(n_side),
                              repeat(args.include_hit)))
    rows = [r for r in raw if r is not None]
    if not rows:
        print("no usable benchmarks", file=sys.stderr)
        sys.exit(1)

    df = pd.DataFrame(rows).set_index("benchmark")
    out = domain_out_dir("cacheline", "top-hot-line-hop", args.run_id)
    df.to_csv(out / "per_benchmark.csv")
    plot_bench_avg(df, out, config, args.top_pct)
    print(f"\n[{config}] wrote {out}")
    print(df.to_string())


if __name__ == "__main__":
    main()
