"""pc_stats_l2miss_per_owner.py 출력(xlsx/csv) 에서 PC별 hotness tier 분류 -> xlsx.

입력 테이블은 이미 pctable.go 의 (reuse, sharing) 신호를 lifetime 기준으로
per-owner 평균해 놓은 것 — 컬럼명 그대로 매핑:
  avg_access_per_addr_per_<group_col>  -> avgPerLine (reuse) = ReqCount/unique
  avg_requestor_chiplets                -> sharing            = tuples/unique

세 분류기를 rdma/pctable.go 결정 트리 기반으로 재현하되, **NonShared 트랙은
쓰지 않는다** — sharing<2(요청 칩렛이 사실상 1개)면 reuse 가 아무리 높아도
그냥 Cold. 분산(distribution)/클러스터 replication 대상이 될 수 없는 PC 라
"얼마나 hot 한 NonShared 인가"를 따로 등급 매겨봐야 이후 분석(footprint를
tier 별로 묶는 것 등)에서 쓸 데가 없다. analyze/common/pc_hotness.py(다른
스크립트들이 공유하는 헬퍼)도 원래 이 규약 — 여기만 처음에 NonShared 를
살려뒀던 게 불일치였음, thresholdClassify/scoreClassify 원본엔 있지만 이
스크립트에선 뺐다.

  tier_threshold   — thresholdClassify 기반(ClassifierThreshold).
  tier_score_nohop — scoreClassify 기반, `.claude/worktrees/decoupled-cluster-size`
                      버전. hop 항 없이 가중치 고정 0.5(reuse)/0.5(sharing) —
                      튜닝 가능한 wReuse/wShare/wHop 대신 scoreWReuse=
                      scoreWShare=0.5 상수 사용 (pctable.go:604-609 in that
                      worktree).
  tier_score_hop   — scoreClassify 기반, master 버전. hop 포함, 기본 가중치
                      (wReuse,wShare,wHop)=(0.40,0.30,0.30) — pctable.go 주석
                      "Default weights (0.40, 0.30, 0.30) yield ~72% ..."
                      hop_norm = clip((avg_hop_count-1)/(diameter-1), 0, 1),
                      diameter = 2*(n_side-1) (기본 n_side=6, xDim==yDim).

score cut 은 세 브랜치 다 동일: scoreColdShared=0.30, scoreWarm=0.50,
scoreGlobal=0.75(+sharing>=10).

워밍업 게이트: pctable.go 의 minReqCountForClassification(=100) 은 원래 PC당
"그 owner 하나"의 개별 ReqCount 로 게이트하는데, 이 입력 테이블의
total_access_count 는 owner 여러 개를 합산한 값이라 그 단위로 정확히 재현은
못 함. 다만 total_access_count(합산) < 100 이면 그 안의 모든 개별 owner
ReqCount 도 반드시 100 미만이므로(부분합 <= 전체합), 실제 시뮬에서도 100% Cold
가 되는 PC 를 걸러내는 건 정확하다 — 세 tier 컬럼 모두 이 조건에서 강제 Cold.
(반대 방향, 즉 total>=100 인데 특정 owner 만 <100 인 경우까지는 못 잡음 —
owner 별 원본 trace 재집계가 필요.)

사용:
  analyze/run.sh pc-stats/pc_hotness_from_stats.py <xlsx-or-csv> [--n-side 6] [--out path.xlsx]
"""

from __future__ import annotations

import argparse
import re
import sys
from pathlib import Path

import numpy as np
import pandas as pd

# ── rdma/pctable.go 상수 (master == decoupled-cluster-size, 안 바뀜) ────────
REUSE_COLD = 2
REUSE_WARM = 4
REUSE_HOT = 15
REUSE_GLOBAL = 30
SHARING_MULTI = 2
SHARING_GLOBAL = 10

SCORE_COLD_SHARED = 0.30
SCORE_WARM = 0.50
SCORE_GLOBAL = 0.75

MIN_REQ_COUNT = 100  # minReqCountForClassification

# decoupled-cluster-size: score 가중치 고정 0.5/0.5, hop 항 없음.
SCORE_W_NOHOP = (0.5, 0.5, 0.0)
# master: 기본 가중치 (wReuse, wShare, wHop).
SCORE_W_HOP = (0.40, 0.30, 0.30)

TIER_ORDER = ["Cold", "Warm", "Hot", "Global"]
TIER_RANK = {t: i for i, t in enumerate(TIER_ORDER)}


def threshold_classify(avg_per_line: np.ndarray, sharing: np.ndarray) -> np.ndarray:
    """rdma/pctable.go thresholdClassify 이식 — NonShared 트랙은 Cold 로 흡수
    (analyze/common/pc_hotness.py 의 _threshold_classify 와 동일 규약)."""
    out = np.full(avg_per_line.shape, "Cold", dtype=object)
    shared = sharing >= SHARING_MULTI

    out[shared & (avg_per_line >= REUSE_WARM)] = "Warm"
    out[shared & (avg_per_line >= REUSE_HOT)] = "Hot"
    out[shared & (avg_per_line >= REUSE_GLOBAL) & (sharing >= SHARING_GLOBAL)] = "Global"

    out[avg_per_line < REUSE_COLD] = "Cold"
    return out


