"""(고리 3d) PC 의 공유도 신호가 시간에 따라 안정적인지.

실행 시간을 N=10 등분(에폭). 각 에폭에서 PC 별 라인 평균 공유도 계산.
- 좌: 에폭 N 의 공유도 vs 에폭 N+1 — 점 = (PC, 인접 에폭쌍), 대각선 기준선,
       전체 피어슨 R² 주석.
- 우: 벤치별 lag-1 자기상관 막대 + 셔플 null 점선.

자기상관이 높고 R² 가 높으면 PC 가 시간에 따라 안정적이라는 신호.

입력: memory-traces/<bench>_<config>_memory_trace.csv
출력: _out/pc-stats/pc-stability/<run_tag>/<bench>.{png,pdf} + summary.{png,pdf}
"""

from __future__ import annotations

import argparse
import sys
from pathlib import Path

import numpy as np
import pandas as pd
import matplotlib.pyplot as plt

from _paths import memory_trace_dir, domain_out_dir
from plot_style import apply_paper_style, color_for, save_fig, SIZE_SINGLE, SIZE_DOUBLE
from motivation_common import trace_chunks, file_suffix_for_config, ALL_TRACE_COLS
from params import benchmarks

N_EPOCHS = 10
MIN_LINES_PER_PC = 4         # 잡음 줄이기


def compute_epoch_matrix(path: Path) -> pd.DataFrame | None:
    """PC × epoch — 평균 공유도(per-line unique chiplet) 행렬.

    1) PC, addr, chiplet, time 을 chunk 로 읽어 (time bin × PC × addr) → chiplet set
       을 만들면 너무 큼. 단순화: (epoch, pc, addr) 마다 chiplet set 누적.
    2) 끝에서 epoch×PC 별 평균 공유도 계산.
    """
    # 1차 패스 — time 최소/최대 구하기
    t_min = None; t_max = None
    for chunk in trace_chunks(path, usecols=["time"]):
        cmin = chunk["time"].min(); cmax = chunk["time"].max()
        t_min = cmin if t_min is None else min(t_min, cmin)
        t_max = cmax if t_max is None else max(t_max, cmax)
    if t_min is None or t_max is None or t_max <= t_min:
        return None
    bins = np.linspace(float(t_min), float(t_max), N_EPOCHS + 1)

    # 2차 패스 — epoch 마다 (pc, addr) 에 대한 chiplet set 누적
    epoch_sets: list[dict[tuple[int, int], set]] = [dict() for _ in range(N_EPOCHS)]
    for chunk in trace_chunks(path, usecols=ALL_TRACE_COLS):
        chunk = chunk.drop_duplicates()          # full-row 완전-중복 제거
        t = chunk["time"].to_numpy(dtype=np.float64)
        ep = np.clip(np.searchsorted(bins, t, side="right") - 1, 0, N_EPOCHS - 1)
        addr_cl = (chunk["addr"].to_numpy(dtype=np.uint64) >> 6)
        pc = chunk["pc"].to_numpy(dtype=np.uint64)
        ch = chunk["chiplet"].to_numpy(dtype=np.int16)
        sub = pd.DataFrame({"ep": ep, "pc": pc, "addr": addr_cl, "chip": ch})
        for ep_id, sub_ep in sub.groupby("ep"):
            d = epoch_sets[int(ep_id)]
            gb = sub_ep.groupby(["pc", "addr"])["chip"].agg(set)
            for key, s in gb.items():
                d.setdefault(key, set()).update(s)

    # epoch × PC -> mean unique_chiplets
    rows = []
    for ep in range(N_EPOCHS):
        if not epoch_sets[ep]:
            continue
        keys = list(epoch_sets[ep].keys())
        sd = pd.DataFrame({
            "pc": [k[0] for k in keys],
            "sd": [len(epoch_sets[ep][k]) for k in keys],
        })
        agg = sd.groupby("pc")["sd"].agg(["mean", "size"]).reset_index()
        agg["ep"] = ep
        rows.append(agg)
    if not rows:
        return None
    full = pd.concat(rows, ignore_index=True)
    full = full[full["size"] >= MIN_LINES_PER_PC]
    if full.empty:
        return None
    # PC × epoch (mean 공유도)
    pivot = full.pivot(index="pc", columns="ep", values="mean")
    return pivot


