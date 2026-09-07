"""pc_line_scatter_bw 에 top-PC 별 convex hull overlay 를 추가한 변형.

pc_line_scatter_bw 는 색+마커 모양으로 PC 를 구분하지만, 점이 많아지면 밀도 때문에
"이 PC 가 (reuse, sharing) 평면에서 어디까지 퍼져 있는지" 경계가 눈으로 잘 안 잡힌다.
여기선 top-PC 마다 자기 라인들의 convex hull(최소 외곽 다각형)을 반투명 색으로
덮어 그려서 각 PC 의 "영역"을 명시적 경계로 보여준다 — 점 산포는 그대로 유지.

주의:
  - hull 은 outlier 하나에도 크게 부풀 수 있다(convex hull 특성). 라인 하나가 멀리
    떨어져 있으면 그 방향으로 hull 이 길게 늘어나 다른 PC 영역과 겹쳐 보일 수 있음
    — 이건 실제 데이터에 그런 outlier 라인이 있다는 뜻이지 버그가 아니다.
  - hull 은 항상 build() 의 전수 데이터로 계산한다(표본 상한 --cap-pc 는 점 밀도만
    낮추는 시각화 전용이라 hull 경계에는 영향 없음 — pc_line_scatter_bw 의 sample
    옵션과 동일 원칙).
  - 유일 (x,y) 좌표가 3개 미만이거나 완전히 일직선(collinear)이면 QhullError 로
    hull 을 못 만든다 — 그런 PC 는 조용히 skip(하나만 로그).

그리기/데이터 로직은 pc_line_scatter_bw 를 그대로 재사용(중복 없음) — 이 파일은
_draw_hulls() 하나만 얹는다.

입력:  memory-traces/<bench>_<config>_memory_trace.csv
출력:  _out/pc-stats/pc-line-scatter-bw-hull/<run_tag>/<mode>/
         <bench>_line_scatter_bw_hull_<mode>.png + grid_bw_hull_<mode>.png

사용:  analyze/run.sh pc-stats/pc_line_scatter_bw_hull.py <run-id> [--access all|rdma|l2miss-rdma]
"""

from __future__ import annotations

import argparse
import math
import sys
from concurrent.futures import ProcessPoolExecutor
from itertools import repeat

import numpy as np
import matplotlib.pyplot as plt
from matplotlib.patches import Polygon
from scipy.spatial import ConvexHull, QhullError

from _paths import memory_trace_dir, run_config, domain_out_dir
from motivation_common import file_suffix_for_config
from plot_style import apply_paper_style, save_fig, SIZE_SINGLE
from pc_hotness import MeshGeom, mesh_dims_from_config
from pc_line_scatter_bw import (
    _scatter, _legend_handles, build, PALETTE, ACCESS_MODES,
)
from params import benchmarks


def _draw_hulls(ax, data, alpha=0.12, min_points=3, linewidth=1.0):
    """top-PC 마다 (log2 access-count, sharing) 평면에서 convex hull 을 반투명
    다각형으로 덮어 그린다.  _scatter() 가 그린 점 위에 얹는 용도라 zorder=2
    (other 구름 zorder=1 보다 위, top-PC 점 zorder=3+ 보다 아래)."""
    top = data["top"]
    lx = np.log2(np.clip(data["x"], 1, None))
    y, dom = data["y"], data["dom"]

    n_skipped = 0
    for i, pc in enumerate(top):
        sel = dom == pc
        pts = np.unique(np.column_stack([lx[sel], y[sel]]), axis=0)
        if len(pts) < min_points:
            n_skipped += 1
            continue
        try:
            hull = ConvexHull(pts)
        except QhullError:
            n_skipped += 1
            continue
        color = PALETTE[i % len(PALETTE)]
        poly = Polygon(pts[hull.vertices], closed=True, facecolor=color,
                       edgecolor=color, alpha=alpha, linewidth=linewidth,
                       linestyle="--", zorder=2)
        ax.add_patch(poly)
    if n_skipped:
        print(f"    [hull] skipped {n_skipped}/{len(top)} top-PC "
              f"(< {min_points} unique points or collinear)", flush=True)


def plot_individual(bench, data, geom, out, mode, xmax=None,
                     sample=True, cap_pc=250, cap_other=1500,
                     hull_alpha=0.4, hull_min_points=3, hull_linewidth=1.0):
    apply_paper_style()
    rng = np.random.default_rng(0)
    fig, ax = plt.subplots(figsize=(SIZE_SINGLE[0] * 2.2, SIZE_SINGLE[1] * 2.2))
    _scatter(ax, data, geom, rng, xmax=xmax, sample=sample, cap_pc=cap_pc, cap_other=cap_other,
             title=f"{bench}  [{mode}]  ({data['n_pcs']} PCs, {len(data['x'])} lines)")
    _draw_hulls(ax, data, alpha=hull_alpha, min_points=hull_min_points, linewidth=hull_linewidth)
    ax.legend(handles=_legend_handles(data), frameon=False, fontsize=6,
              ncol=1, loc="center left", bbox_to_anchor=(1.01, 0.5),
              handletextpad=0.2, labelspacing=1.0)
    save_fig(fig, out, f"{bench}_line_scatter_bw_hull_{mode}", formats=("png",))


