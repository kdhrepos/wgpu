"""tier 별 footprint — pc_hotness_per_time.py 의 windowed(진짜 pctable.go 방식)
분류로 시간-비율 가중해서 pc_footprint 에 적용.

문제: `pc_hotness_footprint.py`는 PC 하나의 lifetime 누적값(reuse=cum ReqCount/
cum UniqueAddrs) 딱 하나로 그 PC 의 footprint 전부를 한 tier 에 몰아줬다. 이
비율은 시간에 따라 단조증가한다 — UniqueAddrs(그 PC 가 만지는 distinct 캐시라인
수)는 보통 초반에 포화되는데 ReqCount(총 요청수)는 시뮬 끝까지 계속 늘어나기
때문. 그래서 "lifetime 끝 값"은 그 PC 가 도달하는 사실상 최댓값이고, 실제
시뮬레이션이 도는 대부분의 시간 동안엔 훨씬 낮은 tier 로 분류돼 있었을 것 —
Global 이 실제보다 과대평가됨(사용자 피드백으로 확인).

고침: pc_hotness_per_time.py 의 windowed classify()(매 window 마다 그 시점까지
누적된 reuse/sharing/windowed-WS 로 재분류 — 실제 엔진의 Decay() 재분류를 그대로
이식한 것, minReqCount 워밍업 게이트도 "그 시점 기준"으로 정확히 적용됨)를 그대로
재사용해서, PC 하나가 **활성 window 중 몇 %를 각 tier 에서 보냈는지** 구한다.
그 시간-비율로 pc_footprint 의 footprint_MB 를 tier 별로 쪼개 담는다 — "이 PC
는 Global 이었다" 대신 "이 PC 는 생애의 12%만 Global 이었다" 만큼만 Global 몫에
반영된다.

NonShared(Warm_NS/Hot_NS)와 Streaming 은 Cold 로 접는다 — 이전 결정
(pc_hotness_from_stats.py)과 동일 규약: 분산(distribution) 대상이 아닌 tier를
따로 등급 매겨봐야 이후 분석에서 쓸 데가 없음.

성능: 트레이스를 두 번 읽는다 — (1) pc_hotness_per_time._load() 로 RDMA-미스
행만 로드(windowed 분류용), (2) pc_hotness_footprint._footprint() 로 전체 demand
행 로드(footprint용). 합치면 더 빠르겠지만 이미 두 스크립트에서 각자 검증된
로직을 그대로 재사용하는 쪽이 새 버그 낼 위험이 적어서 이렇게 뒀다.

사용:
  analyze/run.sh pc-stats/pc_hotness_footprint_windowed.py <run-id> \
      [--window-ns 10000] [--ws-window 7] [--min-req 100] [-j 2]
"""

from __future__ import annotations

import argparse
import os
import sys
from concurrent.futures import ProcessPoolExecutor
from itertools import repeat
from pathlib import Path

import pandas as pd

from _paths import memory_trace_dir, run_config, run_tag
from motivation_common import file_suffix_for_config
from pc_hotness import mesh_dims_from_config
from pc_hotness_per_time import (
    _load as _load_windowed, _owner_cumulative, _pc_timeseries,
    STREAMING, COLD, WARM_NS, HOT_NS, WARM, HOT, GLOBAL,
)
from pc_hotness_footprint import _footprint
from params import benchmarks

TIER_ORDER = ["Cold", "Warm", "Hot", "Global"]
TIER_OF_ENUM = {STREAMING: "Cold", COLD: "Cold", WARM_NS: "Cold", HOT_NS: "Cold",
                WARM: "Warm", HOT: "Hot", GLOBAL: "Global"}

USECOLS_FP = ["time", "pc", "addr", "owner", "chiplet", "rdma", "type"]
DTYPES_FP = {"pc": "uint64", "addr": "uint64", "owner": "int32",
            "chiplet": "int32", "rdma": "bool", "type": "int8"}


def _time_fraction_by_tier(pc_win: pd.DataFrame) -> pd.DataFrame:
    """pc_win: columns win, pc, hotness(enum int, one row per active window).
    -> index=pc, columns=TIER_ORDER, 그 PC 의 활성 window 중 각 tier 비율(합=1)."""
    tier = pc_win["hotness"].map(TIER_OF_ENUM)
    counts = pc_win.groupby([pc_win["pc"], tier]).size().unstack(fill_value=0)
    counts = counts.reindex(columns=TIER_ORDER, fill_value=0)
    return counts.div(counts.sum(axis=1), axis=0)


