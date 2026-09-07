"""벤치마크별 shared / private-local / private-remote capacity — memory trace 기반.

정의:
  shared line        = 2개 이상 서로 다른 chiplet 이 demand 접근(type 0-5, is_ptw==0)
                        한 cacheline (addr>>6). capacity = line 수 × 64B.
  private line       = 정확히 1개 chiplet 만 접근한 cacheline.
  local / remote 기준 = 그 라인에 대한 접근들의 (chiplet, owner) 값.
                        chiplet(요청 발생 L2) == owner(주소의 home chiplet) 이면 local,
                        다르면 remote — trace 컬럼 `rdma` 와 동일한 기준(owner!=chiplet).
                        private line 은 유일한 접근 chiplet 기준으로 local/remote 확정.
  각 칩렛의 private-local / private-remote capacity = 그 칩렛이 단독 소유한
  local/remote 라인 수 × 64B. 벤치마크의 avg 값은 메시 전체 칩렛(access 0인 칩렛
  포함)에 대한 평균.

출력:
  ~/mgvm/figures/shared-private-capacity/<config>-<run_tag>/
      shared_private_local_remote_stacked.png (3-way: Shared/Private-local/Private-remote)
      shared_private_stacked.png (2-way: Shared/Private)
      per_benchmark.csv, per_chiplet_private.csv

사용:
  analyze/run.sh cacheline/shared_private_capacity.py <run-id>
  analyze/run.sh cacheline/shared_private_capacity.py <run-id> -j 8
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
import matplotlib as mpl
import matplotlib.pyplot as plt
import matplotlib.ticker as mticker

from _paths import memory_trace_dir, run_config, run_tag
from motivation_common import file_suffix_for_config, trace_chunks, ALL_TRACE_COLS
from plot_style import save_fig, SIZE_DOUBLE
from pc_hotness import mesh_dims_from_config
from params import benchmarks

DEMAND_TYPES = np.array([0, 1, 2, 3, 4, 5], dtype=np.int8)
USECOLS = ALL_TRACE_COLS
LINE_BYTES = 64

FIG_ROOT = Path.home() / "mgvm" / "figures" / "shared-private-capacity"
FIG_FORMATS = ("png",)


# ── (line,chip,owner) unique 3-쌍 raw ────────────────────────────────────────
def _agg_lines(trace_file: str) -> pd.DataFrame:
    """chunk 별 (line,chip,owner) unique → 청크 경계 병합.

    반환: DataFrame(line, chip, owner) — shared 라인은 그 라인을 실제 만진
    각 chiplet 마다 한 행씩. 그래서 이후 per-chiplet 집계가 "실제로 그 GPM
    이 접근한 라인만" 세게 됨. owner 는 라인의 home chiplet (chip==owner 면
    local access)."""
    parts: list[pd.DataFrame] = []
    for chunk in trace_chunks(trace_file, usecols=USECOLS):
        chunk = chunk.drop_duplicates()
        types = chunk["type"].to_numpy(dtype=np.int8)
        is_ptw = chunk["is_ptw"].to_numpy(dtype=np.int8)
        is_d = np.isin(types, DEMAND_TYPES) & (is_ptw == 0)
        if not is_d.any():
            continue
        cl = chunk["addr"].to_numpy(dtype=np.uint64)[is_d] >> np.uint64(6)
        chip = chunk["chiplet"].to_numpy(dtype=np.int64)[is_d]
        owner = chunk["owner"].to_numpy(dtype=np.int64)[is_d]
        sub = pd.DataFrame({"line": cl, "chip": chip, "owner": owner})
        parts.append(sub.drop_duplicates())
    if not parts:
        return pd.DataFrame(columns=["line", "chip", "owner"])
    return pd.concat(parts, ignore_index=True).drop_duplicates()


# ── per-benchmark worker ─────────────────────────────────────────────────────
def process_benchmark(bench: str, trace_dir: str, suffix: str, n_chiplets: int):
    trace_file = f"{trace_dir}/{bench}_{suffix}_memory_trace.csv"
    if not os.path.exists(trace_file):
        return None
    print(f"[{bench}] shared/private-local/private-remote capacity ...", flush=True)
    try:
        uniq = _agg_lines(trace_file)
        if uniq.empty:
            return None

        # 라인별 서로 다른 chiplet 개수 (전역 shared/private 판정용)
        nchip_per_line = uniq.groupby("line")["chip"].nunique()
        uniq = uniq.merge(nchip_per_line.rename("nchip"),
                          left_on="line", right_index=True)

        chip_idx = pd.RangeIndex(n_chiplets, name="chiplet")

        # shared: 각 chiplet 이 "실제로 접근한" shared 라인만 그 chiplet 으로.
        # (전역 shared 라인 중 이 chiplet 이 안 만진 건 이 chiplet 몫에서 제외)
        shared_touches = uniq[uniq["nchip"] >= 2]
        shared_per_chip = (shared_touches.groupby("chip").size()
                           .reindex(chip_idx, fill_value=0))

        # private (nchip==1): 유일 소유 chiplet 만 그 라인의 접근자. local/remote
        # = chip==owner 여부.
        private_touches = uniq[uniq["nchip"] == 1]
        local_mask = private_touches["chip"] == private_touches["owner"]
        local_per_chip = (private_touches.loc[local_mask, "chip"]
                          .value_counts().reindex(chip_idx, fill_value=0))
        remote_per_chip = (private_touches.loc[~local_mask, "chip"]
                           .value_counts().reindex(chip_idx, fill_value=0))
        private_per_chip = local_per_chip + remote_per_chip

        # total = 그 chiplet 의 L2 가 lifetime 동안 만진 unique line 총량
        total_per_chip = shared_per_chip + private_per_chip

        def _stats(prefix: str, per_chip: pd.Series) -> dict:
            cap_mb = per_chip * LINE_BYTES / (1024.0 ** 2)
            return {
                # 전 chiplet 합 — shared 라인은 여러 chiplet 에 걸쳐 여러 번
                # 카운트됨(각 chiplet 이 실제로 그 라인을 만졌으니 그 chiplet
                # 의 L2 슬라이스가 그만큼 잡음). pc_footprint.py 의 per-PC
                # 합계와 짝이 맞는 건 total (전체 lines_total × 64B) 이 아니라
                # (private_lines_total × 64B) + shared_lines_global × 64B 임.
                f"{prefix}_lines_total": int(per_chip.sum()),
                f"{prefix}_capacity_mb_total": float(cap_mb.sum()),
                f"avg_{prefix}_lines": float(per_chip.mean()),
                # per-chiplet 평균 = chiplet 의 L2 가 lifetime 동안 쥐고 있어야
                # 할 용량 평균. STACK plot / pc_footprint per-chiplet 평균과 짝.
                f"avg_{prefix}_capacity_mb": float(cap_mb.mean()),
                f"std_{prefix}_capacity_mb": float(cap_mb.std()),
            }

        # 전역 참고값: 실제 shared 라인 개수(중복 없이) — chiplet 합보다 훨씬 작음
        shared_lines_global = int((nchip_per_line >= 2).sum())

        row = {
            "benchmark": bench,
            "shared_lines_global": shared_lines_global,
            "shared_capacity_mb_global": shared_lines_global * LINE_BYTES / (1024.0 ** 2),
            **_stats("shared", shared_per_chip),
            **_stats("private_local", local_per_chip),
            **_stats("private_remote", remote_per_chip),
            **_stats("private", private_per_chip),
            **_stats("total", total_per_chip),
        }

        per_chip = pd.DataFrame({
            "shared_lines": shared_per_chip,
            "private_local_lines": local_per_chip,
            "private_remote_lines": remote_per_chip,
            "total_lines": total_per_chip,
        })
        for c in ["shared", "private_local", "private_remote", "total"]:
            per_chip[f"{c}_capacity_mb"] = per_chip[f"{c}_lines"] * LINE_BYTES / (1024.0 ** 2)
        per_chip["benchmark"] = bench
        return row, per_chip.reset_index()
    except Exception as e:  # noqa: BLE001
        print(f"[{bench}] error: {e}")
        return None


def run_one(run_id: str, workers: int):
    trace_dir = str(memory_trace_dir(run_id))
    config = run_config(run_id)
    suffix = file_suffix_for_config(config)
    _, n_chiplets = mesh_dims_from_config(config)

    if workers <= 1:
        raw = [process_benchmark(b, trace_dir, suffix, n_chiplets) for b in benchmarks]
    else:
        with ProcessPoolExecutor(max_workers=workers) as ex:
            raw = list(ex.map(process_benchmark, benchmarks,
                              repeat(trace_dir), repeat(suffix), repeat(n_chiplets)))

    rows, per_chip_frames = [], []
    for r in raw:
        if r is None:
            continue
        row, per_chip = r
        rows.append(row)
        per_chip_frames.append(per_chip)
    if not rows:
        return config, None, None

    df = pd.DataFrame(rows).set_index("benchmark")
    # total_capacity_mb = 한 chiplet L2 가 lifetime 동안 쥐고 있는 unique 용량
    # 평균 = avg_shared + avg_private_local + avg_private_remote (= avg_total)
    df["total_capacity_mb"] = df["avg_total_capacity_mb"]
    df = df.sort_values("total_capacity_mb")
    per_chip_df = pd.concat(per_chip_frames, ignore_index=True)
    return config, df, per_chip_df


# ── 그림 (흑백/hatch, 논문 스타일 stacked bar) ────────────────────────────────
def _apply_bw_style() -> None:
    mpl.rcParams.update({
        "font.family": "sans-serif",
        "font.sans-serif": ["Helvetica", "Arial", "DejaVu Sans"],
        "axes.unicode_minus": False,
        "font.size": 11,
        "axes.edgecolor": "black",
        "axes.linewidth": 0.5,
        "legend.fontsize": 10,
        "xtick.labelsize": 10,
        "ytick.labelsize": 10,
        "xtick.major.width": 0.6,
        "ytick.major.width": 0.6,
        "axes.labelsize": 12,
        "hatch.linewidth": 0.7,
        "savefig.dpi": 400,
        "savefig.bbox": "tight",
        "savefig.pad_inches": 0.05,
    })


def _pow2_ticks(top: float) -> list[float]:
    top = max(float(top), 1.0)
    max_pow = int(np.ceil(np.log2(top)))
    return [2 ** k for k in range(0, max_pow + 1)]


def plot_stacked(df: pd.DataFrame, stack_spec: list[dict], out: Path, filename: str) -> None:
    """stack_spec: [{"col","label","color","hatch"}, ...] bottom→top 순서."""
    _apply_bw_style()
    fig, ax = plt.subplots(figsize=SIZE_DOUBLE)

    benches = df.index.tolist()
    x = np.arange(len(benches))
    bottom = np.zeros(len(benches))
    for spec in stack_spec:
        vals = df[spec["col"]].to_numpy(dtype=np.float64)
        ax.bar(x, vals, bottom=bottom, width=0.2,
              color=spec["color"], edgecolor="black", linewidth=0.5,
              hatch=spec.get("hatch"), label=spec["label"], zorder=3)
        bottom += vals

    ticks = _pow2_ticks(bottom.max() if len(bottom) else 1.0)
    ax.set_yscale("log", base=2)
    ax.set_yticks(ticks)
    ax.set_yticklabels([str(t) for t in ticks])
    ax.yaxis.set_minor_locator(mticker.NullLocator())
    ax.set_ylim(ticks[0], ticks[-1])

    ax.set_xticks(x)
    DISPLAY_NAMES = {
        "cc": "CC", "gups": "GUPS", "llama70b_ffn": "LLAMA",
        "mis": "MIS", "mixtral_ffn": "MIXT", "mst": "MST",
        "pagerank": "PR", "spmv": "SPMV", "syr2k": "SYR2K",
        "xsbench": "XSB", "bptree": "BTR", "spmm": "SPMM",
    }
    ax.set_xticklabels([DISPLAY_NAMES.get(b, b.upper()) for b in benches],
                    rotation=30, ha="right")
    ax.set_ylabel("Memory Footprint (MB)")
    ax.set_axisbelow(True)
    ax.grid(True, which="major", axis="y", linestyle="--", color="0.5", linewidth=0.6, zorder=0)
    ax.grid(False, axis="x")
    for spine in ax.spines.values():
        spine.set_visible(True)
        spine.set_color("black")
        spine.set_linewidth(0.5)

    ax.legend(loc="lower center", bbox_to_anchor=(0.5, 1.0),
             ncol=len(stack_spec), frameon=False, edgecolor="black")
    save_fig(fig, out, filename, formats=FIG_FORMATS)


STACK_3WAY = [
    {"col": "avg_shared_capacity_mb", "label": "Shared", "color": "darkgray", "hatch": None},
    {"col": "avg_private_local_capacity_mb", "label": "Private-local", "color": "white", "hatch": "///"},
    {"col": "avg_private_remote_capacity_mb", "label": "Private-remote", "color": "white", "hatch": "..."},
]

STACK_2WAY = [
    {"col": "avg_shared_capacity_mb", "label": "Shared", "color": "darkgray", "hatch": None},
    {"col": "avg_private_capacity_mb", "label": "Private", "color": "white", "hatch": "///"},
]


# ── 메인 ─────────────────────────────────────────────────────────────────────
def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("run_id", nargs="?", default="mesh-6x6/latest")
    ap.add_argument("-j", "--workers", type=int, default=4,
                    help="벤치마크 병렬 워커 수 (기본 4, 1=순차)")
    args = ap.parse_args()

    config, df, per_chip_df = run_one(args.run_id, args.workers)
    if df is None:
        print("no usable benchmarks", file=sys.stderr)
        sys.exit(1)

    tag = run_tag(args.run_id)
    out = FIG_ROOT / f"{config}-{tag}"
    out.mkdir(parents=True, exist_ok=True)
    df.to_csv(out / "per_benchmark.csv")
    per_chip_df.to_csv(out / "per_chiplet_private.csv", index=False)
    plot_stacked(df, STACK_3WAY, out, "shared_private_local_remote_stacked")
    plot_stacked(df, STACK_2WAY, out, "shared_private_stacked")

    print(f"\n[{config}] wrote {out}")
    print(df[["avg_shared_capacity_mb",
             "avg_private_local_capacity_mb", "avg_private_remote_capacity_mb",
             "avg_private_capacity_mb", "total_capacity_mb"]]
         .to_string())


if __name__ == "__main__":
    main()
