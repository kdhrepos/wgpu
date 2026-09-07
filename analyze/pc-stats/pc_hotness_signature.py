"""Motivation 1 — PC 가 캐시라인 특성(hotness)을 가른다.

두 가지를 동시에 보인다:
  (A) 정렬 hotness 곡선 오버레이 — 벤치마크마다 PC 를 hotness score 내림차순으로
      정렬한 곡선을 겹쳐 그림.  머리가 뾰족하고 롱테일이 길면 "소수 PC 가 hot 을
      지배" = PC 끼리 뚜렷이 갈린다.
  (B) reuse-sharing 산포 격자 — 벤치마크마다 작은 산포도(x=reuse, y=sharing,
      점=PC, 색=tier).  우상단 hot 뭉치 vs 좌하단 cold 롱테일이 갈리는 게 직접 보임.
      "왜 reuse·sharing 축으로 분류하나"까지 정당화.

hotness / tier 정의는 `common/pc_hotness.py` 가 pctable.go 를 미러링(두 motivation
스크립트가 hot 정의를 공유).  신호는 memory trace 의 demand 접근(type 0-5)에서 PC 별
global 집계: reuse=req/unique, sharing=tuples/unique, hop_norm=avg manhattan.

입력:  memory-traces/<bench>_<config>_memory_trace.csv
출력:  _out/pc-stats/pc-hotness/<config>/
         <bench>_pc_signature.csv       (PC 별 신호 + tier + score)
         hotness_curve.png              (정렬 score 곡선 오버레이)
         reuse_sharing_grid.png         (reuse-sharing 산포 격자)
         summary.csv                    (벤치별 hot PC 수 / 지배도)

사용:  analyze/run.sh pc-stats/pc_hotness_signature.py <run-id>
"""

from __future__ import annotations

import argparse
import math
import os
import sys
from concurrent.futures import ProcessPoolExecutor
from itertools import repeat

import numpy as np
import pandas as pd
import matplotlib.pyplot as plt

from _paths import memory_trace_dir, run_config, domain_out_dir
from motivation_common import file_suffix_for_config, trace_chunks, ALL_TRACE_COLS
from plot_style import apply_paper_style, color_for, save_fig, SIZE_DOUBLE, SIZE_WIDE
from pc_hotness import (
    MeshGeom, mesh_dims_from_config, PCAccum, classify_signals,
    DEFAULT_HOT_TIERS,
)
from params import benchmarks

DEMAND_TYPES = {0, 1, 2, 3, 4, 5}
MAX_PCS_FOR_PLOT = 4000   # PC 가 이보다 많으면 접근수 상위만 (드묾)


def build_signature(bench: str, trace_dir: str, suffix: str,
                    geom: MeshGeom, footprint_clamp: bool):
    trace_file = f"{trace_dir}/{bench}_{suffix}_memory_trace.csv"
    if not os.path.exists(trace_file):
        return None
    print(f"[{bench}] classifying PCs ...", flush=True)
    try:
        acc = PCAccum(geom)
        for chunk in trace_chunks(trace_file, usecols=ALL_TRACE_COLS):
            chunk = chunk.drop_duplicates()      # full-row 완전-중복 제거
            chunk = chunk[(chunk["type"].isin(DEMAND_TYPES))
                          & (chunk["is_ptw"] == 0) & (chunk["pc"] != 0)]
            if chunk.empty:
                continue
            cl = chunk["addr"].to_numpy(dtype=np.uint64) >> 6
            acc.update(chunk["pc"].to_numpy(dtype=np.uint64), cl,
                       chunk["chiplet"].to_numpy(dtype=np.int64),
                       chunk["owner"].to_numpy(dtype=np.int64))

        sig = acc.finalize()
        if sig.empty:
            return None
        if len(sig) > MAX_PCS_FOR_PLOT:
            sig = sig.sort_values("req_count", ascending=False).head(MAX_PCS_FOR_PLOT)
        cls = classify_signals(sig, geom, footprint_clamp=footprint_clamp)
        cls = cls.sort_values("score", ascending=False)
        return bench, cls
    except Exception as e:  # noqa: BLE001
        print(f"[{bench}] error: {e}")
        return None


# ── (A) 정렬 hotness 곡선 오버레이 ───────────────────────────────────────────
def plot_hotness_curve(results: list[tuple[str, pd.DataFrame]], out) -> None:
    apply_paper_style()
    fig, ax = plt.subplots(figsize=SIZE_DOUBLE)
    for i, (bench, cls) in enumerate(results):
        s = cls["score"].to_numpy()
        s = np.sort(s)[::-1]
        x = np.arange(1, len(s) + 1) / len(s)     # PC 백분위 [0,1]
        ax.plot(x, s, color=color_for(i), label=bench, linewidth=1.1)
    ax.set_xlabel("PC percentile (by descending hotness score)")
    ax.set_ylabel("hotness score")
    ax.set_xlim(0, 1)
    ax.set_ylim(0, None)
    ax.set_title("PC hotness distribution — a few PCs dominate")
    ax.legend(frameon=False, fontsize=6, ncol=2)
    ax.grid(axis="y", alpha=0.3)
    save_fig(fig, out, "hotness_curve")


