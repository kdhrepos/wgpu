"""(고리 1a) 웨이퍼 규모에서 네트워크가 병목임을 보이는 100% 누적 막대.

각 벤치마크에 대해 실행 시간(=avg_last_WG_finish_time, 칩렛 평균)을 1 로 잡고
다음 세 조각으로 분해해 시각화한다.

  - 원격·NoC 대기 (remote): Σ_c (incoming_trans_count × incoming_trans_latency) 의 칩렛 평균.
                            ChipRDMA 가 본 incoming 트랜잭션 누적 대기 시간.
  - 로컬 접근 (local):       L2 히트가 부담한 시간 — incoming 데이터-트래픽이 아닌 부분.
                            여기서는 (1 - L2 평균 미스율) × (실행시간 − remote) 로 근사.
  - 연산 활동 (compute):     나머지.

세 조각의 절대값은 근사라 결과는 100% 정규화된 막대 한 줄로만 보여 준다 — motivation
용도라 정확한 시간 분해가 아니라 "원격 대기가 큰 비중을 차지한다" 정도면 충분.

입력
----
  reports/<bench>_<config>.csv (long form: where, what, value)

출력
----
  _out/network/bottleneck-breakdown/<run_tag>/all.{png,pdf}
"""

from __future__ import annotations

import argparse
import sys

import numpy as np
import pandas as pd
import matplotlib.pyplot as plt

from _paths import report_dir, run_root, domain_out_dir
from plot_style import apply_paper_style, color_for, save_fig, SIZE_DOUBLE
from motivation_common import load_report, get_scalar, file_suffix_for_config
from params import benchmarks


def per_bench_breakdown(rep_df: pd.DataFrame) -> dict | None:
    # 실행 시간 — 칩렛별 avg_last_WG_finish_time 의 max (전체 실행이 끝나는 시점).
    wg = rep_df[rep_df["what"] == "avg_last_WG_finish_time"]
    if wg.empty:
        return None
    exec_time = float(wg["value"].max())
    if exec_time <= 0:
        return None

    # 원격 대기 — ChipRDMA 의 incoming_trans_count × incoming_trans_latency 누계,
    # 칩렛 평균(36 칩렛으로 나눠 wall-clock 평균 대기를 흉내).
    rd = rep_df[rep_df["where"].str.contains("ChipRDMA", regex=False) &
                rep_df["where"].str.contains("chiplet_", regex=False)]
    cnt = rd[rd["what"] == "incoming_trans_count"].set_index("where")["value"]
    lat = rd[rd["what"] == "incoming_trans_latency"].set_index("where")["value"]
    if cnt.empty or lat.empty:
        return None
    common = cnt.index.intersection(lat.index)
    remote_wait = float((cnt.loc[common] * lat.loc[common]).sum()) / 36.0
    remote_wait = min(remote_wait, exec_time)  # 상한 클램프

    # L2 평균 미스율 — chiplet_XX l2-total-miss-rate 의 칩렛 평균.
    miss = rep_df[(rep_df["what"] == "l2-total-miss-rate") &
                  rep_df["where"].str.match(r"chiplet_\d+", na=False)]
    miss_rate = float(miss["value"].mean()) if not miss.empty else 0.05

    remain = max(exec_time - remote_wait, 0.0)
    local_wait = remain * miss_rate          # L2 미스가 차지(but RDMA 제외)
    compute = remain - local_wait

    return dict(remote=remote_wait, local=local_wait, compute=compute, total=exec_time)


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--date", default="mesh-6x6/latest", help="run id (config/timestamp).")
    args = ap.parse_args()

    rd = report_dir(args.date)
    config_name = rd.parent.parent.name   # 예: 'mesh-6x6'
    suffix = file_suffix_for_config(config_name)
    print(f"Reading reports from {rd}")

    rows = []
    for b in benchmarks:
        f = rd / f"{b}_{suffix}.csv"
        if not f.exists():
            continue
        bd = per_bench_breakdown(load_report(f))
        if bd is None:
            print(f"  skip {b}: missing metrics")
            continue
        total = bd["total"] or 1.0
        rows.append({
            "benchmark": b,
            "compute_frac": bd["compute"] / total,
            "local_frac":   bd["local"] / total,
            "remote_frac":  bd["remote"] / total,
        })

    if not rows:
        print("no usable benchmarks found.", file=sys.stderr)
        sys.exit(1)

    df = pd.DataFrame(rows).set_index("benchmark").sort_values("remote_frac")

    apply_paper_style()
    fig, ax = plt.subplots(figsize=SIZE_DOUBLE)
    x = np.arange(len(df))
    width = 0.7

    c_compute = color_for(2)   # 초록
    c_local   = color_for(0)   # 파랑
    c_remote  = color_for(1)   # 주황

    ax.bar(x, df["compute_frac"], width, color=c_compute, label="연산 활동(compute)")
    ax.bar(x, df["local_frac"], width, bottom=df["compute_frac"],
           color=c_local, label="로컬 접근")
    ax.bar(x, df["remote_frac"], width,
           bottom=df["compute_frac"] + df["local_frac"],
           color=c_remote, label="원격·NoC 대기")

    ax.set_ylim(0, 1.0)
    ax.set_xticks(x)
    ax.set_xticklabels(df.index, rotation=40, ha="right")
    ax.set_ylabel("실행 시간 비중")
    ax.set_title("벤치마크별 실행 시간 분해 — 원격 대기 비중")
    ax.legend(loc="upper left", ncol=3, frameon=False, bbox_to_anchor=(0, 1.18))
    ax.grid(axis="y", alpha=0.18)

    out = domain_out_dir("network", "bottleneck-breakdown", args.date)
    save_fig(fig, out, "all")
    df.to_csv(out / "breakdown.csv")
    print(f"wrote {out}/all.png + .pdf + breakdown.csv")


if __name__ == "__main__":
    main()
