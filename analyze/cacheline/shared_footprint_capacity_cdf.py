"""공유 footprint CDF vs chiplet/cluster L2 용량선.

질문: "chiplet 단위 캐싱으론 안 되고, cluster 단위 pooling에서 되는가?" — 순수
용량 논증 (candidate 2, [[project-...-distribution]] candidate 1 의 분포 논증과 상보적).

정의:
  shared line = 2개 이상 서로 다른 chiplet 이 demand 접근(type 0-5, is_ptw==0)한
                cacheline (addr>>6).
  reuse(line) = 그 라인의 전체 chiplet 합산 demand 접근 수 — 정렬 우선순위로만 사용.

곡선 (reuse 내림차순으로 shared line 을 채워나갈 때):
  x = 누적 접근 커버리지  = Σ reuse(top-k) / Σ reuse(전체 shared)      [0..100%]
  y = 누적 footprint (MB) = k × 64B                                   [log]
  reuse 로 정렬했기 때문에 "적은 용량으로 대부분의 공유 트래픽을 담는다"는
  수확체감 곡선(convex)이 나옴 — 그래서 CDF.

용량선 (common/pc_hotness.py 의 실제 cluster tier 기하에서 유도, 하드코딩 아님):
  chiplet L2      = FOOTPRINT_L2_LINES × 64B = 4 MB
  Global (2×1)    = 2 chiplet × 4 MB = 8 MB
  Hot    (2×2)    = 4 chiplet × 4 MB = 16 MB
  Warm   (2×3)    = 6 chiplet × 4 MB = 24 MB
  곡선이 어떤 수평선을 뚫고 위로 올라가면(=그 capacity 로 못 담음) 배경색이
  초록→빨강으로 짙어짐(SEVERITY_5) — chiplet 은 못 담아도 cluster pool 안에서
  곡선이 끝나면(=색이 옅은 band 안에서 종료) 클러스터링으로 담긴다는 뜻.

출력:
  ~/mgvm/figures/shared-footprint-capacity/<config>-<run_tag>/
      shared_footprint_capacity_cdf.png, per_benchmark.csv

사용:
  analyze/run.sh cacheline/shared_footprint_capacity_cdf.py <run-id>
  analyze/run.sh cacheline/shared_footprint_capacity_cdf.py <run-id> -j 8
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
from plot_style import apply_paper_style, color_for, save_fig, SIZE_DOUBLE, SEVERITY_5
from pc_hotness import mesh_dims_from_config, cluster_dims, FOOTPRINT_L2_LINES, GLOBAL, HOT, WARM
from params import benchmarks

DEMAND_TYPES = np.array([0, 1, 2, 3, 4, 5], dtype=np.int8)
USECOLS = ALL_TRACE_COLS
LINE_BYTES = 64
PER_CHIPLET_MB = FOOTPRINT_L2_LINES * LINE_BYTES / (1024.0 ** 2)   # 4.0

FIG_ROOT = Path.home() / "mgvm" / "figures" / "shared-footprint-capacity"
FIG_FORMATS = ("png",)

Y_MIN_MB = 0.1
MAX_CURVE_POINTS = 3000


def capacity_tiers(n_side: int) -> list[tuple[str, int, float]]:
    """(label, n_chiplets, capacity_MB) — chiplet L2 + cluster pools, tier 기하 그대로."""
    tiers = [("chiplet", 1, PER_CHIPLET_MB)]
    for tier in (GLOBAL, HOT, WARM):
        wx, wy = cluster_dims(tier, n_side)
        n_chip = wx * wy if wx > 0 else 1
        tiers.append((f"{wx}×{wy}", n_chip, n_chip * PER_CHIPLET_MB))
    return tiers


# ── 라인별 (reuse, 공유 chiplet 수) 집계 ─────────────────────────────────────
def _agg_shared_lines(trace_file: str) -> pd.DataFrame:
    """chunk 별 (line,chip) 유일쌍 카운트 → 최종 concat+groupby 1회로 병합.
    반환: index=line(addr>>6), columns=[reuse, nchip]."""
    parts: list[pd.Series] = []
    for chunk in trace_chunks(trace_file, usecols=USECOLS):
        chunk = chunk.drop_duplicates()
        types = chunk["type"].to_numpy(dtype=np.int8)
        is_ptw = chunk["is_ptw"].to_numpy(dtype=np.int8)
        is_d = np.isin(types, DEMAND_TYPES) & (is_ptw == 0)
        if not is_d.any():
            continue
        cl = chunk["addr"].to_numpy(dtype=np.uint64)[is_d] >> np.uint64(6)
        chip = chunk["chiplet"].to_numpy(dtype=np.int64)[is_d]
        sub = pd.DataFrame({"line": cl, "chip": chip})
        parts.append(sub.groupby(["line", "chip"], sort=False).size())
    if not parts:
        return pd.DataFrame(columns=["reuse", "nchip"])
    edges = pd.concat(parts).groupby(level=[0, 1]).sum()   # (line,chip) 유일쌍, chunk 경계 병합
    reuse = edges.groupby(level=0).sum()
    nchip = edges.groupby(level=0).size()
    return pd.DataFrame({"reuse": reuse, "nchip": nchip})


def _downsample(x: np.ndarray, y: np.ndarray, max_points: int = MAX_CURVE_POINTS):
    n = x.size
    if n <= max_points:
        return x, y
    idx = np.unique(np.clip(
        np.round(np.geomspace(1, n, max_points)).astype(np.int64) - 1, 0, n - 1))
    return x[idx], y[idx]


def _coverage_at(x: np.ndarray, y_mb: np.ndarray, capacity_mb: float) -> float:
    """capacity_mb 안에 담기는 top-reuse shared line 들이 커버하는 접근 비율(%)."""
    idx = np.searchsorted(y_mb, capacity_mb, side="right")
    if idx <= 0:
        return 0.0
    if idx >= y_mb.size:
        return 100.0
    return float(x[idx - 1] * 100.0)


def _load_replay_lines(footprint_dir: str, bench: str, n_side: int) -> pd.DataFrame:
    """mgpusim/cmd/footprint 가 뽑은 압축 집계표를 읽는다.

    timing 트레이스는 -max-inst 로 잘려서 공유 footprint 가 과소평가된다. replay
    는 커널 주소 스트림을 host 에서 끝까지 재현하므로 잘리지 않는다. 이미 라인
    단위로 접힌 (line, reuse, nchip) 이라 _agg_shared_lines 의 집계 단계가 불필요
    하고, 반환 형식만 같으면 아래 파이프라인은 그대로 돈다."""
    path = Path(footprint_dir) / f"{bench}_{n_side}x{n_side}" / "shared_lines.csv"
    if not path.exists():
        return pd.DataFrame(columns=["reuse", "nchip"])
    df = pd.read_csv(path, dtype={"line": np.uint64,
                                  "reuse": np.uint64, "nchip": np.int16})
    return df.set_index("line")[["reuse", "nchip"]]


# ── per-benchmark worker ─────────────────────────────────────────────────────
def process_benchmark(bench: str, trace_dir: str, suffix: str, tiers,
                      footprint_dir: str = "", n_side: int = 0):
    if footprint_dir:
        source = "replay"
    else:
        trace_file = f"{trace_dir}/{bench}_{suffix}_memory_trace.csv"
        if not os.path.exists(trace_file):
            return None
        source = "trace"
    print(f"[{bench}] shared-footprint capacity CDF ({source}) ...", flush=True)
    try:
        if footprint_dir:
            agg = _load_replay_lines(footprint_dir, bench, n_side)
        else:
            agg = _agg_shared_lines(trace_file)
        if agg.empty:
            return None
        shared = agg.loc[agg["nchip"] >= 2, "reuse"]
        if shared.empty:
            print(f"[{bench}] no shared lines — skip")
            return None

        reuse_sorted = np.sort(shared.to_numpy(dtype=np.float64))[::-1]
        n = reuse_sorted.size
        cum_access = np.cumsum(reuse_sorted)
        total_access = cum_access[-1]
        x = cum_access / total_access
        y_mb = np.arange(1, n + 1, dtype=np.float64) * LINE_BYTES / (1024.0 ** 2)

        row = {
            "benchmark": bench,
            "shared_lines": int(n),
            "shared_footprint_mb": float(y_mb[-1]),
            "shared_access_events": int(total_access),
        }
        for label, _, cap in tiers:
            row[f"coverage_pct_at_{label}"] = round(_coverage_at(x, y_mb, cap), 2)
            row[f"fits_fully_at_{label}"] = bool(y_mb[-1] <= cap)

        xs, ys = _downsample(x, y_mb)
        return row, xs, ys
    except Exception as e:  # noqa: BLE001
        print(f"[{bench}] error: {e}")
        return None


def run_one(run_id: str, workers: int, footprint_dir: str = ""):
    trace_dir = str(memory_trace_dir(run_id))
    config = run_config(run_id)
    suffix = file_suffix_for_config(config)
    n_side, _ = mesh_dims_from_config(config)
    tiers = capacity_tiers(n_side)

    if workers <= 1:
        raw = [process_benchmark(b, trace_dir, suffix, tiers, footprint_dir, n_side)
               for b in benchmarks]
    else:
        with ProcessPoolExecutor(max_workers=workers) as ex:
            raw = list(ex.map(process_benchmark, benchmarks,
                              repeat(trace_dir), repeat(suffix), repeat(tiers),
                              repeat(footprint_dir), repeat(n_side)))

    rows, curves = [], []
    for r in raw:
        if r is None:
            continue
        row, xs, ys = r
        rows.append(row)
        curves.append((row["benchmark"], xs, ys))
    if not rows:
        return config, None, [], tiers
    df = pd.DataFrame(rows).sort_values("shared_footprint_mb", ascending=False).set_index("benchmark")
    curves.sort(key=lambda c: df.loc[c[0], "shared_footprint_mb"], reverse=True)
    return config, df, curves, tiers


# ── 그림 ──────────────────────────────────────────────────────────────────────
def plot_capacity_cdf(curves, out: Path, config: str, tiers) -> None:
    apply_paper_style()
    fig, ax = plt.subplots(figsize=SIZE_DOUBLE)

    caps = [c for _, _, c in tiers]
    y_max = caps[-1] * 1.6
    bounds = [Y_MIN_MB] + caps + [y_max]
    for i in range(len(bounds) - 1):
        ax.axhspan(bounds[i], bounds[i + 1],
                  color=SEVERITY_5[min(i, len(SEVERITY_5) - 1)], alpha=0.16, zorder=0)

    for i, (bench, xs, ys) in enumerate(curves):
        mask = ys >= Y_MIN_MB
        if not mask.any():
            continue
        xv, yv = xs[mask] * 100.0, ys[mask]
        ax.plot(xv, yv, color=color_for(i), lw=1.1, label=bench, zorder=3)
        ax.plot(xv[-1], yv[-1], "o", color=color_for(i), ms=2.6, zorder=4)

    for cap in caps:
        ax.axhline(cap, color="#333333", lw=0.7, ls="--", zorder=2)
    # 라벨은 선 위가 아니라 자기 band 중앙(geometric mean)에 둬서 인접 tier(16↔24MB
    # 처럼 log 상 가까운 값)끼리 안 겹치게 함.
    for i, (label, n_chip, cap) in enumerate(tiers):
        mid = (bounds[i] * bounds[i + 1]) ** 0.5
        ax.text(99, mid, f"{cap:g} MB  ({label})", ha="right", va="center",
                fontsize=6, color="#333333", zorder=5)
    top_mid = (bounds[-2] * bounds[-1]) ** 0.5
    ax.text(99, top_mid, "no pool fits", ha="right", va="center",
            fontsize=6, color="#7a1f1f", zorder=5)

    ax.set_xlim(0, 100)
    ax.set_ylim(Y_MIN_MB, y_max)
    ax.set_yscale("log")
    ax.set_xlabel("cross-chiplet accesses covered (%, hottest shared lines first)")
    ax.set_ylabel("footprint required (MB)")
    ax.set_title(f"Shared-line footprint vs. L2 capacity — {config}")
    ncol = 4 if len(curves) > 6 else 3
    ax.legend(frameon=False, ncol=ncol, fontsize=6.5, loc="upper center",
              bbox_to_anchor=(0.5, -0.18))
    ax.grid(True, which="major", axis="y", alpha=0.25)
    ax.grid(False, axis="x")
    save_fig(fig, out, "shared_footprint_capacity_cdf", formats=FIG_FORMATS)


# ── 메인 ─────────────────────────────────────────────────────────────────────
def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("run_id", nargs="?", default="mesh-6x6/latest")
    ap.add_argument("-j", "--workers", type=int, default=4,
                    help="벤치마크 병렬 워커 수 (기본 4, 1=순차)")
    ap.add_argument("--footprint-dir", default="",
                    help="mgpusim/cmd/footprint 출력 디렉토리. 주면 timing 트레이스"
                         " 대신 잘리지 않은 host replay 결과를 쓴다. run_id 는"
                         " mesh 크기·용량선·출력 경로를 위해 여전히 필요.")
    args = ap.parse_args()

    config, df, curves, tiers = run_one(args.run_id, args.workers,
                                        args.footprint_dir)
    if df is None:
        print("no usable benchmarks", file=sys.stderr)
        sys.exit(1)

    tag = run_tag(args.run_id)
    if args.footprint_dir:
        tag = f"{tag}-replay"
    out = FIG_ROOT / f"{config}-{tag}"
    out.mkdir(parents=True, exist_ok=True)
    df.to_csv(out / "per_benchmark.csv")
    plot_capacity_cdf(curves, out, config, tiers)

    print(f"\n[{config}] wrote {out}")
    cov_cols = [c for c in df.columns if c.startswith("coverage_pct_at_")]
    print(df[["shared_lines", "shared_footprint_mb"] + cov_cols].to_string())


if __name__ == "__main__":
    main()
