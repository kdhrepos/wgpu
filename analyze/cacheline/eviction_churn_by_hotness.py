"""Motivation 2 (개선) — hotness 와 무관하게 evict/overwrite 되는가?  (churn ratio)

premature(window) 대신 window-free 비율로 "캐시가 hot 을 안 지켜준다"를 보인다.

라인 하나마다 (단일 트레이스, per-line, PC 아님 → 희석 없음):
  churn_ratio = (evict_count + overwrite_count) / demand_access_count
              = victim(type 8+9) 수 / demand 접근 수
              ≈ 그 라인의 miss rate (한 라인이 N번 축출되려면 N번 fetch=miss 필요).
  잘 상주 → 0,  thrash → 1.  bounded [0,1].

라인을 자기 접근수(hotness)로 rank tier(top1% / top10% / 나머지)로 묶어 tier 별
churn_ratio 를 벤치별 막대로.  hot tier 가 cold 와 비슷하거나 높으면 = hotness 가
축출을 못 막음 = 무분별.  보조로 eviction lift(hot 축출share / hot 접근share) 도.

입력:  memory-traces/<bench>_<config>_memory_trace.csv  (victim type 8/9 필요)
출력:  _out/cacheline/eviction-churn/<run_tag>/
         <bench>... 없음.  churn.csv, churn_by_tier.png, eviction_lift.png

사용:  analyze/run.sh cacheline/eviction_churn_by_hotness.py <run-id>
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
from plot_style import apply_paper_style, color_for, save_fig, SIZE_WIDE
from params import benchmarks

DEMAND_TYPES = {0, 1, 2, 3, 4, 5}
EVICT_TYPE = 8
OVERWRITE_TYPE = 9
USECOLS = ALL_TRACE_COLS       # full-row 읽어 완전-중복 제거(subset dedup 은 reuse 붕괴)

# hotness rank tier — 라인을 접근수 내림차순으로 세워 상위 몇 %를 hot 으로.
# (top1% / top10% / 나머지 90%).  rank 기반이라 access=1 동점에 강건.
TIER_CUTS = [0.01, 0.10]          # 누적 라인 비율 경계
TIER_NAMES = ["hottest 1%", "hot 1-10%", "cold 90%"]
TIER_COLORS = ["#9e0142", "#D55E00", "#bdbdbd"]


def _per_line_counts(trace_file: str) -> pd.DataFrame | None:
    """청크 누적 — 라인별 (access, evict, overwrite) 카운트."""
    acc_parts, ev_parts, ow_parts = [], [], []
    for chunk in trace_chunks(trace_file, usecols=USECOLS):
        chunk = chunk.drop_duplicates()          # full-row 완전-중복 제거
        line = chunk["addr"].to_numpy(dtype=np.uint64) >> 6
        typ = chunk["type"].to_numpy()
        dem = (chunk["type"].isin(DEMAND_TYPES) & (chunk["is_ptw"] == 0)).to_numpy()
        if dem.any():
            acc_parts.append(pd.Series(line[dem]).value_counts())
        ev = line[typ == EVICT_TYPE]
        ow = line[typ == OVERWRITE_TYPE]
        if len(ev):
            ev_parts.append(pd.Series(ev).value_counts())
        if len(ow):
            ow_parts.append(pd.Series(ow).value_counts())

    if not acc_parts:
        return None

    def _reduce(parts):
        if not parts:
            return pd.Series(dtype="int64")
        return pd.concat(parts).groupby(level=0).sum()

    access = _reduce(acc_parts)
    evict = _reduce(ev_parts)
    overw = _reduce(ow_parts)

    per = pd.DataFrame({"access": access}).fillna(0)
    per["evict"] = evict.reindex(per.index).fillna(0)
    per["overwrite"] = overw.reindex(per.index).fillna(0)
    per = per[per["access"] > 0]                       # demand 접근 있는 라인만
    return per.astype(np.int64)


def process_benchmark(bench: str, trace_dir: str, suffix: str):
    trace_file = f"{trace_dir}/{bench}_{suffix}_memory_trace.csv"
    if not os.path.exists(trace_file):
        return None
    print(f"[{bench}] eviction-churn ...", flush=True)
    try:
        per = _per_line_counts(trace_file)
        if per is None or per.empty:
            return None
        per = per.sort_values("access", ascending=False)
        n = len(per)
        total_evict = int((per["evict"] + per["overwrite"]).sum())
        total_access = int(per["access"].sum())

        # rank tier 경계 (라인 수 기준)
        k1 = max(1, int(np.ceil(n * TIER_CUTS[0])))
        k10 = max(k1, int(np.ceil(n * TIER_CUTS[1])))
        blocks = {"hottest 1%": per.iloc[:k1],
                  "hot 1-10%": per.iloc[k1:k10],
                  "cold 90%": per.iloc[k10:]}

        row = {"benchmark": bench, "n_lines": n,
               "total_evict+ow": total_evict, "total_access": total_access}
        for name, blk in blocks.items():
            a = int(blk["access"].sum())
            e = int((blk["evict"] + blk["overwrite"]).sum())
            # 접근-가중 tier churn (= tier miss rate)
            row[f"churn_{name}"] = round(e / a, 5) if a else 0.0
            # 라인 평균 churn (참고)
            cl = ((blk["evict"] + blk["overwrite"]) / blk["access"])
            row[f"churn_linemean_{name}"] = round(float(cl.mean()), 5) if len(blk) else 0.0
            row[f"access_share_{name}"] = round(a / total_access, 5) if total_access else 0.0
            row[f"evict_share_{name}"] = round(e / total_evict, 5) if total_evict else 0.0

        # top-10% (hottest+hot) eviction lift
        hot = per.iloc[:k10]
        a_hot = int(hot["access"].sum())
        e_hot = int((hot["evict"] + hot["overwrite"]).sum())
        acc_share = a_hot / total_access if total_access else 0.0
        evc_share = e_hot / total_evict if total_evict else 0.0
        row["hot10_evict_lift"] = round(evc_share / acc_share, 3) if acc_share else 0.0
        return pd.DataFrame([row])
    except Exception as e:  # noqa: BLE001
        print(f"[{bench}] error: {e}")
        return None


def plot_churn_by_tier(df: pd.DataFrame, out) -> None:
    apply_paper_style()
    benches = list(df.index)
    n = len(benches)
    fig, ax = plt.subplots(figsize=SIZE_WIDE)
    group_w = 0.8
    bar_w = group_w / len(TIER_NAMES)
    x0 = np.arange(n)
    for j, (name, col) in enumerate(zip(TIER_NAMES, TIER_COLORS)):
        vals = df[f"churn_{name}"].to_numpy()
        xpos = x0 - group_w / 2 + bar_w * (j + 0.5)
        ax.bar(xpos, vals, bar_w, color=col, label=name,
               edgecolor="white", linewidth=0.3)
    ax.set_xticks(x0)
    ax.set_xticklabels(benches, rotation=35, ha="right", fontsize=6)
    ax.set_ylabel("churn ratio  (evict+overwrite) / access")
    ax.set_title("Eviction churn by per-line hotness "
                 "(hot bar ~ cold bar = eviction ignores hotness)")
    ax.legend(frameon=False, ncol=3, fontsize=6, loc="upper left")
    ax.grid(axis="y", alpha=0.3)
    save_fig(fig, out, "churn_by_tier")


def plot_lift(df: pd.DataFrame, out) -> None:
    apply_paper_style()
    benches = list(df.index)
    x = np.arange(len(benches))
    fig, ax = plt.subplots(figsize=SIZE_WIDE)
    vals = df["hot10_evict_lift"].to_numpy()
    ax.bar(x, vals, 0.6, color=color_for(1), edgecolor="white", linewidth=0.3)
    ax.axhline(1.0, color="black", linestyle="--", linewidth=0.7)
    ax.text(len(benches) - 0.5, 1.02, "lift=1 (proportional)", ha="right",
            va="bottom", fontsize=6)
    ax.set_xticks(x)
    ax.set_xticklabels(benches, rotation=35, ha="right", fontsize=6)
    ax.set_ylabel("eviction lift of top-10% hot lines")
    ax.set_title("Hot-line eviction lift  (>1 = hot evicted more than its access share)")
    ax.grid(axis="y", alpha=0.3)
    save_fig(fig, out, "eviction_lift")


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("run_id", nargs="?", default="latest")
    args = ap.parse_args()

    trace_dir = str(memory_trace_dir(args.run_id))
    config = run_config(args.run_id)
    suffix = file_suffix_for_config(config)
    out = domain_out_dir("cacheline", "eviction-churn", args.run_id)
    out.mkdir(parents=True, exist_ok=True)

    with ProcessPoolExecutor() as ex:
        raw = list(ex.map(process_benchmark, benchmarks,
                          repeat(trace_dir), repeat(suffix)))
    parts = [r for r in raw if r is not None]
    if not parts:
        print("no usable benchmarks.", file=sys.stderr)
        sys.exit(1)

    df = pd.concat(parts, ignore_index=True).set_index("benchmark")
    df.to_csv(out / "churn.csv")
    plot_churn_by_tier(df, out)
    plot_lift(df, out)

    print("\n" + "=" * 72)
    show = [c for c in df.columns if c.startswith("churn_hot")
            or c.startswith("churn_cold") or c == "hot10_evict_lift"]
    print(df[show].to_string())
    print("=" * 72)
    print(f"wrote {out}/{{churn_by_tier,eviction_lift}}.png + churn.csv")


if __name__ == "__main__":
    main()
