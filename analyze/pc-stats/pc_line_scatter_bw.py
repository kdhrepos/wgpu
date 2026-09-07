"""pc_line_scatter 의 흑백(마커 모양) 변형.

pc_line_scatter 는 top-PC 를 색으로만 구분해 흑백 인쇄/색약 열람에서 구분이
안 된다.  여기선 top-PC 마다 **마커 모양도** 다르게 줘서 색 없이도 분리되게
한다(색은 컬러 열람용으로 유지, 모양이 1차 구분 신호).  평균 대표점(X 마커)은
빼고 캐시라인 산포만 그린다 — 마커 모양 자체가 이미 PC 식별자라 대표점이 불필요.

데이터 수집(build/_collect_lines)은 pc_line_scatter 와 동일해 그대로 가져다
쓴다 — 달라지는 건 그리기 뿐.

입력:  memory-traces/<bench>_<config>_memory_trace.csv
출력:  _out/pc-stats/pc-line-scatter-bw/<run_tag>/<mode>/
         <bench>_line_scatter_bw_<mode>.png + grid_bw_<mode>.png

사용:  analyze/run.sh pc-stats/pc_line_scatter_bw.py <run-id> [--access all|rdma|l2miss-rdma]
"""

from __future__ import annotations

import argparse
import math
import sys
from concurrent.futures import ProcessPoolExecutor
from itertools import repeat

import numpy as np
import matplotlib.pyplot as plt
from matplotlib.ticker import FuncFormatter

from _paths import memory_trace_dir, run_config, domain_out_dir
from motivation_common import file_suffix_for_config
from plot_style import apply_paper_style, save_fig, SIZE_SINGLE, SIZE_WIDE
from pc_hotness import MeshGeom, mesh_dims_from_config
from pc_line_scatter import build, PALETTE, OTHER_COLOR, ACCESS_MODES, MAX_PCS_COLOR
from params import benchmarks

# top-PC 별 고유 마커. 색과 별개로 모양 자체가 식별자라 흑백에서도 분리된다.
# 팔레트 색 수와 맞춰 MAX_PCS_COLOR(10) 개까지 순환 — 그러나 top PC 수가
# MAX_PCS_COLOR 를 넘지 않는 한 순환은 실제로 안 일어나(1:1), i % len 은 방어용.
# OTHER_MARKER 는 이 목록 밖의 모양이라 "색 못 받은 PC" 구름이 어떤 top-PC 와도
# 모양이 겹치지 않는다(top 목록에 재사용 금지).
MARKERS = ["s", "^", "D", "v", "P", "*", "X", "h", "<", ">"]
OTHER_MARKER = "."
assert len(MARKERS) >= MAX_PCS_COLOR, "not enough distinct markers for MAX_PCS_COLOR"

# 벤치마크 원본 키(params.benchmarks) -> 그림에 표시할 짧은 이름.
# 목록에 없는 벤치마크는 .upper() 로 대체(새 벤치마크 추가 시 그림이 깨지지 않게).
DISPLAY_NAMES = {
    "cc": "CC", "gups": "GUPS", "llama70b_ffn": "LLAMA",
    "mis": "MIS", "mixtral_ffn": "MIXT", "mst": "MST",
    "pagerank": "PR", "spmv": "SPMV", "syr2k": "SYR2K",
    "xsbench": "XSB", "bptree": "BTR", "spmm": "SPMM",
}


