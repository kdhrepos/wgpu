"""(고리 4) 완벽한 라인 배치를 가정한 한계선 — 평균 hop 감소량.

기본 owner 와 비교해, 라인별로 "가중 Manhattan 거리 합" 을 최소화하는 holder
칩렛을 고르면 평균 hop 이 얼마나 줄어드는지. 진짜 하드웨어로 구현 가능한지를
묻는 게 아니라 — 데이터 배치 자체로 끌어낼 수 있는 상한선을 보여 주기 위함.

라인별:
  cost(c) = Σ_r access_count_from_r[r] × Manhattan(c, r)
  oracle_home(line) = argmin_c cost(c)
  기본은 trace 의 owner 컬럼 그대로.

지표:
  - 평균 hop (기본 / 오라클)
  - 원격 비율 (oracle_home != requester 행의 비율)

성능: 칩렛 36 × 라인 N 의 벡터화 행렬곱. 라인 너무 많으면 access_count 상위
LINE_LIMIT(=200k) 만 사용 — sampling 으로 통계 안정.

입력: memory-traces/<bench>_<config>_memory_trace.csv (L2 미스만)
출력: _out/cacheline/oracle-headroom/<run_tag>/all.{png,pdf}
"""

from __future__ import annotations

import argparse
import sys
from pathlib import Path

import numpy as np
import pandas as pd
import matplotlib.pyplot as plt

from _paths import memory_trace_dir, domain_out_dir
from plot_style import apply_paper_style, color_for, save_fig, SIZE_DOUBLE
from motivation_common import (trace_chunks, filter_l2miss, ALL_TRACE_COLS,
                               manhattan_matrix, NUM_CHIPLETS_DEFAULT,
                               file_suffix_for_config)
from params import benchmarks


LINE_LIMIT = 200_000


def aggregate_requesters_per_line(path: Path) -> tuple[np.ndarray, np.ndarray] | None:
    """모든 L2 miss 행을 (addr_cl, chiplet) 카운트 행렬로 집계.

    반환: (mat[L, N] 가중치, default_owner[L])
    """
    chunks = []
    for chunk in trace_chunks(path, usecols=ALL_TRACE_COLS):
        chunk = chunk.drop_duplicates()          # full-row 완전-중복 제거
        chunk = filter_l2miss(chunk)
        if chunk.empty:
            continue
        addr_cl = chunk["addr"].to_numpy(dtype=np.uint64) >> 6
        chunks.append(pd.DataFrame({
            "addr": addr_cl,
            "chip": chunk["chiplet"].to_numpy(dtype=np.int16),
            "owner": chunk["owner"].to_numpy(dtype=np.int16),
        }))
    if not chunks:
        return None
    df = pd.concat(chunks, ignore_index=True)
    if df.empty:
        return None

    # 라인 누적 카운트
    total = df.groupby("addr").size().rename("n")
    # 상위 라인만 (가중치가 큰 것이 평균에 dominant)
    if len(total) > LINE_LIMIT:
        keep = total.nlargest(LINE_LIMIT).index
        df = df[df["addr"].isin(keep)]
        total = total.loc[keep]

    owners = df.groupby("addr")["owner"].first()
    counts = (df.groupby(["addr", "chip"]).size()
                .unstack(fill_value=0)
                .reindex(columns=np.arange(NUM_CHIPLETS_DEFAULT), fill_value=0))
    counts = counts.loc[total.index]
    mat = counts.to_numpy(dtype=np.float64)
    default_owner = owners.loc[total.index].to_numpy(dtype=np.int32)
    return mat, default_owner


