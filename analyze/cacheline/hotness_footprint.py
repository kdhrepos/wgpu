"""벤치마크별 hotness tier(Cold/Warm/Hot/Global) footprint — memory trace 기반.

`shared_private_capacity.py` 의 변형: 2-way(shared/private) 대신 pctable.go 의
thresholdClassify 를 그대로 미러링한 4-tier 로 캐시라인을 나눈다.

신호 (전체 lifetime, 청크 스트리밍 누적):
  reuse   = 그 캐시라인(addr>>6) 의 전체 demand 접근 횟수(type 0-5, is_ptw==0).
            pctable.go 의 avgPerLine = ReqCount/unique 에서 캐시라인 하나만 볼 때
            unique==1 이 되는 특수형과 동치 — 근사가 아니라 정확히 같은 값.
  sharing = 그 라인을 실제로 만진 distinct chiplet 수.  마찬가지로
            tuples/unique 의 unique==1 특수형과 동치.
  hop_norm= 그 라인을 만진 (owner,chip) presence 쌍 위의 비가중 평균 hop 을
            [0,1] 정규화.  access-count 가중 평균이 아님(2차 패스가 필요해 생략)
            — tier 판정(threshold_classify)에는 영향 없고 참고용 score 축에만
            반영된다.

분류는 `pc_hotness.classify_signals` (rdma/pctable.go 의 ClassifierThreshold 를
재현한 공유 헬퍼, pc_hotness_signature.py / premature_hot_eviction.py 와 동일
정의) 를 그대로 재사용 — footprint_clamp 는 기본 OFF(라인 하나짜리 unique=1 이라
clamp 조건이 사실상 항상 거짓이 되어 무의미).

출력:
  ~/mgvm/figures/hotness-footprint/<config>-<run_tag>/
      hotness_footprint_stacked.png   (Cold/Warm/Hot/Global stacked bar)
      per_benchmark.csv, per_chiplet_hotness.csv

사용:
  analyze/run.sh cacheline/hotness_footprint.py <run-id>
  analyze/run.sh cacheline/hotness_footprint.py <run-id> -j 8 --footprint-clamp
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
from motivation_common import file_suffix_for_config, trace_chunks, ALL_TRACE_COLS
from pc_hotness import MeshGeom, mesh_dims_from_config, manhattan_hop, classify_signals, TIER_ORDER, TIER_COLOR
from shared_private_capacity import plot_stacked, LINE_BYTES
from params import benchmarks

DEMAND_TYPES = np.array([0, 1, 2, 3, 4, 5], dtype=np.int8)
USECOLS = ALL_TRACE_COLS

FIG_ROOT = Path.home() / "mgvm" / "figures" / "hotness-footprint"
FIG_FORMATS = ("png",)

TIER_HATCH = {"Cold": None, "Warm": "///", "Hot": "...", "Global": "xxx"}


# ── 단일 청크 패스: 라인별 access_count + unique (line,chip,owner) 표 ─────────
def _accumulate(trace_file: str) -> tuple[pd.Series, pd.DataFrame]:
    access_parts: list[pd.Series] = []
    uniq_parts: list[pd.DataFrame] = []
    for chunk in trace_chunks(trace_file, usecols=USECOLS):
        chunk = chunk.drop_duplicates()               # 완전-중복 로그행만 제거(reuse 보존)
        types = chunk["type"].to_numpy(dtype=np.int8)
        is_ptw = chunk["is_ptw"].to_numpy(dtype=np.int8)
        is_d = np.isin(types, DEMAND_TYPES) & (is_ptw == 0)
        if not is_d.any():
            continue
        line = chunk["addr"].to_numpy(dtype=np.uint64)[is_d] >> np.uint64(6)
        chip = chunk["chiplet"].to_numpy(dtype=np.int64)[is_d]
        owner = chunk["owner"].to_numpy(dtype=np.int64)[is_d]

        access_parts.append(pd.Series(line).value_counts())
        uniq_parts.append(pd.DataFrame({"line": line, "chip": chip, "owner": owner})
                          .drop_duplicates())

    if not access_parts:
        return pd.Series(dtype="int64"), pd.DataFrame(columns=["line", "chip", "owner"])

    access = pd.concat(access_parts).groupby(level=0).sum()
    uniq = pd.concat(uniq_parts, ignore_index=True).drop_duplicates()
    return access, uniq


def _line_signals(access: pd.Series, uniq: pd.DataFrame, geom: MeshGeom) -> pd.DataFrame:
    """라인별 (reuse, sharing, hop_norm) — classify_signals 입력."""
    sharing = uniq.groupby("line")["chip"].nunique()
    hop = manhattan_hop(uniq["owner"].to_numpy(), uniq["chip"].to_numpy(), geom.n_side)
    hop_avg = pd.Series(hop, index=uniq.index).groupby(uniq["line"]).mean()

    lines = sharing.index
    diam = geom.diameter if geom.diameter > 1 else 2
    sig = pd.DataFrame(index=lines)
    sig["reuse"] = access.reindex(lines, fill_value=0).astype(np.int64)
    sig["sharing"] = sharing.astype(np.int64)
    sig["hop_norm"] = np.clip((hop_avg.reindex(lines, fill_value=1.0) - 1) / (diam - 1), 0, 1)
    return sig


def _tier_stats(prefix: str, per_chip: pd.Series) -> dict:
    cap_mb = per_chip * LINE_BYTES / (1024.0 ** 2)
    return {
        f"{prefix}_lines_total": int(per_chip.sum()),
        f"{prefix}_capacity_mb_total": float(cap_mb.sum()),
        f"avg_{prefix}_lines": float(per_chip.mean()),
        f"avg_{prefix}_capacity_mb": float(cap_mb.mean()),
        f"std_{prefix}_capacity_mb": float(cap_mb.std()),
    }


# ── per-benchmark worker ─────────────────────────────────────────────────────
def process_benchmark(bench: str, trace_dir: str, suffix: str, geom: MeshGeom, footprint_clamp: bool):
    trace_file = f"{trace_dir}/{bench}_{suffix}_memory_trace.csv"
    if not os.path.exists(trace_file):
        return None
    print(f"[{bench}] hotness footprint ...", flush=True)
    try:
        access, uniq = _accumulate(trace_file)
        if uniq.empty:
            return None

        sig = _line_signals(access, uniq, geom)
        classified = classify_signals(sig, geom, footprint_clamp=footprint_clamp)
        tier_of_line = classified["tier"]              # index=line -> tier str

        uniq = uniq.merge(tier_of_line.rename("tier"), left_on="line", right_index=True)
        chip_idx = pd.RangeIndex(geom.num, name="chiplet")

        row = {"benchmark": bench,
               "mean_reuse": float(sig["reuse"].mean()),
               "mean_sharing": float(sig["sharing"].mean()),
               "mean_score": float(classified["score"].mean())}
        per_chip_cols = {}
        for tier in TIER_ORDER:
            prefix = tier.lower()
            touches = uniq[uniq["tier"] == tier]
            # tier 는 각 chiplet 이 "실제로 만진" 라인만 그 chiplet 몫으로 잡는다
            # (shared_private_capacity.py 와 동일 규약) → shared/hot 라인은 그걸
            # 만진 chiplet 수만큼 avg_*_lines 합에 중복 카운트된다.
            per_chip = touches.groupby("chip").size().reindex(chip_idx, fill_value=0)
            per_chip_cols[f"{prefix}_lines"] = per_chip
            row.update(_tier_stats(prefix, per_chip))

            lines_global = int((tier_of_line == tier).sum())
            row[f"{prefix}_lines_global"] = lines_global
            row[f"{prefix}_capacity_mb_global"] = lines_global * LINE_BYTES / (1024.0 ** 2)

        per_chip_df = pd.DataFrame(per_chip_cols)
        for tier in TIER_ORDER:
            p = tier.lower()
            per_chip_df[f"{p}_capacity_mb"] = per_chip_df[f"{p}_lines"] * LINE_BYTES / (1024.0 ** 2)
        per_chip_df["benchmark"] = bench
        return row, per_chip_df.reset_index()
    except Exception as e:  # noqa: BLE001
        print(f"[{bench}] error: {e}")
        return None


def run_one(run_id: str, workers: int, footprint_clamp: bool):
    trace_dir = str(memory_trace_dir(run_id))
    config = run_config(run_id)
    suffix = file_suffix_for_config(config)
    n_side, n_chiplets = mesh_dims_from_config(config)
    geom = MeshGeom(n_side=n_side, num=n_chiplets)

    if workers <= 1:
        raw = [process_benchmark(b, trace_dir, suffix, geom, footprint_clamp) for b in benchmarks]
    else:
        with ProcessPoolExecutor(max_workers=workers) as ex:
            raw = list(ex.map(process_benchmark, benchmarks,
                              repeat(trace_dir), repeat(suffix), repeat(geom), repeat(footprint_clamp)))

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
    df["total_capacity_mb"] = sum(df[f"avg_{t.lower()}_capacity_mb"] for t in TIER_ORDER)
    df = df.sort_values("total_capacity_mb")
    per_chip_df = pd.concat(per_chip_frames, ignore_index=True)
    return config, df, per_chip_df


STACK_HOTNESS = [
    {"col": f"avg_{t.lower()}_capacity_mb", "label": t, "color": TIER_COLOR[t], "hatch": TIER_HATCH[t]}
    for t in TIER_ORDER
]


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("run_id", nargs="?", default="mesh-6x6/latest")
    ap.add_argument("-j", "--workers", type=int, default=4,
                    help="benchmark parallel workers (default 4, 1=sequential)")
    ap.add_argument("--footprint-clamp", action="store_true",
                    help="apply pctable.go's per-tier footprint clamp (default off — "
                         "meaningless at single-line granularity, unique==1 always)")
    args = ap.parse_args()

    config, df, per_chip_df = run_one(args.run_id, args.workers, args.footprint_clamp)
    if df is None:
        print("no usable benchmarks", file=sys.stderr)
        sys.exit(1)

    tag = run_tag(args.run_id)
    out = FIG_ROOT / f"{config}-{tag}"
    out.mkdir(parents=True, exist_ok=True)
    df.to_csv(out / "per_benchmark.csv")
    per_chip_df.to_csv(out / "per_chiplet_hotness.csv", index=False)
    plot_stacked(df, STACK_HOTNESS, out, "hotness_footprint_stacked")

    print(f"\n[{config}] wrote {out}")
    show = [f"avg_{t.lower()}_capacity_mb" for t in TIER_ORDER] + ["total_capacity_mb"]
    print(df[show].to_string())


if __name__ == "__main__":
    main()
