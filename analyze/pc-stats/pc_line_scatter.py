"""Motivation 1 (라인 단위) — 캐시라인 하나하나를 점으로, 색은 그 라인의 PC.

pc_hotness_signature 의 산포는 점 하나가 PC(집계)였다.  여기선 점 하나가 **캐시라인**:
  x = 그 라인의 demand 접근수 (reuse 강도)
  y = 그 라인을 만진 distinct 칩렛 수 (sharing 폭)
  색 = 그 라인의 dominant PC (그 라인을 가장 많이 접근한 PC)

같은 PC 의 라인들이 (x, y) 평면에서 한 덩어리로 뭉치고, 서로 다른 PC 는 다른 영역을
차지하면 → "PC 가 라인 특성을 가른다"가 라인 해상도에서 직접 보인다 (PC 내부 일관 +
PC 간 분리를 동시에).

필터를 통과한 **모든 고유 캐시라인**을 찍는다(top-PC 사전선별/표본 상한 없음).  한 라인을
여러 PC 가 만졌으면 그 라인 최다접근 PC(dominant) 를 따라 색이 정해진다.  팔레트가 유한해
소유 라인 수 상위 MAX_PCS_COLOR 개 PC 만 고유 색, 나머지 PC 의 라인은 회색으로 함께 표시.

겹침 대책: PC 별 layer 를 쌓지 않고 순서를 섞어 한 번에 그려 z-order 편향 제거 + x/y 지터.

파이프라인: chunk 읽기 -> full-row(전 11컬럼) drop_duplicates -> 필요한 컬럼 필터/집계.
dedup 은 파일 쓰기 오류로 생긴 완전-동일 row 만 제거(time 이 다른 정상 재사용은 보존).
chunk 단위라 청크 경계를 넘는 완전-중복은 못 잡음(현 데이터는 완전-중복 0 확인).

접근 필터(--access): all=모든 demand 접근 / rdma=remote(owner!=chiplet) demand /
l2miss-rdma=remote L2 미스(= 실제 RDMA 트랜잭션).  선택값이 파일명에 박힘.

입력:  memory-traces/<bench>_<config>_memory_trace.csv
출력:  _out/pc-stats/pc-line-scatter/<run_tag>/<mode>/
         <bench>_line_scatter_<mode>.png + grid_<mode>.png

사용:  analyze/run.sh pc-stats/pc_line_scatter.py <run-id> [--access all|rdma|l2miss-rdma]
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
from matplotlib.ticker import FuncFormatter

from _paths import memory_trace_dir, run_config, domain_out_dir
from motivation_common import file_suffix_for_config, trace_chunks, ALL_TRACE_COLS
from plot_style import apply_paper_style, save_fig, SIZE_SINGLE, SIZE_WIDE
from pc_hotness import MeshGeom, mesh_dims_from_config
from params import benchmarks

DEMAND_TYPES = {0, 1, 2, 3, 4, 5}
L2MISS_TYPES = {2, 5}       # data-read-miss / data-write-miss (L2 미스)
ACCESS_MODES = ("all", "rdma", "l2miss-rdma")
# 모든 고유 캐시라인을 찍는다(표본 상한 없음).  PC 는 전부 후보이고, 팔레트가 유한하니
# "라인을 가장 많이 소유한" 상위 MAX_PCS_COLOR 개만 고유 색을 받고 나머지는 회색 배경.
MAX_PCS_COLOR = 10          # 고유 색을 줄 PC 수 (팔레트 가독성 한계)
OTHER_COLOR = "#c8c8c8"     # 색을 못 받은 PC 들의 라인

# full-row(전 11컬럼) 중복 제거용.  파일 쓰기 오류(NFS append 재시도 등)로 생긴
# 완전-동일 row 만 제거하고, time 이 다른 정상 재사용은 보존한다.
READ_COLS = ALL_TRACE_COLS


def _demand_mask(chunk, mode: str):
    """접근 필터. pc!=0 이 이미 PTW(pc=0) 제외.  mode:
    all=모든 demand / rdma=remote(owner!=chiplet) demand / l2miss-rdma=remote L2미스."""
    types = L2MISS_TYPES if mode == "l2miss-rdma" else DEMAND_TYPES
    m = chunk["type"].isin(types) & (chunk["pc"] != 0)
    if mode in ("rdma", "l2miss-rdma"):
        m &= (chunk["rdma"] == 1)
    return m

# 정성 12색 (색약 배려, 서로 명도/색상 충분히 차이).  MAX_PCS_COLOR 까지 사용.
PALETTE = [
    "#0072B2", "#D55E00", "#009E73", "#CC79A7", "#E69F00",
    "#56B4E9", "#F0E442", "#661100", "#332288", "#999933",
]


def _collect_lines(trace_file: str, mode: str):
    """단일 패스 — chunk 별 (pc, line) 접근수 Series + (pc, line, chip) 유니크
    triples DataFrame 누적. pandas 벡터화만 사용(람다 .agg 금지 = 최대 병목).

    data_id 가 전부 -1 인 PC(= pc_stats 에서 data_ids==[]) 는 buffer 매핑 불가라
    plotting 에서 제외 — 최소 한 번이라도 data_id!=-1 인 PC 만 남긴다."""
    counts_parts: list[pd.Series] = []
    triples_parts: list[pd.DataFrame] = []
    pc_has_data: set[int] = set()
    for chunk in trace_chunks(trace_file, usecols=READ_COLS):
        chunk = chunk.drop_duplicates()           # full-row 완전-중복 제거
        mnp = _demand_mask(chunk, mode).to_numpy()
        if not mnp.any():
            continue
        pc_arr = chunk["pc"].to_numpy(dtype=np.uint64)[mnp]
        line_arr = chunk["addr"].to_numpy(dtype=np.uint64)[mnp] >> np.uint64(6)
        chip_arr = chunk["chiplet"].to_numpy(dtype=np.int16)[mnp]
        did_arr = chunk["data_id"].to_numpy(dtype=np.int16)[mnp]
        valid = did_arr != -1
        if valid.any():
            pc_has_data.update(int(p) for p in np.unique(pc_arr[valid]))
        sub = pd.DataFrame({"pc": pc_arr, "line": line_arr, "chip": chip_arr})
        counts_parts.append(sub.groupby(["pc", "line"], sort=False).size())
        triples_parts.append(sub.drop_duplicates(ignore_index=True))
    if not counts_parts:
        return None
    counts = pd.concat(counts_parts).groupby(level=[0, 1], sort=False).sum()
    triples = pd.concat(triples_parts, ignore_index=True).drop_duplicates(ignore_index=True)
    keep = counts.index.get_level_values(0).isin(pc_has_data)
    counts = counts[keep]
    triples = triples[triples["pc"].isin(pc_has_data)]
    return counts, triples


def build(bench: str, trace_dir: str, suffix: str, mode: str):
    trace_file = f"{trace_dir}/{bench}_{suffix}_memory_trace.csv"
    if not os.path.exists(trace_file):
        return None
    print(f"[{bench}] per-line scatter ({mode}) ...", flush=True)
    try:
        res = _collect_lines(trace_file, mode)
        if res is None:
            return None
        counts, triples = res
        if counts.empty:
            return None

        # 라인별 집계 전부 벡터화. dominant PC = per-line argmax cnt,
        # sharing = per-line unique chip 수(= OR bitmask 의 bit_count 와 동치).
        df = counts.rename("cnt").reset_index()
        line_tot = df.groupby("line", sort=False)["cnt"].sum()
        best_idx = df.groupby("line", sort=False)["cnt"].idxmax()
        best_pc = df.loc[best_idx].set_index("line")["pc"]
        line_shared = (triples[["line", "chip"]]
                       .drop_duplicates()
                       .groupby("line", sort=False).size())

        lines = line_tot.index.to_numpy()
        x = line_tot.to_numpy(dtype=np.float64)
        y = line_shared.reindex(lines).to_numpy(dtype=np.float64)
        dom = best_pc.reindex(lines).to_numpy(dtype=np.int64)

        # 색을 줄 PC = 소유(dominant) 라인 수 상위 MAX_PCS_COLOR 개.  나머지는 회색.
        pcs, cnts = np.unique(dom, return_counts=True)
        top = [int(p) for p in pcs[np.argsort(-cnts)][:MAX_PCS_COLOR]]
        return bench, dict(top=top, n_pcs=int(len(pcs)), x=x, y=y, dom=dom)
    except Exception as e:  # noqa: BLE001
        print(f"[{bench}] error: {e}")
        return None


def _scatter(ax, data, geom: MeshGeom, rng, legend=True, title=None, xmax=None):
    """점 = 고유 캐시라인 **전수**, 색 = dominant PC.  x=log10(접근수), y=sharing.

    겹침 대책: (1) PC 별로 layer 를 쌓지 않고 순서를 섞어 한 번에 그려 특정 PC 가
    위를 덮는 z-order 편향 제거, (2) x/y 양쪽 지터로 동일 좌표를 흩뿌림.
    """
    top = data["top"]
    lx = np.log10(np.clip(data["x"], 1, None))
    y, dom = data["y"], data["dom"]

    ci = np.full(len(dom), -1, dtype=np.int64)      # 색 인덱스 (-1 = 색 없음)
    for i, pc in enumerate(top):
        ci[dom == pc] = i
    is_top = ci >= 0

    def _jit(n):
        return rng.uniform(-0.045, 0.045, n), rng.uniform(-0.32, 0.32, n)

    nb = int((~is_top).sum())                       # 색 못 받은 PC 라인도 전부 표시
    if nb:
        jx, jy = _jit(nb)
        ax.scatter(lx[~is_top] + jx, np.clip(y[~is_top] + jy, 0.5, None),
                   s=1, c=OTHER_COLOR, alpha=0.35, linewidths=0,
                   rasterized=True, zorder=1)

    idx = np.flatnonzero(is_top)
    idx = idx[rng.permutation(len(idx))]            # 섞어서 공평하게 겹치도록
    cols = np.asarray(PALETTE)[ci[idx] % len(PALETTE)]
    jx, jy = _jit(len(idx))
    ax.scatter(lx[idx] + jx, np.clip(y[idx] + jy, 0.5, None),
               s=5, c=cols, alpha=0.55, linewidths=0,
               rasterized=True, zorder=2)

    # PC 대표값 = top-PC 별 평균 (log10(접근수) 평균, sharing 평균).  마커는 작게
    # 유지하되 검은 테두리로 강조 → 산포를 덮지 않으면서 위치가 눈에 띈다.
    for i, pc in enumerate(top):
        sel = dom == pc
        if not sel.any():
            continue
        mx = float(lx[sel].mean())
        my = float(y[sel].mean())
        ax.scatter([mx], [my], s=28, marker="X",
                   c=PALETTE[i % len(PALETTE)],
                   edgecolors="black", linewidths=0.6,
                   alpha=0.95, zorder=3)

    ax.set_ylim(0.5, geom.num + 1.5)
    # 데이터 끝에 딱 맞춤 (decade 로 올림하면 오른쪽에 빈 여백이 생겨 본체가 눌린다).
    # xmax 를 주면 거기서 자름 — 그만큼 본체가 넓게 퍼지지만 넘는 라인은 잘려 나간다.
    xhi = float(lx.max()) if len(lx) else 4.0
    if xmax is not None:
        cut = int((lx > xmax).sum())
        if cut:
            print(f"    [xmax={xmax}] clipped {cut}/{len(lx)} lines "
                  f"({100 * cut / len(lx):.2f}%) beyond 10^{xmax}", flush=True)
        xhi = float(xmax)
    ax.set_xlim(-0.2, xhi + 0.15)
    ax.xaxis.set_major_locator(plt.MultipleLocator(1))
    ax.xaxis.set_major_formatter(FuncFormatter(lambda v, _: rf"$10^{{{int(v)}}}$"))
    ax.set_xlabel("line access count  (reuse)")
    ax.set_ylabel("sharing degree  (# chiplets)")
    ax.grid(True, which="major", alpha=0.2)
    if title:
        ax.set_title(title, fontsize=8)


def _legend_handles(data):
    from matplotlib.lines import Line2D

    def _h(color, label):
        return Line2D([0], [0], marker="o", linestyle="", markersize=4.5,
                      markerfacecolor=color, markeredgewidth=0, label=label)

    hs = [_h(PALETTE[i % len(PALETTE)], f"PC{i}") for i in range(len(data["top"]))]
    rest = data["n_pcs"] - len(data["top"])
    if rest > 0:
        hs.append(_h(OTHER_COLOR, f"other ({rest} PCs)"))
    return hs


def plot_individual(bench, data, geom, out, mode, xmax=None):
    apply_paper_style()
    rng = np.random.default_rng(0)
    fig, ax = plt.subplots(figsize=(SIZE_SINGLE[0] * 1.4, SIZE_SINGLE[1] * 1.4))
    _scatter(ax, data, geom, rng, legend=False, xmax=xmax,
             title=f"{bench}  [{mode}]  ({data['n_pcs']} PCs, {len(data['x'])} lines)")
    ax.legend(handles=_legend_handles(data), frameon=False, fontsize=6,
              ncol=1, loc="center left", bbox_to_anchor=(1.01, 0.5),
              handletextpad=0.2, labelspacing=0.3)
    save_fig(fig, out, f"{bench}_line_scatter_{mode}", formats=("png",))


def plot_grid(results, geom, out, mode, xmax=None):
    apply_paper_style()
    rng = np.random.default_rng(0)
    n = len(results)
    ncols = min(3, n)
    nrows = math.ceil(n / ncols)
    fig, axes = plt.subplots(nrows, ncols, figsize=(SIZE_WIDE[0], 2.4 * nrows),
                             squeeze=False)
    for idx, (bench, data) in enumerate(results):
        ax = axes[idx // ncols][idx % ncols]
        _scatter(ax, data, geom, rng, legend=False, title=bench, xmax=xmax)
        if idx % ncols != 0:
            ax.set_ylabel("")
        if idx + ncols < n:
            ax.set_xlabel("")
    for idx in range(n, nrows * ncols):
        axes[idx // ncols][idx % ncols].axis("off")
    save_fig(fig, out, f"grid_{mode}", formats=("png",))


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("run_id", nargs="?", default="latest")
    ap.add_argument("--access", choices=ACCESS_MODES, default="all",
                    help="접근 필터: all=모든 demand / rdma=remote demand / "
                         "l2miss-rdma=remote L2미스(실제 RDMA 트랜잭션)")
    ap.add_argument("--xmax", type=float, default=None,
                    help="x축 상한을 10^N 에서 자름 (예: 3 -> 10^3). 본체가 넓게 "
                         "퍼지지만 넘는 라인은 잘림(잘린 수를 출력). 기본=데이터에 맞춤")
    args = ap.parse_args()
    mode = args.access

    trace_dir = str(memory_trace_dir(args.run_id))
    config = run_config(args.run_id)
    suffix = file_suffix_for_config(config)
    n_side, num = mesh_dims_from_config(config)
    geom = MeshGeom(n_side=n_side, num=num)
    out = domain_out_dir("pc-stats", "pc-line-scatter", args.run_id) / mode
    out.mkdir(parents=True, exist_ok=True)

    with ProcessPoolExecutor() as ex:
        raw = list(ex.map(build, benchmarks, repeat(trace_dir),
                          repeat(suffix), repeat(mode)))
    results = [r for r in raw if r is not None]
    if not results:
        print("no usable benchmarks.", file=sys.stderr)
        sys.exit(1)

    for bench, data in results:
        plot_individual(bench, data, geom, out, mode, args.xmax)
    plot_grid(results, geom, out, mode, args.xmax)
    print(f"wrote {out}/<bench>_line_scatter_{mode}.{{png}} + grid_{mode}.png "
          f"({len(results)} benches, access={mode})")


if __name__ == "__main__":
    main()
