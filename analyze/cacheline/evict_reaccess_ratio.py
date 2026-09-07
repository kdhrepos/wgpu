"""Evict/Overwrite 된 캐시라인이 한 번이라도 재접근된 비율 — per-benchmark.

질문 (사용자 스펙 그대로):
  "chip0 에서 라인 A 가 evict/overwrite → 이후 같은 chip0 에서 라인 A 재접근"
  = 그 축출 이벤트 1건이 "재접근됨(=1)" 으로 카운트.  같은 라인이 다음 축출
  이전에 여러 번 재접근돼도 최초 1회만 세고 이후는 캐시 히트라 무시.

  즉,  numerator  = 최초 재접근에 도달한 victim 이벤트 수
       denominator= 전체 victim 이벤트 수 (per (chip, cacheline) 파티션)
       ratio      = 축출된 라인 중 한 번이라도 다시 요청된 비율.

**신호 정의**
  victim (evict/overwrite 된 라인) = type ∈ {8, 9}, is_ptw==0
    (2026-06-23 신설 필드.  addr=축출된 라인의 PA.  참조: memory_trace_format 메모)
  demand (재접근 후보)               = type ∈ {0..5}, is_ptw==0
  카운팅 파티션                       = (chiplet, cacheline)  — chip 을 넘나드는 재접근은 X

**알고리즘 — 스트리밍 청크 + per-key pending 상태 (메모리 O(active-pending))**
  key = (chip << 40) | (addr >> 6)              # 라인 64B, cl≤2^40 로 안전
  pending: set[key]                              # 현재 "축출됐지만 아직 재접근 없음"

  각 청크에서 (chip, key, is_victim) 을 key 로 groupby(sort=False, 청크 내 순서 유지):
    events   = is_victim 시퀀스
    carried  = key ∈ pending?
    if carried: aug = [True] + events            # 이전 청크에서 넘어온 victim = 가상 victim
    seg      = cumsum(aug)                       # 각 event 가 어느 victim 세그먼트에 속하나
    per-seg has_demand                            # 각 seg 에 demand 하나라도 있으면 True
    reaccess += (seg=1..max_seg 중 has_demand True 개수)   # 최초 재접근 = 1 카운트
    total_v  += 이 청크의 real victim 수          # carried 는 이전 청크에서 이미 셈
    pending 갱신: last seg has_demand → pending.discard(key)
                  else                     → pending.add(key)

  같은 (chip,line) 이 축출 없이 여러 번 접근돼도 seg 는 안 늘어남 → 최초 1회만 셈.
  같은 (chip,line) 이 재-축출 되면 seg 가 넘어감 → 이전 victim 이 없이 재접근됐으면
  seg 는 dead 로 남고, 새 victim 이 새 seg 로 시작 → 카운트 분리.  스펙과 정확히 일치.

Figure (400dpi PNG, 논문급):
  A. reaccess_ratio.png    — 벤치별 ratio bar + 축출/재접근 이벤트 수 라벨
  B. reaccess_absolute.png — victim/reaccess 절대값 그룹 bar (log-y)
  (여러 run-id 주고 --compare 하면 config 비교 bar 도 생성)

출력:  _out/cacheline/evict-reaccess-ratio/<run_tag>/
         per_benchmark.csv, reaccess_ratio.png, reaccess_absolute.png

사용:
  analyze/run.sh cacheline/evict_reaccess_ratio.py <run-id>
  analyze/run.sh cacheline/evict_reaccess_ratio.py <run-id> -j 8
  analyze/run.sh cacheline/evict_reaccess_ratio.py <run-a> <run-b> --compare
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
import matplotlib.pyplot as plt

from _paths import memory_trace_dir, run_config, run_tag, domain_out_dir
from motivation_common import file_suffix_for_config, trace_chunks, ALL_TRACE_COLS
from plot_style import apply_paper_style, color_for, save_fig, SIZE_WIDE
from params import benchmarks

DEMAND_TYPES = np.array([0, 1, 2, 3, 4, 5], dtype=np.int8)
VICTIM_TYPES = np.array([8, 9], dtype=np.int8)

USECOLS = ALL_TRACE_COLS   # full-row 읽고 drop_duplicates — 파일-쓰기 완전 중복만 제거
FIG_FORMATS = ("png",)

RATIO_COLOR   = "#D55E00"   # 주황 = 최종 ratio
VICTIM_COLOR  = "#0072B2"   # 파랑 = 총 victim
REACCESS_COLOR = "#009E73"  # 초록 = 재접근된 victim


# ── 코어 알고리즘 ────────────────────────────────────────────────────────────
def _process_chunk(chip: np.ndarray, cl: np.ndarray, is_v: np.ndarray,
                   pending: set, counters: dict) -> None:
    """청크 내 이벤트를 (chip,line) 그룹별로 벡터화 처리, pending set 갱신.
    Python for-loop 없이 pandas groupby.cumsum / any / last 만 사용."""
    # 64-bit key: 상위 24bit chip, 하위 40bit cacheline.  chip < 2^24, cl < 2^40 안전.
    key = (chip.astype(np.uint64) << np.uint64(40)) | cl.astype(np.uint64)
    df = pd.DataFrame({"key": key, "is_v": is_v, "is_d": ~is_v})

    victim_add = int(is_v.sum())

    # per-key cumulative victim count (seg 0 = 아직 victim 없음)
    df["seg"] = df.groupby("key", sort=False)["is_v"].cumsum().astype(np.int64)

    # carried key (이전 청크 pending) 는 seg +1 = 가상 victim 삽입 효과
    if pending:
        pending_arr = np.fromiter(pending, dtype=np.uint64, count=len(pending))
        carried_mask = np.isin(df["key"].to_numpy(), pending_arr)
        if carried_mask.any():
            df.loc[carried_mask, "seg"] += 1

    # (key, seg) 별 demand 존재 여부
    seg_has_d = (df.groupby(["key", "seg"], sort=False)["is_d"]
                   .any().reset_index())

    # reaccess = (seg >= 1 이면서 demand 있는) 세그먼트 수
    reaccess_add = int(((seg_has_d["seg"] >= 1) & seg_has_d["is_d"]).sum())

    # 각 key 의 마지막 seg → pending set 갱신 소스
    last_seg = df.groupby("key", sort=False)["seg"].last().reset_index()
    last_merged = last_seg.merge(seg_has_d, on=["key", "seg"], how="left")
    last_merged["is_d"] = last_merged["is_d"].fillna(False)

    active = last_merged[last_merged["seg"] >= 1]
    resolved = active.loc[active["is_d"], "key"].to_numpy(dtype=np.uint64)
    still_p = active.loc[~active["is_d"], "key"].to_numpy(dtype=np.uint64)
    if resolved.size:
        pending.difference_update(resolved.tolist())
    if still_p.size:
        pending.update(still_p.tolist())

    counters["reaccess"] += reaccess_add
    counters["victim"] += victim_add


def _count_accesses(trace_file: str) -> pd.Series:
    """Pass 1 — cacheline (addr>>6) 별 demand 접근 수 집계. chiplet 무관 순수
    라인 hotness 랭킹용 (같은 라인의 여러 chip 접근은 합산)."""
    parts: list[pd.Series] = []
    for chunk in trace_chunks(trace_file, usecols=USECOLS):
        chunk = chunk.drop_duplicates()
        types = chunk["type"].to_numpy(dtype=np.int8)
        is_ptw = chunk["is_ptw"].to_numpy(dtype=np.int8)
        is_d = np.isin(types, DEMAND_TYPES) & (is_ptw == 0)
        if not is_d.any():
            continue
        cl = chunk["addr"].to_numpy(dtype=np.uint64)[is_d] >> np.uint64(6)
        parts.append(pd.Series(cl).value_counts())
    if not parts:
        return pd.Series(dtype=np.int64)
    return pd.concat(parts).groupby(level=0).sum()


def _analyze_trace(trace_file: str, allowed: np.ndarray | None) -> dict:
    """Pass 2 — victim/reaccess 스트리밍 집계. allowed 가 None 이 아니면
    cacheline(addr>>6) 이 그 정렬된 uint64 배열에 속한 이벤트만 처리
    (chiplet 무관 = 순수 라인 필터).  victim/reaccess 집계는 여전히
    (chip, line) 파티션으로 돌아 재접근을 같은 chip 내에서만 인정."""
    pending: set = set()
    counters = {"victim": 0, "reaccess": 0}

    for chunk in trace_chunks(trace_file, usecols=USECOLS):
        chunk = chunk.drop_duplicates()
        types = chunk["type"].to_numpy(dtype=np.int8)
        is_ptw = chunk["is_ptw"].to_numpy(dtype=np.int8)

        is_v = np.isin(types, VICTIM_TYPES) & (is_ptw == 0)
        is_d = np.isin(types, DEMAND_TYPES) & (is_ptw == 0)
        keep = is_v | is_d
        if not keep.any():
            continue

        cl = (chunk["addr"].to_numpy(dtype=np.uint64) >> np.uint64(6))
        chip = chunk["chiplet"].to_numpy(dtype=np.int64)

        if allowed is not None:
            line_ok = np.isin(cl, allowed, assume_unique=False)
            keep &= line_ok
            if not keep.any():
                continue

        _process_chunk(chip[keep], cl[keep], is_v[keep], pending, counters)

    # 트레이스 끝에서 아직 pending 인 victim = 재접근 관측 X = dead → reaccess 증가 X
    counters["pending_end"] = len(pending)
    return counters


# ── per-benchmark worker ─────────────────────────────────────────────────────
def process_benchmark(bench: str, trace_dir: str, suffix: str, top_pct: float):
    trace_file = f"{trace_dir}/{bench}_{suffix}_memory_trace.csv"
    if not os.path.exists(trace_file):
        return None
    print(f"[{bench}] evict-reaccess (top {top_pct:g}%) ...", flush=True)
    try:
        allowed: np.ndarray | None = None
        n_lines = n_hot = 0
        if top_pct < 100.0:
            counts = _count_accesses(trace_file)   # cacheline hotness (chip 무관)
            if counts.empty:
                return None
            n_lines = int(len(counts))
            n_hot = max(1, int(n_lines * top_pct / 100))
            allowed = np.sort(counts.nlargest(n_hot).index.to_numpy(dtype=np.uint64))

        c = _analyze_trace(trace_file, allowed)
        v = c["victim"]
        r = c["reaccess"]
        return {
            "benchmark":       bench,
            "victim_events":   int(v),
            "reaccess_events": int(r),
            "dead_events":     int(v - r),
            "pending_at_end":  int(c["pending_end"]),
            "reaccess_ratio":  (r / v) if v else 0.0,
            "lines_total":     n_lines,
            "lines_selected":  n_hot,
        }
    except Exception as e:  # noqa: BLE001
        print(f"[{bench}] error: {e}")
        return None


def run_one(run_id: str, workers: int, top_pct: float):
    trace_dir = str(memory_trace_dir(run_id))
    config = run_config(run_id)
    suffix = file_suffix_for_config(config)

    if workers <= 1:
        raw = [process_benchmark(b, trace_dir, suffix, top_pct) for b in benchmarks]
    else:
        with ProcessPoolExecutor(max_workers=workers) as ex:
            raw = list(ex.map(process_benchmark, benchmarks,
                              repeat(trace_dir), repeat(suffix), repeat(top_pct)))
    rows = [r for r in raw if r is not None]
    if not rows:
        return config, None
    df = pd.DataFrame(rows).set_index("benchmark")
    return config, df


# ── 그림 A: 재접근 비율 bar ──────────────────────────────────────────────────
def plot_ratio(df: pd.DataFrame, out: Path, config: str) -> None:
    apply_paper_style()
    benches = list(df.index)
    n = len(benches)
    ratio = df["reaccess_ratio"].to_numpy()

    fig, ax = plt.subplots(figsize=SIZE_WIDE)
    x0 = np.arange(n)
    ax.bar(x0, ratio, 0.7, color=RATIO_COLOR, edgecolor="white", linewidth=0.4)

    for x, v in zip(x0, ratio):
        ax.text(x, v + 0.012, f"{v:.3f}", ha="center", va="bottom",
                fontsize=6, color="#333333")

    ax.set_xticks(x0)
    ax.set_xticklabels(benches, rotation=30, ha="right", fontsize=7)
    ax.set_ylabel("re-access ratio  =  re-accessed victims / total victims")
    ax.set_ylim(0, 1.08)
    ax.set_yticks(np.arange(0, 1.01, 0.25))
    ax.set_title(
        f"Fraction of evicted / overwritten lines re-accessed at least once "
        f"(per (chiplet, line), before next eviction) — {config}"
    )
    ax.grid(axis="y", alpha=0.35)
    save_fig(fig, out, "reaccess_ratio", formats=FIG_FORMATS)


# ── 그림 B: victim / reaccess 절대값 (log-y) ────────────────────────────────
def plot_absolute(df: pd.DataFrame, out: Path, config: str) -> None:
    apply_paper_style()
    benches = list(df.index)
    n = len(benches)
    v = df["victim_events"].to_numpy(dtype=float)
    r = df["reaccess_events"].to_numpy(dtype=float)

    fig, ax = plt.subplots(figsize=SIZE_WIDE)
    x0 = np.arange(n)
    bar_w = 0.36
    ax.bar(x0 - bar_w / 2, v, bar_w, color=VICTIM_COLOR,
           label="victim events (evict + overwrite)", edgecolor="white", linewidth=0.4)
    ax.bar(x0 + bar_w / 2, r, bar_w, color=REACCESS_COLOR,
           label="re-accessed victims (before next eviction)",
           edgecolor="white", linewidth=0.4)

    ax.set_xticks(x0)
    ax.set_xticklabels(benches, rotation=30, ha="right", fontsize=7)
    ax.set_ylabel("event count")
    ax.set_yscale("log")
    ax.set_title(f"Absolute victim vs re-access counts — {config}")
    ax.legend(frameon=False, ncol=2, fontsize=7, loc="upper right")
    ax.grid(axis="y", which="both", alpha=0.3)
    save_fig(fig, out, "reaccess_absolute", formats=FIG_FORMATS)


# ── 그림 C: config 비교 (여러 run-id) ────────────────────────────────────────
def plot_compare(frames: dict[str, pd.DataFrame], out: Path) -> None:
    apply_paper_style()
    configs = list(frames.keys())
    benches = sorted(set().union(*[set(f.index) for f in frames.values()]))
    n, nc = len(benches), len(configs)

    fig, ax = plt.subplots(figsize=SIZE_WIDE)
    group_w = 0.82
    bar_w = group_w / nc
    x0 = np.arange(n)
    for j, cfg in enumerate(configs):
        f = frames[cfg]
        vals = [f.loc[b, "reaccess_ratio"] if b in f.index else np.nan
                for b in benches]
        xpos = x0 - group_w / 2 + bar_w * (j + 0.5)
        ax.bar(xpos, vals, bar_w, color=color_for(j), label=cfg,
               edgecolor="white", linewidth=0.4)

    ax.set_xticks(x0)
    ax.set_xticklabels(benches, rotation=30, ha="right", fontsize=7)
    ax.set_ylabel("re-access ratio")
    ax.set_ylim(0, 1.0)
    ax.set_title("Re-access ratio — platform comparison")
    ax.legend(frameon=False, ncol=min(nc, 3), fontsize=6.5, loc="upper right")
    ax.grid(axis="y", alpha=0.3)
    save_fig(fig, out, "reaccess_ratio_compare", formats=FIG_FORMATS)


# ── 메인 ─────────────────────────────────────────────────────────────────────
def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("run_ids", nargs="*", default=["latest"],
                    help="run-id 1개 이상. 2개 이상이고 --compare 면 비교 그림 생성.")
    ap.add_argument("-j", "--workers", type=int, default=4,
                    help="벤치마크 병렬 워커 수 (기본 4, 1=순차)")
    ap.add_argument("--top-pct", type=float, default=100.0,
                    help="접근 횟수 기준 상위 N%% (chip,line) 만 대상. "
                         "기본 100=전체. 예: 10 → 상위 10%% hot 라인만.")
    ap.add_argument("--compare", action="store_true",
                    help="run-id 2개 이상일 때 config 간 비교 막대 그림 추가.")
    args = ap.parse_args()

    if not (0.0 < args.top_pct <= 100.0):
        ap.error("--top-pct must be in (0, 100]")

    frames: dict[str, pd.DataFrame] = {}
    last_out: Path | None = None

    for rid in args.run_ids:
        config, df = run_one(rid, args.workers, args.top_pct)
        if df is None:
            print(f"[{rid}] no usable benchmarks — skip", file=sys.stderr)
            continue

        out = domain_out_dir("cacheline", "evict-reaccess-ratio", rid)
        df.to_csv(out / "per_benchmark.csv")
        plot_ratio(df, out, config)
        plot_absolute(df, out, config)

        print(f"\n[{config}] wrote {out}")
        print(df[["victim_events", "reaccess_events", "dead_events",
                  "reaccess_ratio", "pending_at_end"]].to_string())

        frames[f"{config}/{run_tag(rid)}"] = df
        last_out = out.parent

    if args.compare and len(frames) > 1 and last_out is not None:
        cmp = last_out / "compare"
        cmp.mkdir(parents=True, exist_ok=True)
        plot_compare(frames, cmp)
        print(f"\ncompare figure -> {cmp}")


if __name__ == "__main__":
    main()
