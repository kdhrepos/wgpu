"""(owner, pc) 단위 exact stack(reuse) distance — footprint/stackdist.go 의
Mattson/Olken 알고리즘(Fenwick tree)을 그대로 이식, PC 스코프로 좁힌 버전.

footprint/stackdist.go 는 칩렛 단위(그 칩렛의 전체 로컬 참조열)로 잰다. 여긴
pc_stats_l2miss_per_owner.py 와 같은 필터(rdma==True & type in [2,5])로 좁혀서,
실제 PCCounter 가 보는 스트림 그대로 잰다 — 한 owner(home chiplet)가 특정 PC
에 대해 받는 원격 요청들만의 시간순 시퀀스.

정의: 한 access 의 stack distance = 그 라인이 마지막으로 만져진 시점 이후 ~
지금 사이에 "다른" distinct 라인이 몇 개 만져졌는가 — LRU 캐시가 몇 라인이면
이 access 가 hit 이 됐을지의 정답(근사 아님). threshold(라인 수) 이하면 hit,
cold miss(그 (owner,pc) 스트림에서 첫 접근)는 분모엔 들어가고 어떤 threshold
에서도 hit 아님(표준 MRC 관례, stackdist.go 와 동일 컨벤션).

성능: 순수 파이썬 루프로는 큰 PC(수천만 access, 예 cc/gups 는 PC 하나가 2-7천만
회)를 감당 못 해서 numba 로 Fenwick add/sum 루프를 JIT 컴파일한다. line id 는
(owner,pc) 스트림 안에서 np.unique(..., return_inverse=True) 로 0..U-1 dense
code 로 미리 바꿔서, Go 의 lastTime map[uint64]int64 대신 크기 U 짜리 배열 하나로
O(1) 조회 — 해시맵보다 단순하고 numba 에 그대로 태울 수 있다.

(owner,pc) 별로 계산한 뒤 pc 로 묶는다 — 이 리포에서 reuse/sharing/hop 을 다룰
때와 같은 규약(pc_stats_l2miss_per_owner.py). "trace 행 순서 = 발생 시간 순서"
를 가정한다(이 리포의 다른 pc-stats 스크립트들과 동일 가정, 재정렬 안 함).

출력 (xlsx, 두 시트):
  pc_hotness  benchmark, pc, owner_count, total_accesses, total_cold_misses,
              hit_rate_<mb>mb_mean(owner 평균) / hit_rate_<mb>mb_reqw(access-weighted)
  per_owner   (benchmark, owner, pc) 단위 원본 — accesses, cold_misses, hit_rate_<mb>mb

사용:
  analyze/run.sh pc-stats/pc_stack_distance.py <run-id> \
      [--thresholds-mb 0.0625,0.25,1,4,16] [--out path.xlsx] [-j 4]
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
from motivation_common import file_suffix_for_config, trace_chunks
from params import benchmarks

LINE_BYTES = 64
DEFAULT_THRESHOLDS_MB = "0.0625,0.25,1,4,16"  # 64KB..16MB, 4MB == real L2 (footprintL2Lines)

# "time" must stay in USECOLS even though it's dropped after dedup: without it,
# drop_duplicates() collapses genuinely repeated accesses to the same line by
# the same (owner,pc) at different times into one row — exactly the reuse this
# script measures. (Other pc-stats scripts that only count presence/totals
# don't need this care; stack distance needs every individual repeat intact.)
USECOLS = ["time", "pc", "addr", "owner", "chiplet", "rdma", "type"]


def _mb_to_lines(mb: float) -> int:
    return max(1, round(mb * 1024 * 1024 / LINE_BYTES))


def _mb_label(mb: float) -> str:
    s = f"{mb:g}"
    return s.replace(".", "p")


# ── Fenwick(BIT) exact stack distance, numba JIT ────────────────────────────
def _make_stack_distance_fn():
    from numba import njit

    @njit(cache=True, inline="always")
    def fen_add(tree, i, delta):
        n = tree.shape[0]
        while i < n:
            tree[i] += delta
            i += i & (-i)

    @njit(cache=True, inline="always")
    def fen_sum(tree, i):
        s = 0
        while i > 0:
            s += tree[i]
            i -= i & (-i)
        return s

    @njit(cache=True)
    def stack_distance(codes, n_unique, thresholds):
        n = codes.shape[0]
        tree = np.zeros(n + 2, dtype=np.int32)
        last_time = np.full(n_unique, -1, dtype=np.int64)
        n_th = thresholds.shape[0]
        hits = np.zeros(n_th, dtype=np.int64)
        cold = 0

        for t in range(n):
            code = codes[t]
            pos = t + 1
            prev = last_time[code]
            if prev >= 0:
                prev_pos = prev + 1
                total = fen_sum(tree, n)
                before = fen_sum(tree, prev_pos)
                dist = total - before
                for i in range(n_th):
                    if dist <= thresholds[i]:
                        hits[i] += 1
                fen_add(tree, prev_pos, -1)
            else:
                cold += 1
            fen_add(tree, pos, 1)
            last_time[code] = t

        return hits, cold

    return stack_distance


_STACK_DISTANCE_FN = None


def _stack_distance_fn():
    global _STACK_DISTANCE_FN
    if _STACK_DISTANCE_FN is None:
        _STACK_DISTANCE_FN = _make_stack_distance_fn()
    return _STACK_DISTANCE_FN


def _load_owner_pc_stream(trace_file: str) -> pd.DataFrame:
    """(owner, pc, line) — rdma==True & type in [2,5] 데이터 행, 원본(시간) 순서 보존."""
    parts = []
    for chunk in trace_chunks(trace_file, usecols=USECOLS):
        chunk = chunk.drop_duplicates()
        keep = (chunk["rdma"].astype(bool)) & (chunk["type"].isin([2, 5])) & (chunk["pc"] != 0)
        if not keep.any():
            continue
        sub = chunk.loc[keep]
        parts.append(pd.DataFrame({
            "owner": sub["owner"].to_numpy(dtype=np.int64),
            "pc": sub["pc"].to_numpy(dtype=np.int64),
            "line": (sub["addr"].to_numpy(dtype=np.uint64) >> np.uint64(6)).astype(np.int64),
        }))
    if not parts:
        return pd.DataFrame(columns=["owner", "pc", "line"])
    return pd.concat(parts, ignore_index=True)


def process_benchmark(bench: str, trace_dir: str, suffix: str, thresholds_mb: list[float]):
    trace_file = f"{trace_dir}/{bench}_{suffix}_memory_trace.csv"
    if not os.path.exists(trace_file):
        return None
    print(f"[{bench}] stack distance ...", flush=True)
    try:
        df = _load_owner_pc_stream(trace_file)
        if df.empty:
            return None

        thresholds_lines = np.array([_mb_to_lines(mb) for mb in thresholds_mb], dtype=np.int64)
        stack_distance = _stack_distance_fn()

        owner_arr = df["owner"].to_numpy()
        pc_arr = df["pc"].to_numpy()
        line_arr = df["line"].to_numpy()

        groups = df.groupby(["owner", "pc"], sort=False).indices  # (owner,pc) -> row positions, original order
        rows = []
        for (owner, pc), pos in groups.items():
            seg = line_arr[pos]
            n = seg.shape[0]
            _uniq, codes = np.unique(seg, return_inverse=True)
            hits, cold = stack_distance(codes.astype(np.int64), len(_uniq), thresholds_lines)
            row = {"benchmark": bench, "owner": int(owner), "pc": int(pc),
                   "accesses": int(n), "cold_misses": int(cold),
                   "unique_lines": int(len(_uniq))}
            for mb, h in zip(thresholds_mb, hits):
                row[f"hit_rate_{_mb_label(mb)}mb"] = round(float(h) / n, 5) if n else 0.0
            rows.append(row)

        per_owner = pd.DataFrame(rows)
        return per_owner
    except Exception as e:  # noqa: BLE001
        print(f"[{bench}] error: {e}")
        return None


def aggregate_per_pc(per_owner: pd.DataFrame, thresholds_mb: list[float]) -> pd.DataFrame:
    hr_cols = [f"hit_rate_{_mb_label(mb)}mb" for mb in thresholds_mb]

    agg = {
        "owner_count": ("owner", "nunique"),
        "total_accesses": ("accesses", "sum"),
        "total_cold_misses": ("cold_misses", "sum"),
        "avg_unique_lines": ("unique_lines", "mean"),
    }
    out = per_owner.groupby(["benchmark", "pc"]).agg(**agg).reset_index()

    mean_hr = per_owner.groupby(["benchmark", "pc"])[hr_cols].mean().reset_index()
    for c in hr_cols:
        out[f"{c}_mean"] = mean_hr[c].round(5)

    # access-weighted alt: sum(hit_rate*accesses)/sum(accesses) per threshold,
    # computed from hits directly (hit_rate*accesses == hits, up to rounding —
    # recompute hits exactly to avoid compounding the round() above).
    tmp = per_owner.copy()
    for c in hr_cols:
        tmp[f"__hits_{c}"] = tmp[c] * tmp["accesses"]
    reqw = tmp.groupby(["benchmark", "pc"]).agg(
        **{f"__hits_{c}": (f"__hits_{c}", "sum") for c in hr_cols},
        __acc=("accesses", "sum"),
    ).reset_index()
    for c in hr_cols:
        out[f"{c}_reqw"] = (reqw[f"__hits_{c}"] / reqw["__acc"]).round(5)

    return out.sort_values(["benchmark", "total_accesses"], ascending=[True, False])


def main():
    ap = argparse.ArgumentParser(description=__doc__,
                                 formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("run_id", nargs="?", default="mesh-6x6/latest")
    ap.add_argument("--thresholds-mb", default=DEFAULT_THRESHOLDS_MB,
                    help=f"comma-separated capacities in MB (default {DEFAULT_THRESHOLDS_MB})")
    ap.add_argument("--out", default=None, help="출력 xlsx 경로")
    ap.add_argument("-j", "--workers", type=int, default=4)
    args = ap.parse_args()

    thresholds_mb = [float(x) for x in args.thresholds_mb.split(",")]

    trace_dir = str(memory_trace_dir(args.run_id))
    config = run_config(args.run_id)
    suffix = file_suffix_for_config(config)

    if args.workers <= 1:
        raw = [process_benchmark(b, trace_dir, suffix, thresholds_mb) for b in benchmarks]
    else:
        with ProcessPoolExecutor(max_workers=args.workers) as ex:
            raw = list(ex.map(process_benchmark, benchmarks,
                              repeat(trace_dir), repeat(suffix), repeat(thresholds_mb)))

    parts = [r for r in raw if r is not None]
    if not parts:
        print("no usable benchmarks", file=sys.stderr)
        sys.exit(1)

    per_owner = pd.concat(parts, ignore_index=True)
    per_pc = aggregate_per_pc(per_owner, thresholds_mb)

    out_path = Path(args.out) if args.out else (
        Path.home() / "mgvm" / "analyze" / "_out" / "pc-stats" / "pc_stack_distance" /
        run_tag(args.run_id) / f"{config}_stack_distance.xlsx")
    out_path.parent.mkdir(parents=True, exist_ok=True)

    with pd.ExcelWriter(out_path) as xw:
        per_pc.to_excel(xw, index=False, sheet_name="pc_hotness")
        per_owner.to_excel(xw, index=False, sheet_name="per_owner")

    print(f"wrote {out_path}  (pc rows={len(per_pc)}, per-owner rows={len(per_owner)})")
    print()
    show = ["benchmark", "pc", "owner_count", "total_accesses"] + \
        [f"hit_rate_{_mb_label(mb)}mb_reqw" for mb in thresholds_mb]
    print(per_pc[show].to_string(index=False))


if __name__ == "__main__":
    main()