# ── (B) reuse-sharing 산포 격자 ──────────────────────────────────────────────
def plot_reuse_sharing_grid(results: list[tuple[str, pd.DataFrame]], out) -> None:
    apply_paper_style()
    n = len(results)
    ncols = min(4, n)
    nrows = math.ceil(n / ncols)
    fig, axes = plt.subplots(nrows, ncols, figsize=(SIZE_WIDE[0], 2.0 * nrows),
                             squeeze=False)
    for idx, (bench, cls) in enumerate(results):
        ax = axes[idx // ncols][idx % ncols]
        # jitter 로 정수 격자 겹침 완화
        rng = np.random.default_rng(0)
        rx = cls["reuse"].to_numpy(dtype=float) + rng.uniform(-0.15, 0.15, len(cls))
        ry = cls["sharing"].to_numpy(dtype=float) + rng.uniform(-0.15, 0.15, len(cls))
        # 색 = PC 별 고유 색 (hotness 아님) — 서로 다른 PC 를 시각적으로만 구분.
        n_pc = len(cls)
        colors = plt.cm.hsv(np.linspace(0.0, 1.0, n_pc, endpoint=False))
        colors = colors[rng.permutation(n_pc)]   # score 정렬 색 쏠림 방지
        # 접근수로 점 크기 (log)
        sz = 3 + 6 * np.log10(cls["req_count"].to_numpy(dtype=float).clip(min=1)) \
            / max(1.0, np.log10(cls["req_count"].max()))
        ax.scatter(np.clip(rx, 0.5, None), np.clip(ry, 0.5, None),
                   s=sz, c=colors, alpha=0.7, linewidths=0)
        ax.set_xscale("log")
        ax.set_yscale("log")
        ax.set_title(bench, fontsize=7)
        ax.grid(True, which="both", alpha=0.2)
        if idx % ncols == 0:
            ax.set_ylabel("sharing", fontsize=6)
        if idx + ncols >= n:                       # 아래에 패널이 없으면 = 열의 최하단
            ax.set_xlabel("reuse", fontsize=6)
    # 남는 패널 숨김
    for idx in range(n, nrows * ncols):
        axes[idx // ncols][idx % ncols].axis("off")

    # 색 = PC 구분용 → 범례 없음
    save_fig(fig, out, "reuse_sharing_grid")


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("run_id", nargs="?", default="latest",
                    help="run-id (latest, <config>, <config>/<date>/<time>, ...)")
    ap.add_argument("--footprint-clamp", action="store_true",
                    help="pctable 의 용량 clamp 도 적용 (기본 OFF=순수 reuse/sharing tier)")
    args = ap.parse_args()

    trace_dir = str(memory_trace_dir(args.run_id))
    config = run_config(args.run_id)
    suffix = file_suffix_for_config(config)
    n_side, num = mesh_dims_from_config(config)
    geom = MeshGeom(n_side=n_side, num=num)

    out = domain_out_dir("pc-stats", "pc-hotness", args.run_id)

    with ProcessPoolExecutor() as ex:
        raw = list(ex.map(build_signature, benchmarks,
                          repeat(trace_dir), repeat(suffix),
                          repeat(geom), repeat(args.footprint_clamp)))
    results = [r for r in raw if r is not None]
    if not results:
        print("no usable benchmarks.", file=sys.stderr)
        sys.exit(1)

    summary = []
    for bench, cls in results:
        cls.to_csv(out / f"{bench}_pc_signature.csv")
        hot_mask = cls["tier"].isin(set(DEFAULT_HOT_TIERS))
        # hot 이 접근을 얼마나 지배하나
        hot_acc = cls.loc[hot_mask, "req_count"].sum()
        tot_acc = cls["req_count"].sum()
        summary.append(dict(
            benchmark=bench,
            n_pcs=len(cls),
            n_hot_pcs=int(hot_mask.sum()),
            hot_pc_frac=round(float(hot_mask.mean()), 4),
            hot_access_frac=round(float(hot_acc / tot_acc) if tot_acc else 0.0, 4),
            max_score=round(float(cls["score"].max()), 4),
        ))

    sdf = pd.DataFrame(summary).set_index("benchmark")
    sdf.to_csv(out / "summary.csv")

    # score 내림차순으로 벤치 순서 고정 (그림 가독성)
    results.sort(key=lambda bc: -bc[1]["score"].max())
    plot_hotness_curve(results, out)
    plot_reuse_sharing_grid(results, out)

    print("\n" + "=" * 72)
    print(sdf.to_string())
    print("=" * 72)
    print(f"wrote {out}/{{hotness_curve,reuse_sharing_grid}}.png + per-bench csv")


if __name__ == "__main__":
    main()
