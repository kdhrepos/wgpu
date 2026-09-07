"""PC 별 hotness(pc-stats 신호로 분류) x footprint(같은 트레이스) -> tier 별 footprint.

지금까지 만든 두 조각을 하나로 합친다:
  - pc_hotness_from_stats.py 가 하던 일(reuse/sharing/hop -> pctable.go 결정
    트리로 tier 분류)을, 미리 뽑아둔 xlsx가 아니라 **이 트레이스에서 직접**
    재현한다 — pc_stats_l2miss_per_owner.py 와 동일 필터(rdma==True & type in
    [2,5]) 로 (owner,pc) 신호를 뽑고 pc 로 평균.
  - pc_footprint.py 가 하던 일(type in {0..5} 전체 demand 에서 (chip,line) 당
    argmax-count PC 를 exclusive owner 로 삼아 PC 별 footprint 산출)도 같은
    트레이스에서 재현.
  - 둘을 (benchmark,pc) 로 merge -> PC 하나가 어떤 tier 인지 + 그 PC 가 얼마나
    되는 footprint 를 쥐고 있는지 -> tier 별로 합산.

reuse/sharing 신호는 원본 스크립트처럼 트레이스 전체를 한 번에 로드한다(청크
스트리밍 안 함) — RDMA-miss 로 필터한 뒤의 행 수는 전체 demand 의 일부라 이
편이 pandas groupby 로 더 간단하고 빠르다. footprint 신호도 같은 in-memory
프레임에서 뽑아서 트레이스를 두 번 읽지 않는다. USECOLS 만 골라 읽으므로(6개
컬럼) 원본 CSV 전체보다는 훨씬 가볍지만, 그래도 벤치마크 하나당 GB 단위일 수
있음 — 기본 워커 4, 메모리 부족하면 -j 낮출 것.

classify 는 threshold / score(hop 없음, decoupled-cluster-size) / score(hop
포함, master 기본가중치) 세 가지 다 낸다 — pc_hotness_from_stats.py 와 동일
정의, 그 모듈에서 직접 import.

주의: drop_duplicates() 의 dedup 키에 "time" 을 반드시 포함시켰다 — 안 그러면
같은 PC 가 같은 주소를 같은 칩렛에서 서로 다른 시각에 정확히 같은 방식
(rw,type 등)으로 반복 접근했을 때 한 행으로 뭉개져 reuse/access_count 가
심하게 과소평가된다(합성 트레이스로 실제 재현·수정함 — 150회 재접근짜리 PC
가 1회로 뭉개졌었음). pc_footprint.py 는 청크마다 4컬럼(addr,pc,type,chiplet)
만 읽고 time 없이 dedup 하는데, 그쪽은 (chip,line) 당 owner PC 를 argmax
count 로 정하는 tie-break 에만 영향이 있고(라인 소유권 자체는 count>0 이면
바뀌지 않음) footprint 결과에 미치는 영향이 훨씬 좁아서 원본 그대로 뒀다 —
이 스크립트가 그 코드를 그대로 재현할 때도 마찬가지로 좁은 노출.

footprint-only PC(원격 요청이 한 번도 없어 PCTable 엔트리 자체가 없는 PC)는
tier 를 Cold 로 채운다 — 실제 시스템에서도 ReqFromInside 가 미등록 PC 에
Cold 를 반환하는 것과 동일.

출력 (xlsx, 두 시트):
  pc        benchmark, pc, reuse, sharing, hop_norm, owner_count,
            total_access_count, tier_threshold, tier_score_nohop,
            tier_score_hop, avg_footprint_MB, avg_shared_footprint_MB,
            avg_private_footprint_MB
  by_tier   (benchmark, classifier, tier) -> n_pc, total_avg_footprint_MB
            (long-form, 세 classifier 다 한 시트에)

사용:
  analyze/run.sh pc-stats/pc_hotness_footprint.py <run-id> [-j 4]
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

from _paths import memory_trace_dir, run_config, run_tag
from motivation_common import file_suffix_for_config
from pc_hotness import mesh_dims_from_config, manhattan_hop
from pc_hotness_from_stats import (
    threshold_classify, score_classify, SCORE_W_NOHOP, SCORE_W_HOP,
    MIN_REQ_COUNT, TIER_ORDER,
)
from params import benchmarks

DEMAND_TYPES = [0, 1, 2, 3, 4, 5]
RDMA_MISS_TYPES = [2, 5]
LINE_BYTES = 64
LINE_MB = LINE_BYTES / (1024.0 ** 2)

# "time" stays in USECOLS purely as a dedup tie-breaker (dropped after) — without
# it, drop_duplicates() collapses genuinely repeated accesses (same pc/addr/
# owner/chiplet/rdma/type at different times) into one row, corrupting every
# count-based signal below (reuse, access_count, tie-breaking in footprint
# ownership). Mirrors pc_stats_l2miss_per_owner.py, which loads the full CSV
# (time included) before dropping duplicates and only subsets columns after.
USECOLS = ["time", "pc", "addr", "owner", "chiplet", "rdma", "type"]
DTYPES = {"pc": "uint64", "addr": "uint64", "owner": "int32",
         "chiplet": "int32", "rdma": "bool", "type": "int8"}


def _reuse_sharing_tiers(df: pd.DataFrame, n_side: int) -> pd.DataFrame:
    """pc_stats_l2miss_per_owner.py(group_by='owner') 재현 + tier 분류."""
    cols = ["pc", "reuse", "sharing", "hop_norm", "owner_count", "total_access_count",
           "tier_threshold", "tier_score_nohop", "tier_score_hop"]
    rdma = df[df["rdma"] & df["type"].isin(RDMA_MISS_TYPES) & (df["pc"] != 0)]
    if rdma.empty:
        return pd.DataFrame(columns=cols)

    addr_chip = rdma.groupby(["pc", "addr"])["chiplet"].nunique().reset_index(name="chiplet_count")
    avg_sharing = addr_chip.groupby("pc")["chiplet_count"].mean().rename("sharing")

    hop = manhattan_hop(rdma["owner"].to_numpy(), rdma["chiplet"].to_numpy(), n_side)
    rdma = rdma.assign(hop=hop)

    per_owner = rdma.groupby(["owner", "pc"]).agg(
        access_count=("addr", "count"), unique_addrs=("addr", "nunique"),
        avg_hop=("hop", "mean")).reset_index()
    per_owner["access_per_addr"] = per_owner["access_count"] / per_owner["unique_addrs"]

    pc_agg = per_owner.groupby("pc").agg(
        owner_count=("owner", "nunique"),
        reuse=("access_per_addr", "mean"),
        avg_hop=("avg_hop", "mean"),
        total_access_count=("access_count", "sum"),
    ).reset_index()
    pc_agg = pc_agg.merge(avg_sharing, on="pc", how="left")
    diam = max(2 * (n_side - 1), 2)
    pc_agg["hop_norm"] = np.clip((pc_agg["avg_hop"] - 1) / (diam - 1), 0, 1)

    reuse = pc_agg["reuse"].to_numpy(dtype=np.float64)
    sharing = pc_agg["sharing"].to_numpy(dtype=np.float64)
    hop_norm = pc_agg["hop_norm"].to_numpy(dtype=np.float64)

    tier_threshold = threshold_classify(reuse, sharing)
    tier_nohop, _ = score_classify(reuse, sharing, np.zeros_like(reuse), SCORE_W_NOHOP)
    tier_hop, _ = score_classify(reuse, sharing, hop_norm, SCORE_W_HOP)

    gated = pc_agg["total_access_count"].to_numpy() < MIN_REQ_COUNT
    pc_agg["tier_threshold"] = np.where(gated, "Cold", tier_threshold)
    pc_agg["tier_score_nohop"] = np.where(gated, "Cold", tier_nohop)
    pc_agg["tier_score_hop"] = np.where(gated, "Cold", tier_hop)

    return pc_agg[cols]


def _footprint(df: pd.DataFrame, n_chiplets: int) -> pd.DataFrame:
    """pc_footprint.py 재현: (chip,line) 당 argmax-count PC 를 exclusive owner 로."""
    fp = df[df["type"].isin(DEMAND_TYPES)].copy()
    if fp.empty:
        return pd.DataFrame(columns=["pc", "avg_footprint_MB",
                                     "avg_shared_footprint_MB", "avg_private_footprint_MB"])
    fp["line"] = fp["addr"].to_numpy(dtype=np.uint64) >> np.uint64(6)

    pc_line_chip = fp.groupby(["pc", "line", "chiplet"]).size().rename("n").reset_index()
    pc_line_chip = pc_line_chip.sort_values(
        ["chiplet", "line", "n", "pc"], ascending=[True, True, False, True], kind="mergesort")
    owner_pc = pc_line_chip.drop_duplicates(subset=["chiplet", "line"], keep="first")

    nchip_per_line = fp.groupby("line")["chiplet"].nunique()
    shared_idx = nchip_per_line.index[nchip_per_line >= 2]
    owner_pc = owner_pc.assign(is_shared=owner_pc["line"].isin(shared_idx))

    per_chip_pc = owner_pc.groupby(["chiplet", "pc"]).agg(
        owned_lines=("line", "size"), shared_lines=("is_shared", "sum")).reset_index()
    per_chip_pc["private_lines"] = per_chip_pc["owned_lines"] - per_chip_pc["shared_lines"]

    pc_fp = per_chip_pc.groupby("pc").agg(
        total_owned_lines=("owned_lines", "sum"),
        total_shared_lines=("shared_lines", "sum"),
        total_private_lines=("private_lines", "sum")).reset_index()
    pc_fp["avg_footprint_MB"] = pc_fp["total_owned_lines"] / n_chiplets * LINE_MB
    pc_fp["avg_shared_footprint_MB"] = pc_fp["total_shared_lines"] / n_chiplets * LINE_MB
    pc_fp["avg_private_footprint_MB"] = pc_fp["total_private_lines"] / n_chiplets * LINE_MB
    return pc_fp[["pc", "avg_footprint_MB", "avg_shared_footprint_MB", "avg_private_footprint_MB"]]


def process_benchmark(bench: str, trace_dir: str, suffix: str, n_chiplets: int, n_side: int):
    trace_file = f"{trace_dir}/{bench}_{suffix}_memory_trace.csv"
    if not os.path.exists(trace_file):
        return None
    print(f"[{bench}] pc hotness x footprint ...", flush=True)
    try:
        df = pd.read_csv(trace_file, skipinitialspace=True, usecols=USECOLS, dtype=DTYPES)
        df = df.drop_duplicates()

        tiers = _reuse_sharing_tiers(df, n_side)
        fp = _footprint(df, n_chiplets)

        merged = tiers.merge(fp, on="pc", how="outer")
        for c in ("tier_threshold", "tier_score_nohop", "tier_score_hop"):
            merged[c] = merged[c].fillna("Cold")
        for c in ("reuse", "sharing", "hop_norm", "owner_count", "total_access_count",
                 "avg_footprint_MB", "avg_shared_footprint_MB", "avg_private_footprint_MB"):
            merged[c] = merged[c].fillna(0)
        merged.insert(0, "benchmark", bench)
        return merged
    except Exception as e:  # noqa: BLE001
        print(f"[{bench}] error: {e}")
        return None


def by_tier(all_pc: pd.DataFrame) -> pd.DataFrame:
    rows = []
    for classifier in ("tier_threshold", "tier_score_nohop", "tier_score_hop"):
        g = all_pc.groupby(["benchmark", classifier]).agg(
            n_pc=("pc", "size"),
            total_avg_footprint_MB=("avg_footprint_MB", "sum")).reset_index()
        g = g.rename(columns={classifier: "tier"})
        g.insert(1, "classifier", classifier)
        rows.append(g)
    out = pd.concat(rows, ignore_index=True)
    out["tier"] = pd.Categorical(out["tier"], categories=TIER_ORDER, ordered=True)
    return out.sort_values(["classifier", "benchmark", "tier"])


def main():
    ap = argparse.ArgumentParser(description=__doc__,
                                 formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("run_id", nargs="?", default="mesh-6x6/latest")
    ap.add_argument("--out", default=None, help="출력 xlsx 경로")
    ap.add_argument("-j", "--workers", type=int, default=4,
                    help="벤치마크 병렬 워커 수 (기본 4; 트레이스 전체 로드라 메모리 부족하면 낮출 것)")
    args = ap.parse_args()

    trace_dir = str(memory_trace_dir(args.run_id))
    config = run_config(args.run_id)
    suffix = file_suffix_for_config(config)
    n_side, n_chiplets = mesh_dims_from_config(config)

    if args.workers <= 1:
        raw = [process_benchmark(b, trace_dir, suffix, n_chiplets, n_side) for b in benchmarks]
    else:
        with ProcessPoolExecutor(max_workers=args.workers) as ex:
            raw = list(ex.map(process_benchmark, benchmarks,
                              repeat(trace_dir), repeat(suffix), repeat(n_chiplets), repeat(n_side)))

    parts = [r for r in raw if r is not None]
    if not parts:
        print("no usable benchmarks", file=sys.stderr)
        sys.exit(1)

    all_pc = pd.concat(parts, ignore_index=True)
    tier_summary = by_tier(all_pc)

    out_path = Path(args.out) if args.out else (
        Path.home() / "mgvm" / "analyze" / "_out" / "pc-stats" / "pc_hotness_footprint" /
        run_tag(args.run_id) / f"{config}_pc_hotness_footprint.xlsx")
    out_path.parent.mkdir(parents=True, exist_ok=True)

    with pd.ExcelWriter(out_path) as xw:
        all_pc.to_excel(xw, index=False, sheet_name="pc")
        tier_summary.to_excel(xw, index=False, sheet_name="by_tier")

    print(f"wrote {out_path}  (pc rows={len(all_pc)})")
    print()
    print("total_avg_footprint_MB by tier (tier_score_hop):")
    piv = (tier_summary[tier_summary["classifier"] == "tier_score_hop"]
          .pivot(index="benchmark", columns="tier", values="total_avg_footprint_MB")
          .reindex(columns=TIER_ORDER).fillna(0.0))
    print(piv.to_string())


if __name__ == "__main__":
    main()