def _scatter(ax, data, geom: MeshGeom, rng, title=None, xmax=None,
             sample=True, cap_pc=250, cap_other=1500, show_mean=False):
    """점 = 고유 캐시라인, 색+모양 = dominant PC (모양이 흑백 1차 구분).

    top-PC 하나당 scatter 를 따로 호출해야 마커 모양을 바꿀 수 있어(matplotlib
    은 한 호출 안에서 마커를 배열로 못 받음).  실 데이터는 클래스당 수만 라인이라
    전수를 찍으면 모양이 잉크에 파묻힌다 — sample=True 면 클래스별 표본 상한
    (cap_pc/cap_other, 시각화 전용, build() 의 전수 집계는 그대로)으로 밀도를
    낮추고, 표본이 적은(=화면에서 묻히기 쉬운) 클래스를 나중에(위에) 그려 항상
    보이게 한다.  sample=False 면 상한 없이 전수를 그린다(겹침 그대로 노출).
    좌표는 x/y 지터로 겹침을 추가로 흩뿌린다.
    """
    top = data["top"]
    lx = np.log2(np.clip(data["x"], 1, None))
    y, dom = data["y"], data["dom"]

    ci = np.full(len(dom), -1, dtype=np.int64)
    for i, pc in enumerate(top):
        ci[dom == pc] = i
    is_top = ci >= 0

    def _jit(n):
        return rng.uniform(-0.045, 0.045, n), rng.uniform(-0.32, 0.32, n)

    def _thin(idx, cap):
        if sample and len(idx) > cap:
            idx = rng.choice(idx, size=cap, replace=False)
        return idx

    nb_idx = _thin(np.flatnonzero(~is_top), cap_other)
    if len(nb_idx):
        jx, jy = _jit(len(nb_idx))
        ax.scatter(lx[nb_idx] + jx, np.clip(y[nb_idx] + jy, 0.5, None),
                   s=16, c=OTHER_COLOR, marker=OTHER_MARKER, alpha=0.4,
                   edgecolors="black", linewidths=0.3, rasterized=True, zorder=1)

    # PC별로 통째로 그리면 큰 클래스가 작은 클래스를 다 덮음. 각 PC의 인덱스를
    # K개 청크로 쪼개 라운드로빈으로 그리고 라운드마다 PC 순서를 회전 → 화면
    # 어느 지역에서도 마지막에 얹히는 PC가 매번 바뀌어 모든 PC가 관측 가능.
    K = 16
    pc_indices = []
    for i, pc in enumerate(top):
        sel_idx = _thin(np.flatnonzero(dom == pc), cap_pc)
        rng.shuffle(sel_idx)                  # sel_idx[r::K] 청크가 편향되지 않게
        pc_indices.append(sel_idx)
    n_top = len(pc_indices)
    for round_r in range(K):
        for slot in range(n_top):
            i = (round_r + slot) % n_top      # 라운드마다 top-of-round PC 회전
            sel_idx = pc_indices[i]
            if not len(sel_idx):
                continue
            chunk = sel_idx[round_r::K]
            if not len(chunk):
                continue
            jx, jy = _jit(len(chunk))
            ax.scatter(lx[chunk] + jx, np.clip(y[chunk] + jy, 0.5, None),
                       s=32, c=PALETTE[i % len(PALETTE)],
                       marker=MARKERS[i % len(MARKERS)],
                       alpha=0.7, edgecolors="black", linewidths=0.4,
                       rasterized=True, zorder=3 + round_r * n_top + slot)

    if show_mean:
        # 색이 부여된 상위 PC 전부에 평균 대표점(라벨/리더 없음). top 은 build()
        # 에서 dominant-line count 내림차순이라 top[:N] 이 상위 N. dot 색 = PC
        # 색, 흰 테두리로 산포 위 시각 앵커.
        MEAN_TOP_N = MAX_PCS_COLOR
        top_zbase = 3 + K * n_top
        for i, pc in enumerate(top[:MEAN_TOP_N]):
            sel = dom == pc
            if not sel.any():
                continue
            mx = float(lx[sel].mean())
            my = float(y[sel].mean())
            ax.scatter([mx], [my], s=60, c=PALETTE[i % len(PALETTE)],
                       marker="o", edgecolors="white", linewidths=1.4,
                       zorder=top_zbase + 20 + i)

    ax.set_ylim(0.5, geom.num + 1.5)
    xhi = float(lx.max()) if len(lx) else 4.0
    if xmax is not None:
        cut = int((lx > xmax).sum())
        if cut:
            print(f"    [xmax={xmax}] clipped {cut}/{len(lx)} lines "
                  f"({100 * cut / len(lx):.2f}%) beyond 2^{xmax}", flush=True)
        xhi = float(xmax)
    ax.set_xlim(-0.2, xhi + 0.15)
    ax.xaxis.set_major_locator(plt.MultipleLocator(2))
    ax.xaxis.set_major_formatter(FuncFormatter(lambda v, _: rf"$2^{{{int(v)}}}$"))
    ax.set_xlabel("Access Count", fontsize=14)
    ax.set_ylabel("Sharing Degree", fontsize=14)
    ax.tick_params(axis="both", labelsize=12)
    ax.grid(True, which="major", alpha=0.5, linestyle="--", linewidth=0.5)
    for spine in ax.spines.values():
        spine.set_visible(True)
        spine.set_color("black")
        spine.set_linewidth(0.6)
    if title:
        ax.set_title(title, fontsize=12)


