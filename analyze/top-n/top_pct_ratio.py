"""Top-N% hot 주소가 전체 접근에서 차지하는 비율 (4 스토리 축).

각 벤치마크 × {Total, RDMA, Local, RemoteAccess(=type∈{2,5} & rdma)} 조합에 대해
addr 별 접근수/hop 누적 → 접근수 내림차순 상위 N% addr 뽑아
  · 접근수 합계 (전체 / top-N%)
  · hop 합계   (전체 / top-N%)
  · top-N% 의 RW/RO 비율 (해당 addr 에 쓰기 발생했는지)
를 리포트. `top_share = top_access / total_access` = "상위 N% 주소가 전체 접근의 X%".

이전판(top_10p_ratio.py) 대비 변경:
  · usecols=필요 컬럼만(6개) + 명시 dtype     ← 메모리 대폭 절감
  · pd.read_csv chunked                       ← 트레이스 전체를 안 올림
  · --top-pct N (기본 10, 0<N≤100)
  · mesh dims 는 config 에서 파싱 (하드코딩 w=6 제거)
  · 파일 suffix 는 file_suffix_for_config 로 자동 (mesh-6x6 하드코딩 제거)

사용:
  analyze/run.sh top-n/top_pct_ratio.py <run-id>
  analyze/run.sh top-n/top_pct_ratio.py <run-id> --top-pct 5 -j 8
"""

from __future__ import annotations

import argparse
import os
import sys
from concurrent.futures import ProcessPoolExecutor
from itertools import repeat

import numpy as np
import pandas as pd

from _paths import memory_trace_dir, run_config, domain_out_dir
from motivation_common import file_suffix_for_config
from pc_hotness import mesh_dims_from_config
from params import benchmarks

USECOLS = ["addr", "rw", "rdma", "type", "chiplet", "owner"]
DTYPES = {
    "addr":    np.uint64,
    "rw":      np.int8,
    "rdma":    np.int8,
    "type":    np.int8,
    "chiplet": np.int16,
    "owner":   np.int16,
}
CHUNK_ROWS = 4_000_000
MISS_TYPES = np.array([2, 5], dtype=np.int8)
SUBSETS = ("total", "rdma", "local", "remote_access")


def _read_chunks(path: str):
    return pd.read_csv(path, skipinitialspace=True,
                       usecols=USECOLS, dtype=DTYPES,
                       chunksize=CHUNK_ROWS)


def _stats(df: pd.DataFrame, rw_addrs: np.ndarray, top_pct: float,
           name: str) -> dict:
    """concat 된 addr 별 (hop_sum, count) DataFrame 에서 top-N% 통계 산출."""
    out = {}
    if df.empty:
        for k in ("addr_count", "top_addr_count",
                  "total_access", "top_access", "top_access_share",
                  "total_hop", "top_hop", "top_hop_share",
                  "top_rw_ratio", "top_ro_ratio",
                  "total_mean", "top_mean"):
            out[f"{name}_{k}"] = 0.0 if "ratio" in k or "share" in k or "mean" in k else 0
        return out

    df = df.sort_values("count", ascending=False, kind="stable")
    n_addr = int(len(df))
    n_hot = max(1, int(n_addr * top_pct / 100))
    top = df.head(n_hot)

    total_c = int(df["count"].sum())
    total_h = int(df["hop_sum"].sum())
    top_c   = int(top["count"].sum())
    top_h   = int(top["hop_sum"].sum())

    top_addrs = top.index.to_numpy(dtype=np.uint64)
    rw_hit = int(np.isin(top_addrs, rw_addrs).sum()) if rw_addrs.size else 0
    rw_ratio = rw_hit / n_hot if n_hot else 0.0

    out[f"{name}_addr_count"]        = n_addr
    out[f"{name}_top_addr_count"]    = n_hot
    out[f"{name}_total_access"]      = total_c
    out[f"{name}_top_access"]        = top_c
    out[f"{name}_top_access_share"]  = (top_c / total_c) if total_c else 0.0
    out[f"{name}_total_hop"]         = total_h
    out[f"{name}_top_hop"]           = top_h
    out[f"{name}_top_hop_share"]     = (top_h / total_h) if total_h else 0.0
    out[f"{name}_top_rw_ratio"]      = rw_ratio
    out[f"{name}_top_ro_ratio"]      = 1.0 - rw_ratio
    out[f"{name}_total_mean"]        = (total_c / n_addr) if n_addr else 0.0
    out[f"{name}_top_mean"]          = (top_c / n_hot) if n_hot else 0.0
    return out


