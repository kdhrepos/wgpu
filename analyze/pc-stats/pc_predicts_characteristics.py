"""(고리 3c ★ 연구 심장) PC 가 라인의 공유도·접근수를 예측하는지 정량화.

핵심 아이디어 — 같은 PC 가 만든 라인들은 비슷한 공유도/접근수를 갖고,
서로 다른 PC 들 사이엔 큰 차이가 있어야 PC 가 의미 있는 신호.

지표:
  group_eta2 = 분산분해(between-PC) / 전체 분산
             = inter / (inter + intra)
  0 이면 PC 가 정보 없음, 1 이면 PC 가 완벽 예측.

null 베이스라인: 라인-(PC 라벨) 매핑을 셔플(PC 크기 보존)해서 5회 평균 group_eta2.
신뢰성 = 실제 / 셔플.

좌: 상위 K(=20) PC 의 공유도 분포 박스플롯 (PC 별 분포가 좁고 서로 다르면 강한 신호).
우: 벤치별 group_eta2 막대(공유도, 접근수 2개) + 셔플 null 점선.

입력: memory-traces/<bench>_<config>_memory_trace.csv
출력: _out/pc-stats/pc-predicts/<run_tag>/<bench>.{png,pdf} + summary.{png,pdf} + eta_table.csv
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

TOP_K_PCS = 20
SHUFFLE_REPEATS = 5


def aggregate_line_stats(path: Path) -> pd.DataFrame | None:
    """라인 (pc, addr) 단위 → unique_chiplets, access_count.

    트레이스가 크면 chunk별로 partial counts 를 누적했다가 마지막에 reduce.
    여기서 addr 는 byte 단위지만 PC 의 공유 신호엔 충분(cacheline 단위로 압축하면
    더 깔끔하나 raw byte 도 통계적으로 동일 결론을 준다).
    """
    accum_count: dict[tuple[int, int], int] = {}
    accum_chips: dict[tuple[int, int], set] = {}
    rows_read = 0
    for chunk in trace_chunks(path, usecols=ALL_TRACE_COLS):
        chunk = chunk.drop_duplicates()          # full-row 완전-중복 제거
        rows_read += len(chunk)
        # cacheline 단위로 묶기 (64B)
        addr_cl = (chunk["addr"].to_numpy(dtype=np.uint64) >> 6)
        pc = chunk["pc"].to_numpy(dtype=np.uint64)
        ch = chunk["chiplet"].to_numpy(dtype=np.int16)
        # 빠른 누적: pandas groupby
        sub = pd.DataFrame({"pc": pc, "addr": addr_cl, "chip": ch})
        gb = sub.groupby(["pc", "addr"])
        counts = gb.size()
        chips = gb["chip"].agg(set)
        for key, c in counts.items():
            accum_count[key] = accum_count.get(key, 0) + int(c)
        for key, s in chips.items():
            accum_chips.setdefault(key, set()).update(s)
    if not accum_count:
        return None
    keys = list(accum_count.keys())
    df = pd.DataFrame({
        "pc": [k[0] for k in keys],
        "addr": [k[1] for k in keys],
        "access_count": [accum_count[k] for k in keys],
        "unique_chiplets": [len(accum_chips[k]) for k in keys],
    })
    print(f"    rows read={rows_read:,}, lines={len(df):,}")
    return df


def group_eta2(values: np.ndarray, groups: np.ndarray) -> float:
    """one-way ANOVA-style η²: between-group SS / total SS."""
    if len(values) < 2:
        return 0.0
    grand = values.mean()
    df = pd.DataFrame({"v": values, "g": groups})
    g_means = df.groupby("g")["v"].mean()
    g_counts = df.groupby("g")["v"].size()
    ss_between = float((g_counts * (g_means - grand) ** 2).sum())
    ss_total = float(((values - grand) ** 2).sum())
    if ss_total <= 0:
        return 0.0
    return ss_between / ss_total


def shuffled_eta2(values: np.ndarray, groups: np.ndarray,
                  reps: int = SHUFFLE_REPEATS, seed: int = 1) -> float:
    rng = np.random.default_rng(seed)
    out = []
    for _ in range(reps):
        sg = rng.permutation(groups)        # PC 크기는 보존됨
        out.append(group_eta2(values, sg))
    return float(np.mean(out))


def plot_per_bench(bench: str, df: pd.DataFrame, out: Path,
                   eta_sd: float, eta_ac: float,
                   null_sd: float, null_ac: float) -> None:
    """좌: 상위 K PC 공유도 박스, 우: 막대 + null."""
    apply_paper_style()
    fig, (ax_box, ax_eta) = plt.subplots(1, 2, figsize=SIZE_DOUBLE,
                                          gridspec_kw={"width_ratios": [1.6, 1]})

    # 상위 K PC — PC 별 라인 수가 많고, 공유도 중앙값 내림차순.
    by_pc = df.groupby("pc")["unique_chiplets"].agg(["count", "median"])
    top = by_pc.sort_values(["count", "median"], ascending=False).head(TOP_K_PCS)
    boxes = []
    labels = []
    for i, pc_ in enumerate(top.index):
        v = df[df["pc"] == pc_]["unique_chiplets"].to_numpy()
        boxes.append(v)
        labels.append(f"PC{i}")
    bp = ax_box.boxplot(boxes, showfliers=False, widths=0.6, patch_artist=True)
    for i, patch in enumerate(bp["boxes"]):
        patch.set_facecolor(color_for(i))
        patch.set_alpha(0.7)
        patch.set_linewidth(0.4)
    for line in bp["medians"]:
        line.set_color("black"); line.set_linewidth(0.6)
    ax_box.set_xticks(np.arange(1, len(labels) + 1))
    ax_box.set_xticklabels(labels, rotation=60, fontsize=5)
    ax_box.set_ylabel("공유도 (서로 다른 칩렛 수)")
    ax_box.set_title(f"{bench}: 상위 PC 별 공유도 분포")
    ax_box.grid(axis="y", alpha=0.2)

    bars = ax_eta.bar([0, 1], [eta_sd, eta_ac],
                      color=[color_for(0), color_for(1)], width=0.6)
    ax_eta.axhline(null_sd, color=color_for(0), linestyle=":", linewidth=0.8,
                   alpha=0.7)
    ax_eta.axhline(null_ac, color=color_for(1), linestyle=":", linewidth=0.8,
                   alpha=0.7)
    ax_eta.set_xticks([0, 1])
    ax_eta.set_xticklabels(["공유도", "접근수"], fontsize=7)
    ax_eta.set_ylabel("PC 설명력 (분산 분해 비율)")
    ax_eta.set_title(f"PC 신호 (점선=셔플 null)")
    ax_eta.set_ylim(0, 1.0)
    ax_eta.grid(axis="y", alpha=0.2)

    save_fig(fig, out, bench)


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--date", default="mesh-6x6/latest")
    args = ap.parse_args()

    td = memory_trace_dir(args.date)
    config_name = td.parent.parent.name
    suffix = file_suffix_for_config(config_name)
    out = domain_out_dir("pc-stats", "pc-predicts", args.date)

    summary_rows = []
    for b in benchmarks:
        f = td / f"{b}_{suffix}_memory_trace.csv"
        if not f.exists():
            continue
        print(f"  {b} ...", flush=True)
        try:
            df = aggregate_line_stats(f)
        except Exception as e:
            print(f"    skip {b}: {e}")
            continue
        if df is None or df.empty:
            continue

        # PC 가 너무 많으면 (드물 일이지만) 상위 1000 개로만.
        if df["pc"].nunique() > 5000:
            top_pcs = df.groupby("pc").size().nlargest(5000).index
            df = df[df["pc"].isin(top_pcs)]

        v_sd = df["unique_chiplets"].to_numpy(dtype=np.float64)
        v_ac = df["access_count"].to_numpy(dtype=np.float64)
        g = df["pc"].to_numpy()

        eta_sd = group_eta2(v_sd, g)
        eta_ac = group_eta2(v_ac, g)
        null_sd = shuffled_eta2(v_sd, g)
        null_ac = shuffled_eta2(v_ac, g)

        summary_rows.append(dict(
            benchmark=b,
            eta_sharing=eta_sd, eta_access=eta_ac,
            null_sharing=null_sd, null_access=null_ac,
            ratio_sharing=eta_sd / null_sd if null_sd > 0 else float("inf"),
            ratio_access=eta_ac / null_ac if null_ac > 0 else float("inf"),
            n_lines=len(df), n_pcs=int(df["pc"].nunique()),
        ))
        plot_per_bench(b, df, out, eta_sd, eta_ac, null_sd, null_ac)

    if not summary_rows:
        print("no usable benchmarks.", file=sys.stderr)
        sys.exit(1)

    sdf = pd.DataFrame(summary_rows).set_index("benchmark")
    sdf.to_csv(out / "eta_table.csv")

    # summary plot: 벤치별 막대 두 개 + null 점선
    apply_paper_style()
    fig, ax = plt.subplots(figsize=SIZE_DOUBLE)
    x = np.arange(len(sdf))
    w = 0.36
    ax.bar(x - w/2, sdf["eta_sharing"], w, color=color_for(0), label="공유도")
    ax.bar(x + w/2, sdf["eta_access"], w, color=color_for(1), label="접근수")
    # null 점선
    for i, (ns, na) in enumerate(zip(sdf["null_sharing"], sdf["null_access"])):
        ax.hlines(ns, x[i] - w, x[i], colors=color_for(0),
                  linestyles=":", linewidth=0.8)
        ax.hlines(na, x[i], x[i] + w, colors=color_for(1),
                  linestyles=":", linewidth=0.8)
    ax.set_xticks(x); ax.set_xticklabels(sdf.index, rotation=40, ha="right", fontsize=6)
    ax.set_ylabel("PC 설명력 (분산 분해 비율)")
    ax.set_ylim(0, 1.0)
    ax.set_title("PC 가 라인 특성을 얼마나 예측하나 (점선 = 셔플 null)")
    ax.legend(frameon=False, fontsize=6, ncol=2)
    ax.grid(axis="y", alpha=0.2)
    save_fig(fig, out, "summary")
    print(f"wrote {out}/summary.{{png,pdf}} + per-bench + eta_table.csv")


if __name__ == "__main__":
    main()