def _legend_handles(data):
    from matplotlib.lines import Line2D

    def _h(marker, color, label):
        return Line2D([0], [0], marker=marker, linestyle="", markersize=6,
                      markerfacecolor=color, markeredgecolor="black",
                      markeredgewidth=0.5, label=label)

    hs = [_h(MARKERS[i % len(MARKERS)], PALETTE[i % len(PALETTE)], f"PC{i}")
          for i in range(len(data["top"]))]
    rest = data["n_pcs"] - len(data["top"])
    if rest > 0:
        hs.append(_h(OTHER_MARKER, OTHER_COLOR, f"other"))
    return hs


def plot_individual(bench, data, geom, out, mode, xmax=None,
                     sample=False, cap_pc=250, cap_other=1500, show_mean=False):
    apply_paper_style()
    rng = np.random.default_rng(0)
    fig, ax = plt.subplots(figsize=(SIZE_SINGLE[0] * 1.6, SIZE_SINGLE[1] * 1.6))
    disp = DISPLAY_NAMES.get(bench, bench.upper())
    _scatter(ax, data, geom, rng, xmax=xmax, sample=sample, cap_pc=cap_pc, cap_other=cap_other,
             show_mean=show_mean,
             title=f"{disp}  [{mode}]  ({data['n_pcs']} PCs, {len(data['x'])} lines)")
    ax.legend(handles=_legend_handles(data), frameon=False, fontsize=8,
              ncol=1, loc="center left", bbox_to_anchor=(1.01, 0.5),
              handletextpad=0.2, labelspacing=1.0)
    save_fig(fig, out, f"{bench}_line_scatter_bw_{mode}", formats=("png", "pdf"))