def process_benchmark(bench: str, trace_dir: str, suffix: str, n_side: int, n_chiplets: int,
                      window_ns: int, ws_window: int, min_req: int):
    trace_file = f"{trace_dir}/{bench}_{suffix}_memory_trace.csv"
    if not os.path.exists(trace_file):
        return None
    print(f"[{bench}] windowed hotness x footprint ...", flush=True)
    try:
        # 1) windowed classification -- 그 시점까지 누적된 신호로 매 window 재분류
        df_w = _load_windowed(trace_file, n_side, window_ns)
        if df_w is None or df_w.empty:
            return None
        hop_ref = max(1, 2 * (n_side - 1))
        base = _owner_cumulative(df_w)
        pc_win = _pc_timeseries(df_w, base, n_side, n_side, ws_window, min_req, hop_ref, window_ns)
        if pc_win.empty:
            return None
        frac = _time_fraction_by_tier(pc_win)

        # 2) footprint -- 전체 demand 행에서 (chip,line) 당 argmax-count PC 를 exclusive owner 로
        df_fp = pd.read_csv(trace_file, skipinitialspace=True, usecols=USECOLS_FP, dtype=DTYPES_FP)
        df_fp = df_fp.drop_duplicates()
        fp = _footprint(df_fp, n_chiplets)

        merged = fp.merge(frac.reset_index(), on="pc", how="left")
        # footprint 는 있는데 RDMA 요청이 아예 없어 windowed 분류 자체가 없는 PC
        # (한 chiplet 안에서만 도는 순수 로컬 PC) -> 실제로도 PCTable 엔트리가
        # 없으니 100% Cold.
        for t in TIER_ORDER:
            merged[t] = merged[t].fillna(1.0 if t == "Cold" else 0.0)
        for t in TIER_ORDER:
            merged[f"footprint_MB_{t}"] = merged["avg_footprint_MB"] * merged[t]

        merged.insert(0, "benchmark", bench)
        merged = merged.rename(columns={t: f"time_frac_{t}" for t in TIER_ORDER})
        return merged
    except Exception as e:  # noqa: BLE001
        print(f"[{bench}] error: {e}")
        return None


def by_tier(all_pc: pd.DataFrame) -> pd.DataFrame:
    rows = []
    for t in TIER_ORDER:
        g = all_pc.groupby("benchmark")[f"footprint_MB_{t}"].sum().rename("total_avg_footprint_MB").reset_index()
        g.insert(1, "tier", t)
        rows.append(g)
    out = pd.concat(rows, ignore_index=True)
    out["tier"] = pd.Categorical(out["tier"], categories=TIER_ORDER, ordered=True)
    return out.sort_values(["benchmark", "tier"])


def main():
    ap = argparse.ArgumentParser(description=__doc__,
                                 formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("run_id", nargs="?", default="mesh-6x6/latest")
    ap.add_argument("--window-ns", type=int, default=50000,
                    help="pctable.go 의 samplingPeriod(Decay 주기)와 동일 단위, 기본 10000 (=10000 cycles @ 1GHz)")
    ap.add_argument("--ws-window", type=int, default=7,
                    help="footprint clamp 용 trailing window 수 (= wsDecaySampleInterval), 기본 7")
    ap.add_argument("--min-req", type=int, default=100,
                    help="minReqCountForClassification, 기본 100 (pctable.go 기본값)")
    ap.add_argument("--out", default=None)
    ap.add_argument("-j", "--workers", type=int, default=2,
                    help="벤치마크 병렬 워커 수 (기본 2 -- 트레이스 두 번 읽고 windowed 분류가 무거움)")
    args = ap.parse_args()

    trace_dir = str(memory_trace_dir(args.run_id))
    config = run_config(args.run_id)
    suffix = file_suffix_for_config(config)
    n_side, n_chiplets = mesh_dims_from_config(config)

    if args.workers <= 1:
        raw = [process_benchmark(b, trace_dir, suffix, n_side, n_chiplets,
                                 args.window_ns, args.ws_window, args.min_req)
              for b in benchmarks]
    else:
        with ProcessPoolExecutor(max_workers=args.workers) as ex:
            raw = list(ex.map(process_benchmark, benchmarks,
                              repeat(trace_dir), repeat(suffix), repeat(n_side), repeat(n_chiplets),
                              repeat(args.window_ns), repeat(args.ws_window), repeat(args.min_req)))

    parts = [r for r in raw if r is not None]
    if not parts:
        print("no usable benchmarks", file=sys.stderr)
        sys.exit(1)

    all_pc = pd.concat(parts, ignore_index=True)
    tier_summary = by_tier(all_pc)

    out_path = Path(args.out) if args.out else (
        Path.home() / "mgvm" / "analyze" / "_out" / "pc-stats" / "pc_hotness_footprint_windowed" /
        run_tag(args.run_id) / f"{config}_pc_hotness_footprint_windowed.xlsx")
    out_path.parent.mkdir(parents=True, exist_ok=True)

    with pd.ExcelWriter(out_path) as xw:
        all_pc.to_excel(xw, index=False, sheet_name="pc")
        tier_summary.to_excel(xw, index=False, sheet_name="by_tier")

    print(f"wrote {out_path}  (pc rows={len(all_pc)})")
    print()
    piv = tier_summary.pivot(index="benchmark", columns="tier", values="total_avg_footprint_MB").reindex(columns=TIER_ORDER).fillna(0.0)
    piv["Total"] = piv.sum(axis=1)
    print(piv.sort_values("Total", ascending=False).round(3).to_string())


if __name__ == "__main__":
    main()
