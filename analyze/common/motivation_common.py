"""Motivation 그림들이 공통으로 쓰는 가벼운 데이터 헬퍼.

- 리포트 CSV(long form: where, what, value) 파싱
- 칩렛 간 트래픽 행렬 (`Chip-to-chip-traced`, `Chip-to-chip-ptw-traced`) 추출
- xDim×yDim 메시 위 Manhattan 거리
- 메모리 트레이스 chunk 읽기 (큰 트레이스 대비)
"""

from __future__ import annotations

import os
import re
from pathlib import Path
from typing import Iterable, Optional

import numpy as np
import pandas as pd

# 기본 메시 형상. 비정사각형은 mgvm 워크플로에서 안 쓴다(메모리: project_waferscale_square_mesh).
XDIM_DEFAULT = 6
YDIM_DEFAULT = 6
NUM_CHIPLETS_DEFAULT = XDIM_DEFAULT * YDIM_DEFAULT


def chiplet_xy(chip: int, xdim: int = XDIM_DEFAULT) -> tuple[int, int]:
    return chip % xdim, chip // xdim


def manhattan(a: int, b: int, xdim: int = XDIM_DEFAULT) -> int:
    ax, ay = chiplet_xy(a, xdim)
    bx, by = chiplet_xy(b, xdim)
    return abs(ax - bx) + abs(ay - by)


def manhattan_matrix(xdim: int = XDIM_DEFAULT, ydim: int = YDIM_DEFAULT) -> np.ndarray:
    """N×N (N=xdim*ydim) Manhattan 거리 행렬. 정수."""
    n = xdim * ydim
    xs = np.arange(n) % xdim
    ys = np.arange(n) // xdim
    dx = np.abs(xs[:, None] - xs[None, :])
    dy = np.abs(ys[:, None] - ys[None, :])
    return (dx + dy).astype(np.int32)


# ---------------------------------------------------------------------------
# Long-form 리포트 (where, what, value) 파싱
# ---------------------------------------------------------------------------

def load_report(csv_path: os.PathLike) -> pd.DataFrame:
    """리포트 CSV 를 (where, what, value) 데이터프레임으로 로드. 공백 strip + value 숫자화.

    리포트는 머리에 'Parameters' free-form 블록(컬럼 수 가변)이 붙어 있어
    python 엔진 + on_bad_lines='skip' 로 안전 파싱한다.
    값이 'NaN'/숫자가 아닌 행은 value=NaN 으로 들어간다(필요시 dropna).
    """
    df = pd.read_csv(csv_path, header=0, names=["where", "what", "value"],
                     skipinitialspace=True,
                     dtype={"where": str, "what": str, "value": str},
                     engine="python", on_bad_lines="skip")
    df["where"] = df["where"].astype(str).str.strip()
    df["what"] = df["what"].astype(str).str.strip()
    df["value"] = pd.to_numeric(df["value"], errors="coerce")
    return df


def get_scalar(df: pd.DataFrame, where: str, what: str) -> Optional[float]:
    sel = df[(df["where"] == where) & (df["what"] == what)]
    if sel.empty:
        return None
    return float(sel["value"].iloc[0])


def get_what_where_prefix(df: pd.DataFrame, where_prefix: str, what: str) -> pd.DataFrame:
    """where 가 prefix 로 시작하는 (where, value) 부분집합."""
    return df[(df["where"].str.startswith(where_prefix)) & (df["what"] == what)][["where", "value"]]


def traffic_matrix(df: pd.DataFrame, where_label: str,
                   n: int = NUM_CHIPLETS_DEFAULT) -> np.ndarray:
    """`where == where_label`, `what == "src-dst"` 행을 N×N 행렬로.

    where_label 예: 'Chip-to-chip-traced' (data) 또는 'Chip-to-chip-ptw-traced' (PTW).
    """
    mat = np.zeros((n, n), dtype=np.float64)
    sel = df[df["where"] == where_label]
    if sel.empty:
        return mat
    parts = sel["what"].str.split("-", n=1, expand=True)
    if parts.shape[1] < 2:
        return mat
    src = pd.to_numeric(parts[0], errors="coerce")
    dst = pd.to_numeric(parts[1], errors="coerce")
    val = sel["value"]
    mask = src.notna() & dst.notna() & val.notna()
    si = src[mask].astype(int).to_numpy()
    di = dst[mask].astype(int).to_numpy()
    vi = val[mask].to_numpy()
    in_range = (si >= 0) & (si < n) & (di >= 0) & (di < n)
    mat[si[in_range], di[in_range]] += vi[in_range]
    return mat