def epoch_pair_correlation(pivot: pd.DataFrame) -> tuple[np.ndarray, np.ndarray, float]:
    """모든 인접 (ep, ep+1) 쌍을 누적 -> (x, y, R²)."""
    eps = sorted(pivot.columns)
    xs = []; ys = []
    for a, b in zip(eps, eps[1:]):
        ab = pivot[[a, b]].dropna()
        xs.append(ab[a].to_numpy())
        ys.append(ab[b].to_numpy())
    if not xs:
        return np.array([]), np.array([]), 0.0
    x = np.concatenate(xs); y = np.concatenate(ys)
    if len(x) < 3:
        return x, y, 0.0
    r = np.corrcoef(x, y)[0, 1]
    return x, y, float(r ** 2)


def lag1_autocorr(pivot: pd.DataFrame) -> tuple[float, float]:
    """PC 시계열별 lag-1 자기상관 평균과 셔플 null 평균."""
    eps = sorted(pivot.columns)
    if len(eps) < 3:
        return 0.0, 0.0
    real = []; null = []
    rng = np.random.default_rng(7)
    for pc, row in pivot.iterrows():
        s = row[eps].dropna().to_numpy()
        if len(s) < 3 or s.std() == 0:
            continue
        ac = np.corrcoef(s[:-1], s[1:])[0, 1]
        if np.isfinite(ac):
            real.append(ac)
        # null: 같은 시계열 셔플
        for _ in range(3):
            sh = rng.permutation(s)
            ac_n = np.corrcoef(sh[:-1], sh[1:])[0, 1]
            if np.isfinite(ac_n):
                null.append(ac_n)
    if not real:
        return 0.0, 0.0
    return float(np.mean(real)), float(np.mean(null) if null else 0.0)


def plot_per_bench(bench: str, x: np.ndarray, y: np.ndarray, r2: float, out: Path):
    apply_paper_style()
    fig, ax = plt.subplots(figsize=SIZE_SINGLE)
    if len(x) > 0:
        lim = float(max(x.max(), y.max(), 1.0))
    else:
        lim = 36.0
    ax.plot([0, lim], [0, lim], color="#888", linestyle="--", linewidth=0.6)
    ax.scatter(x, y, s=4, alpha=0.4, color=color_for(0), edgecolors="none")
    ax.set_xlabel("에폭 N: 평균 공유도")
    ax.set_ylabel("에폭 N+1: 평균 공유도")
    ax.set_xlim(0, lim); ax.set_ylim(0, lim)
    ax.set_title(f"{bench}: 시간 안정성 (R²={r2:.2f})")
    ax.grid(alpha=0.2)
    save_fig(fig, out, bench)


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--date", default="mesh-6x6/latest")
    args = ap.parse_args()

    td = memory_trace_dir(args.date)
    config_name = td.parent.parent.name
    suffix = file_suffix_for_config(config_name)
    out = domain_out_dir("pc-stats", "pc-stability", args.date)

    summary = []
    for b in benchmarks:
        f = td / f"{b}_{suffix}_memory_trace.csv"
        if not f.exists():
            continue
        print(f"  {b} ...", flush=True)
        try:
            pivot = compute_epoch_matrix(f)
        except Exception as e:
            print(f"    skip {b}: {e}")
            continue
        if pivot is None or pivot.empty:
            continue
        x, y, r2 = epoch_pair_correlation(pivot)
        ac, ac_null = lag1_autocorr(pivot)
        summary.append(dict(benchmark=b, r2=r2, autocorr=ac, autocorr_null=ac_null,
                            n_pcs=int(pivot.shape[0])))
        plot_per_bench(b, x, y, r2, out)

    if not summary:
        print("no usable benchmarks.", file=sys.stderr)
        sys.exit(1)

    sdf = pd.DataFrame(summary).set_index("benchmark")
    sdf.to_csv(out / "stability_table.csv")

    apply_paper_style()
    fig, ax = plt.subplots(figsize=SIZE_DOUBLE)
    x = np.arange(len(sdf))
    ax.bar(x, sdf["autocorr"], 0.6, color=color_for(2), label="실제")
    for i, n in enumerate(sdf["autocorr_null"]):
        ax.hlines(n, x[i] - 0.35, x[i] + 0.35, colors="#444",
                  linestyles=":", linewidth=0.8)
    ax.set_xticks(x); ax.set_xticklabels(sdf.index, rotation=40, ha="right", fontsize=6)
    ax.set_ylabel("PC 시계열 lag-1 자기상관 (평균)")
    ax.set_title("PC 공유도 시간 안정성 (점선=셔플 null)")
    ax.set_ylim(-0.2, 1.0)
    ax.legend(frameon=False, fontsize=6)
    ax.grid(axis="y", alpha=0.2)
    save_fig(fig, out, "summary")
    print(f"wrote {out}/summary.{{png,pdf}} + per-bench + stability_table.csv")


if __name__ == "__main__":
    main()