def score_classify(avg_per_line: np.ndarray, sharing: np.ndarray, hop_norm: np.ndarray,
                   w: tuple[float, float, float]) -> tuple[np.ndarray, np.ndarray]:
    """rdma/pctable.go scoreClassify 이식. w=(wReuse,wShare,wHop) — hop 없는
    변형(decoupled-cluster-size)은 hop_norm=0 배열 + w=(0.5,0.5,0.0)로 호출.
    NonShared 트랙은 Cold 로 흡수(threshold_classify 와 동일 규약)."""
    reuse_n = np.clip(avg_per_line / REUSE_GLOBAL, 0, 1)
    share_n = np.clip(sharing / SHARING_GLOBAL, 0, 1)
    score = w[0] * reuse_n + w[1] * share_n + w[2] * hop_norm

    out = np.full(avg_per_line.shape, "Cold", dtype=object)
    shared = sharing >= SHARING_MULTI

    out[shared & (score >= SCORE_COLD_SHARED)] = "Warm"
    out[shared & (score >= SCORE_WARM) & (score < SCORE_GLOBAL)] = "Hot"
    out[shared & (score >= SCORE_GLOBAL) & (sharing >= SHARING_GLOBAL)] = "Global"
    # scoreClassify's shared-track fallthrough is Hot for any score>=scoreWarm
    # that isn't Global (including score>=scoreGlobal but sharing<SHARING_GLOBAL).
    out[shared & (score >= SCORE_GLOBAL) & (sharing < SHARING_GLOBAL)] = "Hot"

    return out, score


def _load(path: Path) -> pd.DataFrame:
    if path.suffix.lower() in (".xlsx", ".xls"):
        return pd.read_excel(path)
    return pd.read_csv(path)


def _find_col(df: pd.DataFrame, pattern: str) -> str:
    for c in df.columns:
        if re.fullmatch(pattern, c):
            return c
    raise SystemExit(f"no column matching /{pattern}/ in {df.columns.tolist()}")


def classify(df: pd.DataFrame, n_side: int, min_req: int = MIN_REQ_COUNT) -> pd.DataFrame:
    reuse_col = _find_col(df, r"avg_access_per_addr_per_\w+")
    hop_col = _find_col(df, r"avg_hop_count_per_\w+")
    sharing_col = "avg_requestor_chiplets"

    reuse = df[reuse_col].astype(np.float64).to_numpy()
    sharing = df[sharing_col].astype(np.float64).to_numpy()
    diam = max(2 * (n_side - 1), 2)
    hop_norm = np.clip((df[hop_col].astype(np.float64).to_numpy() - 1) / (diam - 1), 0, 1)

    tier_threshold = threshold_classify(reuse, sharing)
    tier_nohop, score_nohop = score_classify(reuse, sharing, np.zeros_like(reuse), SCORE_W_NOHOP)
    tier_hop, score_hop = score_classify(reuse, sharing, hop_norm, SCORE_W_HOP)

    # minReqCountForClassification 게이트: total_access_count(owner 합산) < min_req
    # 이면 그 안의 모든 owner ReqCount 도 반드시 < min_req -> 실제 시뮬은 무조건 Cold.
    total_access = df["total_access_count"].to_numpy() if "total_access_count" in df.columns \
        else np.full(reuse.shape, np.inf)
    gated = total_access < min_req
    tier_threshold = np.where(gated, "Cold", tier_threshold)
    tier_nohop = np.where(gated, "Cold", tier_nohop)
    tier_hop = np.where(gated, "Cold", tier_hop)

    out = df[["benchmark", "pc"]].copy()
    out["reuse"] = reuse
    out["sharing"] = sharing
    out["hop_norm"] = np.round(hop_norm, 4)
    out["gated_min_req"] = gated
    out["tier_threshold"] = tier_threshold
    out["score_nohop"] = np.round(score_nohop, 4)
    out["tier_score_nohop"] = tier_nohop
    out["score_hop"] = np.round(score_hop, 4)
    out["tier_score_hop"] = tier_hop
    out["rank_score_hop"] = [TIER_RANK[t] for t in tier_hop]
    for c in ("total_access_count", "total_unique_addrs", "data_ids"):
        if c in df.columns:
            out[c] = df[c]
    return out.sort_values(["benchmark", "rank_score_hop", "reuse"], ascending=[True, False, False])


def main():
    ap = argparse.ArgumentParser(description=__doc__,
                                 formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("stats_path", help="pc_stats_l2miss_per_owner.py 출력 (.xlsx / .csv)")
    ap.add_argument("--n-side", type=int, default=6, help="mesh side length (default 6, xDim==yDim)")
    ap.add_argument("--min-req", type=int, default=MIN_REQ_COUNT,
                    help=f"minReqCountForClassification, 기본 {MIN_REQ_COUNT} (pctable.go 기본값). "
                         "낮추면 총 접근이 적어 Cold로 게이트되던 PC가 더 많이 풀림.")
    ap.add_argument("--out", default=None, help="출력 xlsx 경로 (기본: 입력 옆에 _hotness.xlsx)")
    args = ap.parse_args()

    path = Path(args.stats_path)
    if not path.exists():
        sys.exit(f"not found: {path}")

    df = _load(path)
    result = classify(df, args.n_side, args.min_req)

    out_path = Path(args.out) if args.out else path.with_name(path.stem + "_hotness.xlsx")
    result.to_excel(out_path, index=False, sheet_name="pc_hotness")

    print(f"wrote {out_path}  ({len(result)} rows)")
    n_gated = int(result["gated_min_req"].sum())
    print(f"gated to Cold by minReqCount<{args.min_req}: {n_gated}/{len(result)}")
    print()
    for col, label in (("tier_threshold", "threshold"),
                       ("tier_score_nohop", "score, no hop (decoupled-cluster-size)"),
                       ("tier_score_hop", "score, with hop (master, 0.40/0.30/0.30)")):
        print(f"tier ({label}):")
        print(result.groupby(["benchmark", col]).size().unstack(fill_value=0)
             .reindex(columns=TIER_ORDER, fill_value=0).to_string())
        print()


if __name__ == "__main__":
    main()