def report_path(report_dir, bench: str, config: str) -> Path:
    """run_root(run_id)/reports/<bench>_<config>.csv 경로."""
    return Path(report_dir) / f"{bench}_{config}.csv"


def memory_trace_path(trace_dir, bench: str, config: str) -> Path:
    return Path(trace_dir) / f"{bench}_{config}_memory_trace.csv"


# ---------------------------------------------------------------------------
# Bench 목록 — params.benchmarks 그대로 쓰되 graceful skip 위해 존재하는 것만 필터.
# ---------------------------------------------------------------------------

def existing_benches_in_dir(dir_path, suffix: str, all_benches: Iterable[str]) -> list[str]:
    out: list[str] = []
    for b in all_benches:
        if (Path(dir_path) / f"{b}{suffix}").exists():
            out.append(b)
    return out


def file_suffix_for_config(config_name: str) -> str:
    """트레이스 파일 suffix = '<platform>_<WxH>' (variant tag 제외).

    파일명은 runner 가 '-platform-type' + dims 로 짓는다: 'dynamic-clustering_6x6'.
    config 디렉토리는 여기에 variant 를 붙임: 'dynamic-clustering-6x6-score'.
    -> dims(WxH) 를 찾아 그 앞(platform, 내부 하이픈 유지)만 취하고 뒤 tag 는 버림.
    'mesh-6x6' -> 'mesh_6x6', 'mesh-6x6-oracle' -> 'mesh_6x6'.
    dims 없으면 예전처럼 하이픈->언더스코어."""
    m = re.search(r"\d+x\d+", config_name)
    if not m:
        return config_name.replace("-", "_")
    platform = config_name[:m.start()].rstrip("-")
    return f"{platform}_{m.group(0)}"


# ---------------------------------------------------------------------------
# 메모리 트레이스 chunk 읽기 헬퍼 — 큰 trace 메모리 절약.
# ---------------------------------------------------------------------------

DEFAULT_TRACE_DTYPES = {
    "addr": "uint64",
    "pc": "uint64",
    "owner": "int16",
    "chiplet": "int16",
    "type": "int8",
    "data_id": "int16",
    "rdma": "int8",
    "vector": "int8",
    "is_ptw": "int8",
    "rw": "category",
}

# memory_trace.csv 전 컬럼(고정 순서).  full-row drop_duplicates 용 —
# 11컬럼 전부 동일한 row(파일 쓰기 오류 중복)만 제거하고 정상 재사용은 보존.
ALL_TRACE_COLS = ["time", "addr", "pc", "owner", "rw", "rdma",
                  "vector", "type", "chiplet", "data_id", "is_ptw"]


def trace_chunks(path, usecols, chunksize=4_000_000):
    """memory trace 를 chunk 로 읽어 yield. dtype 강제로 메모리 줄임.

    is_ptw 컬럼이 'true'/'false' 문자열일 수도 있으니 호출자가 알아서 변환.
    """
    dtypes = {c: DEFAULT_TRACE_DTYPES[c] for c in usecols if c in DEFAULT_TRACE_DTYPES}
    return pd.read_csv(
        path,
        usecols=list(usecols),
        skipinitialspace=True,
        chunksize=chunksize,
        dtype=dtypes,
        low_memory=False,
    )


def filter_l2miss(df: pd.DataFrame) -> pd.DataFrame:
    """type ∈ {2,5} 인 L2 miss 행만."""
    if "type" in df.columns:
        return df[df["type"].isin([2, 5])]
    return df