def process_benchmark(bench: str, trace_dir: str, suffix: str,
                      top_pct: float, w: int) -> dict | None:
    trace_file = f"{trace_dir}/{bench}_{suffix}_memory_trace.csv"
    if not os.path.exists(trace_file):
        print(f"No {trace_file}, skip", flush=True)
        return None

    print(f"[{bench}] top-{top_pct:g}% ratio ...", flush=True)
    try:
        parts: dict[str, list[pd.DataFrame]] = {s: [] for s in SUBSETS}
        rw_addr_parts: list[np.ndarray] = []
        w_i = np.int64(w)

        for chunk in _read_chunks(trace_file):
            chunk = chunk.drop_duplicates()
            addr = chunk["addr"].to_numpy(dtype=np.uint64)
            chip = chunk["chiplet"].to_numpy(dtype=np.int64)
            owner = chunk["owner"].to_numpy(dtype=np.int64)
            rdma = chunk["rdma"].to_numpy(dtype=np.int8)
            ty = chunk["type"].to_numpy(dtype=np.int8)
            rw = chunk["rw"].to_numpy(dtype=np.int8)

            # owner 유효 (-1 등) 체크 후 hop.  invalid 는 hop=0 처리.
            valid = (chip >= 0) & (owner >= 0)
            sx, sy = chip % w_i, chip // w_i
            dx, dy = owner % w_i, owner // w_i
            hop = np.where(valid,
                           np.abs(sx - dx) + np.abs(sy - dy), 0).astype(np.int64)

            is_write = rw == 1
            if is_write.any():
                rw_addr_parts.append(np.unique(addr[is_write]))

            masks = {
                "total":         np.ones(len(chunk), dtype=bool),
                "rdma":          rdma == 1,
                "local":         rdma == 0,
                "remote_access": (rdma == 1) & np.isin(ty, MISS_TYPES),
            }

            for name, m in masks.items():
                if not m.any():
                    continue
                sub = pd.DataFrame({"addr": addr[m], "hop": hop[m]})
                g = sub.groupby("addr", sort=False)["hop"].agg(["sum", "count"])
                g.columns = ["hop_sum", "count"]
                parts[name].append(g)

        # rw addr 통합 (한 번만 unique)
        rw_addrs = (np.unique(np.concatenate(rw_addr_parts))
                    if rw_addr_parts else np.empty(0, dtype=np.uint64))

        row: dict = {"benchmark": bench}
        for name in SUBSETS:
            if parts[name]:
                merged = pd.concat(parts[name]).groupby(level=0).sum()
            else:
                merged = pd.DataFrame(columns=["hop_sum", "count"])
            row.update(_stats(merged, rw_addrs, top_pct, name))
        return row
    except Exception as e:  # noqa: BLE001
        print(f"[{bench}] error: {e}", flush=True)
        return None


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("run_id", nargs="?", default="latest")
    ap.add_argument("-j", "--workers", type=int, default=4,
                    help="벤치마크 병렬 워커 수 (기본 4, 1=순차)")
    ap.add_argument("--top-pct", type=float, default=10.0,
                    help="접근수 기준 상위 N%% addr (기본 10)")
    args = ap.parse_args()

    if not (0.0 < args.top_pct <= 100.0):
        ap.error("--top-pct must be in (0, 100]")

    trace_dir = str(memory_trace_dir(args.run_id))
    config = run_config(args.run_id)
    suffix = file_suffix_for_config(config)
    n_side, _ = mesh_dims_from_config(config)

    print(f"trace_dir = {trace_dir}  suffix = {suffix}  w = {n_side}  "
          f"top = {args.top_pct}%", flush=True)

    if args.workers <= 1:
        raw = [process_benchmark(b, trace_dir, suffix, args.top_pct, n_side)
               for b in benchmarks]
    else:
        with ProcessPoolExecutor(max_workers=args.workers) as ex:
            raw = list(ex.map(process_benchmark, benchmarks,
                              repeat(trace_dir), repeat(suffix),
                              repeat(args.top_pct), repeat(n_side)))
    rows = [r for r in raw if r is not None]
    if not rows:
        print("no usable benchmarks", file=sys.stderr)
        sys.exit(1)

    df = pd.DataFrame(rows).set_index("benchmark")
    out = domain_out_dir("top-n", "top-pct-ratio", args.run_id)
    csv = out / f"top{int(args.top_pct)}p_ratio.csv"
    df.to_csv(csv)
    print(f"\nwrote {csv}")
    # 요약: subset 별 top-N% share 만 뽑아 보기 좋게
    share_cols = [f"{s}_top_access_share" for s in SUBSETS]
    print("\n--- top-N% access share (top_access / total_access) ---")
    print(df[share_cols].to_string(float_format=lambda x: f"{x:.3f}"))


if __name__ == "__main__":
    main()
