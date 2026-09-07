"""
(pc, owner)별로 메모리 접근 address들이 cache의 같은 set으로 얼마나 몰리는지 계산.

같은 (pc, owner) 그룹에서 가장 많이 hit하는 set의 access 비율(=same-set ratio)을 구함.
1.0에 가까울수록 모든 접근이 한 set에 몰리는 것 → conflict miss 유발 가능성 높음.
"""

import os
username = os.path.expanduser('~')
import pandas as pd

from _paths import memory_trace_dir

# === 설정 ===
_run_id = next((a for a in __import__("sys").argv[1:] if not a.startswith("-")), "latest")
from _paths import run_root as _run_root, run_tag as _run_tag
date = _run_tag(_run_id)
LINE_SIZE = 64
NUM_SETS = 4096

benchmarks = [
    # Low-sharing Apps
    "fdtd2d", "jacobi1d", "fastwalshtransform", "relu", "fft", "fir",
    "sssp", "convolution2d", "color", "bitonicsort", "pagerank", "simpleconvolution",
    # High-sharing Apps
    'gups', 'syr2k', 'spmv', 'mis', 'stencil2d', 'gemm', 'lu',
    'matrixmultiplication', 'mm2', 'mm3', 'gaussian', 'matrixtranspose',
    'im2col', 'vgg16', 'bfs',
]


def compute_same_set_ratio(df, benchmark):
    df['set'] = (df['addr'] // LINE_SIZE) % NUM_SETS

    g = df.groupby(['pc', 'owner'])
    total = g.size().rename('total_accesses')
    unique = g['set'].nunique().rename('unique_sets')
    dominant = g['set'].agg(lambda s: s.value_counts().iloc[0]).rename('dominant_set_count')

    out = pd.concat([total, unique, dominant], axis=1).reset_index()
    out['same_set_ratio'] = out['dominant_set_count'] / out['total_accesses']
    out.insert(0, 'benchmark', benchmark)
    return out


if __name__ == "__main__":
    expanded_dir = f"{memory_trace_dir(date)}/expanded"

    out_dir = f"{username}/mgvm/analyze/_out/pc/same-set-ratio/{date}"
    os.makedirs(out_dir, exist_ok=True)

    all_results = []
    for benchmark in benchmarks:
        trace_file = f"{expanded_dir}/{benchmark}_mesh_6x6_memory_trace.csv"

        if not os.path.exists(trace_file):
            print(f"Skip: No {trace_file}")
            continue

        print(f"Reading {benchmark}")
        df = pd.read_csv(trace_file, skipinitialspace=True)
        df = df.drop_duplicates()
        df = df[['addr', 'pc', 'owner']]
        result = compute_same_set_ratio(df, benchmark)
        all_results.append(result)

    if not all_results:
        print("No results.")
        raise SystemExit(0)

    combined = pd.concat(all_results, ignore_index=True)
    out_file = f"{out_dir}/same_set_ratio_per_pc.csv"
    combined.to_csv(out_file, index=False)
    print(f"Saved: {out_file} ({len(combined)} rows)")