def plot_grid(results, geom, out, mode, xmax=None, sample=False, cap_pc=250, cap_other=1500,
              show_mean=False):
    apply_paper_style()
    rng = np.random.default_rng(0)
    n = len(results)
    ncols = math.ceil(math.sqrt(n))          # 정사각형에 최대한 가깝게
    nrows = math.ceil(n / ncols)
    fig, axes = plt.subplots(nrows, ncols,
                             figsize=(4 * ncols, 2 * nrows), squeeze=False)
    for idx, (bench, data) in enumerate(results):
        ax = axes[idx // ncols][idx % ncols]
        _scatter(ax, data, geom, rng, title=DISPLAY_NAMES.get(bench, bench.upper()), xmax=xmax,
                 sample=sample, cap_pc=cap_pc, cap_other=cap_other, show_mean=show_mean)
        if idx % ncols != 0:
            ax.set_ylabel("")
        if idx + ncols < n:
            ax.set_xlabel("")
    for idx in range(n, nrows * ncols):
        axes[idx // ncols][idx % ncols].axis("off")
    save_fig(fig, out, f"grid_bw_{mode}", formats=("png", "pdf"))


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("run_id", nargs="?", default="latest")
    ap.add_argument("--access", choices=ACCESS_MODES, default="all",
                    help="접근 필터: all=모든 demand / rdma=remote demand / "
                         "l2miss-rdma=remote L2미스(실제 RDMA 트랜잭션)")
    ap.add_argument("--xmax", type=float, default=None,
                    help="x축 상한을 10^N 에서 자름 (예: 3 -> 10^3). 본체가 넓게 "
                         "퍼지지만 넘는 라인은 잘림(잘린 수를 출력). 기본=데이터에 맞춤")
    ap.add_argument("--benches", type=str, default=None,
                    help="추가 subset grid(grid_bw_<mode>_subset.png) 에 넣을 벤치마크 "
                         "콤마 목록 (예: xsbench,gups,pagerank). 전체 벤치마크 개별 png "
                         "+ grid_bw_<mode>.png 는 항상 기본대로(params.benchmarks 전체) 그림")
    ap.add_argument("--sampling", type=str, choices=("true", "false"), default="false",
                    help="true=클래스별 표본 상한 적용(겹침 완화, 기본) / "
                         "false=전수 표시(상한 없음, 겹침 그대로)")
    ap.add_argument("--cap-pc", type=int, default=600,
                    help="top-PC 클래스당 표시 라인 표본 상한(--sampling=true 일 때만 적용)")
    ap.add_argument("--cap-other", type=int, default=4000,
                    help="'other' 회색 구름의 표시 표본 상한(--sampling=true 일 때만 적용)")
    ap.add_argument("--mean", type=str, choices=("true", "false"), default="false",
                    help="true=top-PC 별 캐시라인 평균 지점(X 마커, 전수 라인 기준)을 "
                         "산포 위에 덧그림 / false=평균 표시 없음(기본)")
    args = ap.parse_args()
    mode = args.access
    sample = args.sampling == "true"
    show_mean = args.mean == "true"

    trace_dir = str(memory_trace_dir(args.run_id))
    config = run_config(args.run_id)
    suffix = file_suffix_for_config(config)
    n_side, num = mesh_dims_from_config(config)
    geom = MeshGeom(n_side=n_side, num=num)
    out = domain_out_dir("pc-stats", "pc-line-scatter-bw", args.run_id) / mode
    out.mkdir(parents=True, exist_ok=True)

    with ProcessPoolExecutor() as ex:
        raw = list(ex.map(build, benchmarks, repeat(trace_dir),
                          repeat(suffix), repeat(mode)))
    results = [r for r in raw if r is not None]
    if not results:
        print("no usable benchmarks.", file=sys.stderr)
        sys.exit(1)

    for bench, data in results:
        plot_individual(bench, data, geom, out, mode, args.xmax, sample,
                        args.cap_pc, args.cap_other, show_mean)
    plot_grid(results, geom, out, mode, args.xmax, sample, args.cap_pc, args.cap_other, show_mean)
    print(f"wrote {out}/<bench>_line_scatter_bw_{mode}.{{png}} + grid_bw_{mode}.png "
          f"({len(results)} benches, access={mode}, sampling={sample}, mean={show_mean})")

    if args.benches:
        sel = [b.strip() for b in args.benches.split(",") if b.strip()]
        sub = [(bench, data) for bench, data in results if bench in sel]
        missing = set(sel) - {b for b, _ in sub}
        if missing:
            print(f"[subset] no usable data for: {sorted(missing)}", file=sys.stderr)
        if sub:
            sub.sort(key=lambda bd: sel.index(bd[0]))
            plot_grid(sub, geom, out, f"{mode}_subset", args.xmax, sample,
                      args.cap_pc, args.cap_other, show_mean)
            print(f"wrote {out}/grid_bw_{mode}_subset.png ({len(sub)} benches)")


if __name__ == "__main__":
    main()