def compute_headroom(mat: np.ndarray, default_owner: np.ndarray
                    ) -> dict:
    """mat[L, N] = 라인별 칩렛 c 의 접근수. 기본 vs oracle hop 평균."""
    dmat = manhattan_matrix()                          # [N, N]
    # cost[L, c] = mat @ dmat (c 가 home 일 때 라인 L 의 가중 hop)
    cost = mat @ dmat.astype(np.float64)               # [L, N]
    oracle_home = cost.argmin(axis=1)                  # [L]
    # 기본 hop 합 — Σ mat[L, r] × dmat[default_owner, r]
    base_hop = (mat * dmat[default_owner]).sum(axis=1)
    or_hop   = (mat * dmat[oracle_home]).sum(axis=1)
    total_acc = mat.sum(axis=1)
    base_mean = base_hop.sum() / max(total_acc.sum(), 1)
    or_mean   = or_hop.sum() / max(total_acc.sum(), 1)

    # 원격 비율 (요청과 home 같으면 local)
    base_remote = 0.0; or_remote = 0.0
    # vectorize: 각 (line, r) 에 대해 r != home 일 때 mat 누적
    cols = np.arange(NUM_CHIPLETS_DEFAULT)[None, :]
    base_mask = (cols != default_owner[:, None])
    or_mask   = (cols != oracle_home[:, None])
    base_remote = (mat * base_mask).sum() / max(mat.sum(), 1)
    or_remote   = (mat * or_mask).sum() / max(mat.sum(), 1)

    return dict(base_hop=base_mean, oracle_hop=or_mean,
                base_remote=base_remote, oracle_remote=or_remote,
                reduction_pct=(base_mean - or_mean) / base_mean * 100 if base_mean > 0 else 0.0,
                n_lines=int(mat.shape[0]),
                total_access=int(mat.sum()))


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--date", default="mesh-6x6/latest")
    args = ap.parse_args()

    td = memory_trace_dir(args.date)
    config_name = td.parent.parent.name
    suffix = file_suffix_for_config(config_name)
    rows = []
    for b in benchmarks:
        f = td / f"{b}_{suffix}_memory_trace.csv"
        if not f.exists():
            continue
        print(f"  {b} ...", flush=True)
        try:
            agg = aggregate_requesters_per_line(f)
        except Exception as e:
            print(f"    skip {b}: {e}")
            continue
        if agg is None:
            continue
        mat, default_owner = agg
        r = compute_headroom(mat, default_owner)
        r["benchmark"] = b
        rows.append(r)

    if not rows:
        print("no usable benchmarks.", file=sys.stderr)
        sys.exit(1)

    df = pd.DataFrame(rows).set_index("benchmark").sort_values("base_hop", ascending=False)

    apply_paper_style()
    fig, (ax_h, ax_r) = plt.subplots(1, 2, figsize=SIZE_DOUBLE)
    x = np.arange(len(df))
    w = 0.4
    ax_h.bar(x - w/2, df["base_hop"], w, color=color_for(0), label="기본")
    ax_h.bar(x + w/2, df["oracle_hop"], w, color=color_for(2), label="오라클")
    for i, pct in enumerate(df["reduction_pct"]):
        ax_h.text(x[i] + w/2, df["oracle_hop"].iloc[i], f"-{pct:.0f}%",
                  ha="center", va="bottom", fontsize=5, color="#333")
    ax_h.set_xticks(x); ax_h.set_xticklabels(df.index, rotation=40, ha="right", fontsize=6)
    ax_h.set_ylabel("L2 miss 가중 평균 hop")
    ax_h.set_title("기본 vs 오라클 배치 — 평균 hop")
    ax_h.legend(frameon=False, fontsize=6)
    ax_h.grid(axis="y", alpha=0.2)

    ax_r.bar(x - w/2, df["base_remote"] * 100, w, color=color_for(0), label="기본")
    ax_r.bar(x + w/2, df["oracle_remote"] * 100, w, color=color_for(2), label="오라클")
    ax_r.set_xticks(x); ax_r.set_xticklabels(df.index, rotation=40, ha="right", fontsize=6)
    ax_r.set_ylabel("원격 접근 비율 (%)")
    ax_r.set_title("기본 vs 오라클 — 원격 접근 비율")
    ax_r.legend(frameon=False, fontsize=6)
    ax_r.grid(axis="y", alpha=0.2)

    out = domain_out_dir("cacheline", "oracle-headroom", args.date)
    save_fig(fig, out, "all")
    df.to_csv(out / "headroom.csv")
    print(f"wrote {out}/all.{{png,pdf}} + headroom.csv")


if __name__ == "__main__":
    main()
