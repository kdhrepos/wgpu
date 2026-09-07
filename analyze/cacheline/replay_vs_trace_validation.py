"""host replay 결과 vs timing 트레이스 대조 검증.

목적: mgpusim/cmd/footprint 의 host replay 가 실제 timing 실행과 같은 공유 구조를
내는지 확인. **끝까지 돈(=truncate 안 된) 실행에서만 의미가 있다** — max-inst 로
잘린 트레이스와 비교하면 당연히 replay 쪽이 크게 나오고, 그건 버그가 아니라 애초에
이 도구를 만든 이유다.

비교 대상 = 공유도 히스토그램 (chiplet 개수별 라인 수).
절대 주소는 비교하지 않는다: 트레이스는 physical, replay 는 virtual 이고, 변환이
page 정렬 단사라 cacheline 은 일대일이지만 라벨은 다르다. 반면 "몇 개의 라인이 몇
개의 chiplet 에 걸쳐 있는가"는 라벨과 무관하므로 그대로 대조된다.

트레이스가 L2 directory stage 에서 나온다는 점(=L1 hit 은 안 보임)은 (line,chiplet)
집합에는 영향이 없다. 어떤 chiplet 이 라인을 처음 만질 때는 반드시 자기 L1 을
miss 하고 L2 로 내려오기 때문. 접근 '횟수'는 다르며, 그건 비교하지 않는다.

알려진 오염원:
  - H2D 복사: 트레이스에 잡힌다. timing 실행에 -fast-h2d 를 주면 사라진다.
  - D2H 복사: 끌 수 없다. 출력 버퍼 라인을 복사 work-group 이 만지므로 그 버퍼의
    공유도가 replay 보다 높게 나온다. --exclude-data-id 로 해당 버퍼를 빼거나,
    replay summary.json 의 buffers 항목으로 크기를 확인해 델타를 해석할 것.

사용:
  analyze/run.sh cacheline/replay_vs_trace_validation.py <run-id> \\
      --benchmark pagerank --footprint-dir ~/mgvm/results/footprint
"""

from __future__ import annotations

import argparse
import json
import os
import sys
from pathlib import Path

import numpy as np
import pandas as pd

from _paths import memory_trace_dir, run_config
from motivation_common import file_suffix_for_config, trace_chunks, ALL_TRACE_COLS
from pc_hotness import mesh_dims_from_config

DEMAND_TYPES = np.array([0, 1, 2, 3, 4, 5], dtype=np.int8)
LINE_BYTES = 64


def trace_hist(trace_file: str, n_chip: int, exclude_data_id: set[int]) -> np.ndarray:
    """트레이스에서 (line -> 접근한 chiplet 집합) 을 만들고 공유도 히스토그램 반환."""
    parts: list[pd.Series] = []
    for chunk in trace_chunks(trace_file, usecols=ALL_TRACE_COLS):
        chunk = chunk.drop_duplicates()
        types = chunk["type"].to_numpy(dtype=np.int8)
        is_ptw = chunk["is_ptw"].to_numpy(dtype=np.int8)
        keep = np.isin(types, DEMAND_TYPES) & (is_ptw == 0)
        if exclude_data_id:
            data_id = chunk["data_id"].to_numpy(dtype=np.int64)
            keep &= ~np.isin(data_id, list(exclude_data_id))
        if not keep.any():
            continue
        line = chunk["addr"].to_numpy(dtype=np.uint64)[keep] >> np.uint64(6)
        chip = chunk["chiplet"].to_numpy(dtype=np.int64)[keep]
        parts.append(
            pd.DataFrame({"line": line, "chip": chip})
            .groupby(["line", "chip"], sort=False).size()
        )

    hist = np.zeros(n_chip + 1, dtype=np.int64)
    if not parts:
        return hist

    edges = pd.concat(parts).groupby(level=[0, 1]).sum()
    nchip = edges.groupby(level=0).size().to_numpy()
    counts = np.bincount(nchip, minlength=n_chip + 1)

    return counts[: n_chip + 1]


def replay_hist(footprint_dir: str, bench: str, n_side: int) -> tuple[np.ndarray, dict]:
    path = Path(footprint_dir) / f"{bench}_{n_side}x{n_side}" / "summary.json"
    if not path.exists():
        sys.exit(f"no replay summary at {path}")
    summary = json.loads(path.read_text())

    return np.array(summary["lines_per_nchip"], dtype=np.int64), summary


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("run_id", nargs="?", default="mesh-6x6/latest")
    ap.add_argument("--benchmark", required=True)
    ap.add_argument("--footprint-dir", required=True)
    ap.add_argument("--exclude-data-id", default="",
                    help="쉼표 구분 data_id 목록. D2H 오염된 출력 버퍼 제외용.")
    args = ap.parse_args()

    config = run_config(args.run_id)
    n_side, _ = mesh_dims_from_config(config)
    n_chip = n_side * n_side

    trace_file = (f"{memory_trace_dir(args.run_id)}/"
                  f"{args.benchmark}_{file_suffix_for_config(config)}"
                  f"_memory_trace.csv")
    if not os.path.exists(trace_file):
        sys.exit(f"no trace at {trace_file}")

    exclude = {int(x) for x in args.exclude_data_id.split(",") if x.strip()}

    t_hist = trace_hist(trace_file, n_chip, exclude)
    r_hist, summary = replay_hist(args.footprint_dir, args.benchmark, n_side)

    if r_hist.size != t_hist.size:
        sys.exit(f"chiplet count mismatch: replay {r_hist.size - 1}, "
                 f"trace {t_hist.size - 1}")

    print(f"config={config}  benchmark={args.benchmark}  chiplets={n_chip}")
    if summary.get("buffers"):
        print("replay buffers:")
        for b in summary["buffers"]:
            print(f"  {b['name']:<16} {b['size'] / 1024 ** 2:9.2f} MB")

    print(f"\n{'nchip':>6} {'trace':>14} {'replay':>14} {'delta':>14}")
    for k in range(1, n_chip + 1):
        if t_hist[k] == 0 and r_hist[k] == 0:
            continue
        print(f"{k:6d} {t_hist[k]:14d} {r_hist[k]:14d} "
              f"{int(r_hist[k]) - int(t_hist[k]):+14d}")

    t_shared, r_shared = int(t_hist[2:].sum()), int(r_hist[2:].sum())
    to_mb = LINE_BYTES / 1024 ** 2
    print(f"\nshared lines   trace {t_shared:12d} ({t_shared * to_mb:8.2f} MB)")
    print(f"               replay{r_shared:12d} ({r_shared * to_mb:8.2f} MB)")
    if t_shared:
        print(f"               replay/trace = {r_shared / t_shared:.3f}x")

    if t_shared == r_shared and np.array_equal(t_hist[2:], r_hist[2:]):
        print("\nMATCH: sharing-degree histogram identical.")
    else:
        print("\nDIFFER. Check, in order: (1) was the timing run truncated by "
              "-max-inst? (2) was -fast-h2d set? (3) is the D2H-touched output "
              "buffer excluded? (4) do -matrix-seed and -zipf-s match?")


if __name__ == "__main__":
    main()