def plot_grid(results, geom, out, mode, xmax=None, sample=True, cap_pc=250, cap_other=1500,
             hull_alpha=0.12, hull_min_points=3, hull_linewidth=1.0):
    apply_paper_style()
    rng = np.random.default_rng(0)
    n = len(results)
    ncols = math.ceil(math.sqrt(n))
    nrows = math.ceil(n / ncols)
    fig, axes = plt.subplots(nrows, ncols,
                             figsize=(3.4 * ncols, 3.4 * nrows), squeeze=False)
    for idx, (bench, data) in enumerate(results):
        ax = axes[idx // ncols][idx % ncols]
        _scatter(ax, data, geom, rng, title=bench, xmax=xmax,
                 sample=sample, cap_pc=cap_pc, cap_other=cap_other)
        _draw_hulls(ax, data, alpha=hull_alpha, min_points=hull_min_points, linewidth=hull_linewidth)
        if idx % ncols != 0:
            ax.set_ylabel("")
        if idx + ncols < n:
            ax.set_xlabel("")
    for idx in range(n, nrows * ncols):
        axes[idx // ncols][idx % ncols].axis("off")
    save_fig(fig, out, f"grid_bw_hull_{mode}", formats=("png",))


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("run_id", nargs="?", default="latest")
    ap.add_argument("--access", choices=ACCESS_MODES, default="all",
                    help="접근 필터: all=모든 demand / rdma=remote demand / "
                         "l2miss-rdma=remote L2미스(실제 RDMA 트랜잭션)")
    ap.add_argument("--xmax", type=float, default=None,
                    help="x축 상한을 2^N 에서 자름. 기본=데이터에 맞춤")
    ap.add_argument("--benches", type=str, default=None,
                    help="추가 subset grid(grid_bw_hull_<mode>_subset.png) 에 넣을 "
                         "벤치마크 콤마 목록")
    ap.add_argument("--sampling", type=str, choices=("true", "false"), default="false",
                    help="true=클래스별 표본 상한 적용(점 밀도만, hull 경계는 항상 전수) / false=전수 표시")
    ap.add_argument("--cap-pc", type=int, default=600)
    ap.add_argument("--cap-other", type=int, default=4000)
    ap.add_argument("--hull-alpha", type=float, default=0.12,
                    help="hull 채우기 투명도 (0=안 보임, 1=완전 불투명)")
    ap.add_argument("--hull-min-points", type=int, default=3,
                    help="hull 을 그릴 최소 유일 (x,y) 점 개수 (미만이면 그 PC skip)")
    ap.add_argument("--hull-linewidth", type=float, default=1.0)
    args = ap.parse_args()
    mode = args.access
    sample = args.sampling == "true"

    trace_dir = str(memory_trace_dir(args.run_id))
    config = run_config(args.run_id)
    suffix = file_suffix_for_config(config)
    n_side, num = mesh_dims_from_config(config)
    geom = MeshGeom(n_side=n_side, num=num)
    out = domain_out_dir("pc-stats", "pc-line-scatter-bw-hull", args.run_id) / mode
    out.mkdir(parents=True, exist_ok=True)

    with ProcessPoolExecutor() as ex:
        raw = list(ex.map(build, benchmarks, repeat(trace_dir),
                          repeat(suffix), repeat(mode)))
    results = [r for r in raw if r is not None]
    if not results:
        print("no usable benchmarks.", file=sys.stderr)
        sys.exit(1)

    hull_kwargs = dict(hull_alpha=args.hull_alpha, hull_min_points=args.hull_min_points,
                       hull_linewidth=args.hull_linewidth)
    for bench, data in results:
        plot_individual(bench, data, geom, out, mode, args.xmax, sample, args.cap_pc, args.cap_other,
                        **hull_kwargs)
    plot_grid(results, geom, out, mode, args.xmax, sample, args.cap_pc, args.cap_other, **hull_kwargs)
    print(f"wrote {out}/<bench>_line_scatter_bw_hull_{mode}.{{png}} + grid_bw_hull_{mode}.png "
          f"({len(results)} benches, access={mode}, sampling={sample})")

    if args.benches:
        sel = [b.strip() for b in args.benches.split(",") if b.strip()]
        sub = [(bench, data) for bench, data in results if bench in sel]
        missing = set(sel) - {b for b, _ in sub}
        if missing:
            print(f"[subset] no usable data for: {sorted(missing)}", file=sys.stderr)
        if sub:
            sub.sort(key=lambda bd: sel.index(bd[0]))
            plot_grid(sub, geom, out, f"{mode}_subset", args.xmax, sample, args.cap_pc, args.cap_other,
                      **hull_kwargs)
            print(f"wrote {out}/grid_bw_hull_{mode}_subset.png ({len(sub)} benches)")


if __name__ == "__main__":
    main()
