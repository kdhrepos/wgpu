"""벤치마크별 per-chiplet evict / overwrite volume — memory trace 기반.

shared_private_capacity.py 의 축출 버전 — 그 GPM (칩렛) L2 가 lifetime 동안
얼마나 많은 데이터를 밀어냈는가 (evict) / 덮어썼는가 (overwrite).

정의:
  evict     = trace type 8. L2 라인이 축출됨 (fill 시 밀려남).
  overwrite = trace type 9. L2 위치가 다른 라인으로 덮어써짐.
  chiplet   = 그 이벤트가 일어난 L2 소속 칩렛 = 축출을 낸 GPM.

  event count       = 그 chiplet 이 evict/overwrite 이벤트를 몇 번 냈나
                      (같은 라인이 여러 번 축출되면 여러 번 카운트, 데이터 이동
                       총량 관점).
  unique line count = 그 chiplet 이 한 번이라도 축출/덮어쓰기한 서로 다른 라인 수
                      (thrash 라인 하나 → 1, unique 축출 footprint).

  event 기반이 "얼만큼의 데이터가 움직였나" 에 더 직관적, unique 는 "얼만큼의
  서로 다른 라인이 자리를 못 지켰나" 에 답.

출력:
  ~/mgvm/figures/evict-overwrite-per-chiplet/<config>-<run_tag>/
      per_benchmark.csv       — 벤치별 요약 (chiplet 평균 / 전 chiplet 합)
      per_chiplet.csv         — 벤치 × chiplet 상세
      evict_overwrite_events_stacked.png    — evict + overwrite event MB (chiplet 평균)
      evict_overwrite_unique_stacked.png    — evict + overwrite unique line MB (chiplet 평균)

사용:
  analyze/run.sh cacheline/evict_overwrite_per_chiplet.py <run-id>
  analyze/run.sh cacheline/evict_overwrite_per_chiplet.py <run-id> -j 8
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
from shared_private_capacity import plot_stacked
from pc_hotness import mesh_dims_from_config
from params import benchmarks

EVICT_TYPE = 8
OVERWRITE_TYPE = 9
USECOLS = ALL_TRACE_COLS   # full-row dedup (subset dedup 하면 reuse 붕괴)
LINE_BYTES = 64
LINE_MB = LINE_BYTES / (1024.0 ** 2)

FIG_ROOT = Path.home() / "mgvm" / "figures" / "evict-overwrite-per-chiplet"
FIG_FORMATS = ("png",)


# ── 청크 축적: chiplet 별 (event 수 / unique line 수) ─────────────────────
def _agg(trace_file: str):
    """chunk 별 (chiplet, kind) event 수 + (chiplet, kind, line) unique 3-쌍."""
    event_parts = []   # per chunk: (chiplet, kind) -> count
    uniq_parts = []    # per chunk: (chiplet, kind, line) unique
    for chunk in trace_chunks(trace_file, usecols=USECOLS):
        chunk = chunk.drop_duplicates()          # full-row dedup
        typ = chunk["type"].to_numpy(dtype=np.int8)
        m = (typ == EVICT_TYPE) | (typ == OVERWRITE_TYPE)
        if not m.any():
            continue
        chip = chunk["chiplet"].to_numpy(dtype=np.int64)[m]
        line = chunk["addr"].to_numpy(dtype=np.uint64)[m] >> np.uint64(6)
        kind = np.where(typ[m] == EVICT_TYPE, "evict", "overwrite")
        sub = pd.DataFrame({"chiplet": chip, "kind": kind, "line": line})
        event_parts.append(sub.groupby(["chiplet", "kind"]).size())
        uniq_parts.append(sub.drop_duplicates())
    if not event_parts:
        return None, None
    events = pd.concat(event_parts).groupby(level=[0, 1]).sum()
    uniq = pd.concat(uniq_parts, ignore_index=True).drop_duplicates()
    return events, uniq


def _per_chip(series_or_df, n_chiplets: int, name: str) -> pd.Series:
    """chip → count reindex-full 로 접근 0 칩렛도 0 으로 유지."""
    chip_idx = pd.RangeIndex(n_chiplets, name="chiplet")
    return series_or_df.reindex(chip_idx, fill_value=0).rename(name)


# ── per-benchmark worker ──────────────────────────────────────────────────
def process_benchmark(bench: str, trace_dir: str, suffix: str, n_chiplets: int):
    trace_file = f"{trace_dir}/{bench}_{suffix}_memory_trace.csv"
    if not os.path.exists(trace_file):
        return None
    print(f"[{bench}] per-chiplet evict/overwrite ...", flush=True)
    try:
        events, uniq = _agg(trace_file)
        if events is None:
            print(f"[{bench}] no victim (type 8/9) events — skip")
            return None

        # event counts
        ev_events = events.xs("evict", level="kind") if "evict" in events.index.get_level_values("kind") else pd.Series(dtype="int64")
        ow_events = events.xs("overwrite", level="kind") if "overwrite" in events.index.get_level_values("kind") else pd.Series(dtype="int64")

        # unique line counts (that chiplet 이 한 번이라도 축출/덮어쓴 서로 다른 라인)
        ev_uniq = uniq[uniq["kind"] == "evict"].groupby("chiplet").size()
        ow_uniq = uniq[uniq["kind"] == "overwrite"].groupby("chiplet").size()

        ev_events = _per_chip(ev_events, n_chiplets, "evict_events")
        ow_events = _per_chip(ow_events, n_chiplets, "overwrite_events")
        ev_uniq = _per_chip(ev_uniq, n_chiplets, "evict_unique_lines")
        ow_uniq = _per_chip(ow_uniq, n_chiplets, "overwrite_unique_lines")
        total_events = (ev_events + ow_events).rename("total_events")
        total_uniq = (ev_uniq + ow_uniq).rename("total_unique_lines")

        def _stats(prefix: str, per_chip: pd.Series) -> dict:
            cap_mb = per_chip * LINE_MB   # event 수 or line 수 × 64B
            return {
                f"{prefix}_total": int(per_chip.sum()),
                f"{prefix}_capacity_mb_total": float(cap_mb.sum()),
                f"avg_{prefix}": float(per_chip.mean()),
                f"avg_{prefix}_capacity_mb": float(cap_mb.mean()),
                f"std_{prefix}_capacity_mb": float(cap_mb.std()),
            }

        row = {
            "benchmark": bench,
            **_stats("evict_events", ev_events),
            **_stats("overwrite_events", ow_events),
            **_stats("total_events", total_events),
            **_stats("evict_unique_lines", ev_uniq),
            **_stats("overwrite_unique_lines", ow_uniq),
            **_stats("total_unique_lines", total_uniq),
        }

        per_chip = pd.concat([ev_events, ow_events, total_events,
                              ev_uniq, ow_uniq, total_uniq], axis=1)
        for c in per_chip.columns:
            per_chip[f"{c}_capacity_mb"] = per_chip[c] * LINE_MB
        per_chip["benchmark"] = bench
        return row, per_chip.reset_index()
    except Exception as e:   # noqa: BLE001
        import traceback
        traceback.print_exc()
        print(f"[{bench}] error: {e}")
        return None


# ── 벤치 sweep + plot ─────────────────────────────────────────────────────
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
    df = df.sort_values("avg_total_events_capacity_mb")
    per_chip_df = pd.concat(per_chip_frames, ignore_index=True)
    return config, df, per_chip_df


# ── STACK spec (shared_private_capacity.plot_stacked 재활용) ──────────────
STACK_EVENTS = [
    {"col": "avg_evict_events_capacity_mb", "label": "Evict",
     "color": "darkgray", "hatch": None},
    {"col": "avg_overwrite_events_capacity_mb", "label": "Overwrite",
     "color": "white", "hatch": "///"},
]

STACK_UNIQUE = [
    {"col": "avg_evict_unique_lines_capacity_mb", "label": "Evict (unique)",
     "color": "darkgray", "hatch": None},
    {"col": "avg_overwrite_unique_lines_capacity_mb", "label": "Overwrite (unique)",
     "color": "white", "hatch": "///"},
]


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("run_id", nargs="?", default="mesh-6x6/latest")
    ap.add_argument("-j", "--workers", type=int, default=4,
                    help="벤치마크 병렬 워커 수 (기본 4)")
    args = ap.parse_args()

    config, df, per_chip_df = run_one(args.run_id, args.workers)
    if df is None:
        print("no usable benchmarks (need victim type 8/9 in trace)", file=sys.stderr)
        sys.exit(1)

    tag = run_tag(args.run_id)
    out = FIG_ROOT / f"{config}-{tag}"
    out.mkdir(parents=True, exist_ok=True)
    df.to_csv(out / "per_benchmark.csv")
    per_chip_df.to_csv(out / "per_chiplet.csv", index=False)
    # 정렬 재적용 — Events 기준
    df_ev_sorted = df.sort_values("avg_total_events_capacity_mb")
    plot_stacked(df_ev_sorted, STACK_EVENTS, out, "evict_overwrite_events_stacked")
    df_uq_sorted = df.sort_values("avg_total_unique_lines_capacity_mb")
    plot_stacked(df_uq_sorted, STACK_UNIQUE, out, "evict_overwrite_unique_stacked")

    print(f"\n[{config}] wrote {out}")
    print(df[["avg_evict_events_capacity_mb",
             "avg_overwrite_events_capacity_mb",
             "avg_total_events_capacity_mb",
             "avg_evict_unique_lines_capacity_mb",
             "avg_overwrite_unique_lines_capacity_mb",
             "avg_total_unique_lines_capacity_mb"]].to_string())


if __name__ == "__main__":
    main()
