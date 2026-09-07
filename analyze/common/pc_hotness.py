"""PC hotness 재현 — memory trace 에서 `rdma/pctable.go` 의 tier 분류를 그대로 복제.

두 motivation 스크립트(`pc_hotness_signature`, `premature_hot_eviction`)가 "hot PC"
정의를 **동일하게** 공유하도록 여기 한 곳에 둔다.  분류는 pctable.go 의
thresholdClassify + (옵션) clampByFootprint 를 미러링.

**shared track 만 사용**: sharing>=2 인 PC 만 Warm/Hot/Global 로 분류하고, NonShared
(sharing<2)는 reuse 가 아무리 높아도 Cold 로 흡수한다.  같은 이유로 NonShared 를
shared tier 로 끌어올리던 hop 승격(promoteNonSharedByHop)도 제거.  tier = {Cold, Warm,
Hot, Global}.  hop_norm 은 tier 엔 안 쓰이고 hotness_score 의 가중합에만 반영.

신호 정의 (pctable.go 와 동일, 정수 나눗셈 floor 까지 일치):
  reuse   = floor(req_count / unique)          # 캐시라인당 평균 접근 = 재사용 강도
  sharing = floor(tuples   / unique)           # 캐시라인당 평균 접근 칩렛 수 = 공유 폭
  unique  = distinct 캐시라인 수
  tuples  = distinct (캐시라인, 요청자 칩렛) 쌍 수
  hop_norm= clamp((avg_manhattan-1)/(diameter-1), 0, 1)

sim 과의 차이 (문서화):
  - sim 의 PCTable 은 per-owner + 원격요청(ReqFromOutside)만 집계.  여기선 한 PC 를
    전 칩렛에 걸쳐 global 로 집계(요청자=chiplet 컬럼) → PC 당 점 1개.  motivation 의
    "PC 가 특성을 가른다" 주장엔 global 집계가 더 직접적.
  - footprint clamp 는 sim 에서 windowed working-set(WSAddrs, rank-decay)을 쓰지만
    trace 로는 lifetime unique 만 복원 가능 → clamp 은 기본 OFF.  clamp 은 "용량이
    hot 을 못 담는다"는 Motivation 2 의 경험적 주제로 분리해서 보인다.
"""

from __future__ import annotations

import re
from dataclasses import dataclass

import numpy as np
import pandas as pd

# ── tier 라벨 / 순위 / 색 ────────────────────────────────────────────────────
# shared track(sharing>=2)만 사용.  NonShared(Warm_NS/Hot_NS)는 제외 = Cold 로 흡수.
COLD = "Cold"
WARM = "Warm"
HOT = "Hot"
GLOBAL = "Global"

# hotnessRank — 낮을수록 cold.
TIER_RANK = {COLD: 0, WARM: 1, HOT: 2, GLOBAL: 3}

# scatter 색 — cold 는 회색, shared track 은 Okabe-Ito warm 계열.
TIER_COLOR = {
    COLD:    "#bdbdbd",
    WARM:    "#E69F00",
    HOT:     "#D55E00",
    GLOBAL:  "#9e0142",
}
TIER_ORDER = [COLD, WARM, HOT, GLOBAL]

# "hot line" OR 규칙에서 hot 으로 칠 tier 기본값.  shared track 의 재사용-hot 두 tier.
DEFAULT_HOT_TIERS = frozenset({HOT, GLOBAL})

# ── pctable.go 상수 미러 ─────────────────────────────────────────────────────
REUSE_COLD = 2
REUSE_WARM = 4
REUSE_HOT = 15
REUSE_GLOBAL = 30
SHARING_MULTI = 2
SHARING_GLOBAL = 10
FOOTPRINT_L2_LINES = 65536          # 4 MB / 64 B
SCORE_W = (0.40, 0.30, 0.30)        # (reuse, share, hop) 기본 가중치


def mesh_dims_from_config(config_name: str) -> tuple[int, int]:
    """'mesh-6x6' / 'mesh_6x6' → (xdim=6, num_chiplets=36).  기본 6x6."""
    m = re.search(r"(\d+)x(\d+)", config_name)
    if not m:
        return 6, 36
    x, y = int(m.group(1)), int(m.group(2))
    return x, x * y


# ── cluster geometry (dynamic_clustering_rdmaengine.go clusterDims) ──────────
def _smallest_divisor_above(n: int, k: int) -> int:
    for d in range(k + 1, n + 1):
        if n % d == 0:
            return d
    return n


def cluster_dims(tier: str, n_side: int) -> tuple[int, int]:
    if tier == GLOBAL:
        return 2, 1
    if tier == HOT:
        return 2, 2
    if tier == WARM:
        return 2, _smallest_divisor_above(n_side, 2)
    return 0, 0


def fan_in_for_tier(tier: str, n_side: int, num_chiplets: int) -> int:
    wx, wy = cluster_dims(tier, n_side)
    if wx <= 0:
        return 1
    cluster = wx * wy
    if cluster >= num_chiplets:
        return 1
    return (num_chiplets - cluster) // cluster


# ── classify 파이프라인 ──────────────────────────────────────────────────────
def _threshold_classify(reuse_i: np.ndarray, sharing_i: np.ndarray) -> np.ndarray:
    """벡터화 thresholdClassify.  shared track(sharing>=2)만 — NonShared 는 Cold."""
    out = np.full(reuse_i.shape, COLD, dtype=object)
    shared = sharing_i >= SHARING_MULTI

    out[shared & (reuse_i >= REUSE_WARM)] = WARM
    out[shared & (reuse_i >= REUSE_HOT)] = HOT
    out[shared & (reuse_i >= REUSE_GLOBAL) & (sharing_i >= SHARING_GLOBAL)] = GLOBAL

    # reuse < 2 는 무조건 Cold (위 조건들이 reuse>=4 를 요구하므로 이미 Cold, 명시)
    out[reuse_i < REUSE_COLD] = COLD
    return out


def _demote_one(tier: str) -> str:
    return {GLOBAL: HOT, HOT: WARM, WARM: COLD}.get(tier, tier)


def _clamp_by_footprint(tier: str, ws_unique: float, n_side: int, num: int) -> str:
    t = tier
    while t in (WARM, HOT, GLOBAL):
        if ws_unique * fan_in_for_tier(t, n_side, num) < FOOTPRINT_L2_LINES:
            break
        t = _demote_one(t)
    return t


def hotness_score(reuse: np.ndarray, sharing: np.ndarray, hop_norm: np.ndarray,
                  w: tuple[float, float, float] = SCORE_W) -> np.ndarray:
    """단일 hotness score (scoreClassify 의 가중합).  정렬-곡선용 연속 축이라 float 신호 사용."""
    reuse_n = np.clip(reuse / REUSE_GLOBAL, 0, 1)
    share_n = np.clip(sharing / SHARING_GLOBAL, 0, 1)
    hop_n = np.clip(hop_norm, 0, 1)
    return w[0] * reuse_n + w[1] * share_n + w[2] * hop_n


# ── per-PC 신호 집계 ─────────────────────────────────────────────────────────
@dataclass
class MeshGeom:
    n_side: int
    num: int

    @property
    def diameter(self) -> int:
        return 2 * (self.n_side - 1)


def manhattan_hop(owner: np.ndarray, chiplet: np.ndarray, n_side: int) -> np.ndarray:
    ox, oy = owner % n_side, owner // n_side
    cx, cy = chiplet % n_side, chiplet // n_side
    return np.abs(ox - cx) + np.abs(oy - cy)


def aggregate_pc_signals(df: pd.DataFrame, geom: MeshGeom) -> pd.DataFrame:
    """demand 행(이미 필터됨: type 0-5, is_ptw==0, pc!=0)에서 PC 별 신호 집계.

    입력 컬럼: pc, cacheline, chiplet, owner.
    반환: pc 인덱스, [req_count, unique, tuples, reuse, sharing, avg_hop, hop_norm].
    """
    g = df.groupby("pc")
    req = g.size().rename("req_count")
    unique = g["cacheline"].nunique().rename("unique")

    tup = (df[["pc", "cacheline", "chiplet"]]
           .drop_duplicates()
           .groupby("pc").size().rename("tuples"))

    hop = manhattan_hop(df["owner"].to_numpy(), df["chiplet"].to_numpy(), geom.n_side)
    avg_hop = (pd.Series(hop, index=df.index)
               .groupby(df["pc"]).mean().rename("avg_hop"))

    sig = pd.concat([req, unique, tup, avg_hop], axis=1).fillna(0)
    sig["unique"] = sig["unique"].clip(lower=1)
    # floor 정수 나눗셈 = sim 과 동일
    sig["reuse"] = (sig["req_count"] // sig["unique"]).astype(np.int64)
    sig["sharing"] = (sig["tuples"] // sig["unique"]).astype(np.int64)
    diam = geom.diameter if geom.diameter > 1 else 2
    sig["hop_norm"] = np.clip((sig["avg_hop"] - 1) / (diam - 1), 0, 1)
    return sig


class PCAccum:
    """청크 스트리밍 per-PC 신호 누적기 (거대 trace 대비, 메모리 bounded-ish).

    trace 가 수십 GB 라 full-load 불가.  청크마다 (pc, cacheline) 별 chiplet 을
    uint64 비트마스크로 OR 누적하고, pc 별 req_count / hop 합을 별도 누적.
    `pc_predicts_characteristics.py` 의 dict 누적 패턴과 동형이나 chiplet 을 set 대신
    비트마스크로 들고 있어 더 가볍다 (칩렛 ≤ 64 가정, wafer-scale 타깃 충족).

    최종 finalize() 로 per-PC sig 프레임을 만들고, hot_cachelines() 로 hot PC 가
    touch 한 캐시라인 집합(OR 규칙)을 그 누적 dict 에서 바로 유도.
    """

    def __init__(self, geom: MeshGeom):
        self.geom = geom
        self.mask: dict[tuple[int, int], int] = {}   # (pc, cacheline) -> chiplet 비트마스크
        self.req: dict[int, int] = {}                # pc -> demand 접근 수
        self.hopsum: dict[int, float] = {}           # pc -> hop 합

    def update(self, pc: np.ndarray, cacheline: np.ndarray,
               chiplet: np.ndarray, owner: np.ndarray) -> None:
        sub = pd.DataFrame({"pc": pc, "cl": cacheline, "chip": chiplet})
        # (pc, cl) 별 chiplet 비트마스크
        bit = (np.uint64(1) << sub["chip"].to_numpy(dtype=np.uint64))
        sub["bit"] = bit
        gb = sub.groupby(["pc", "cl"])["bit"].agg(
            lambda s: int(np.bitwise_or.reduce(s.to_numpy(dtype=np.uint64))))
        mask = self.mask
        for key, b in gb.items():
            mask[key] = mask.get(key, 0) | int(b)

        # pc 별 req / hop
        hop = manhattan_hop(owner, chiplet, self.geom.n_side)
        hsum = pd.Series(hop).groupby(pc).sum()
        rcnt = pd.Series(np.ones(len(pc), dtype=np.int64)).groupby(pc).sum()
        for p, c in rcnt.items():
            self.req[p] = self.req.get(p, 0) + int(c)
        for p, h in hsum.items():
            self.hopsum[p] = self.hopsum.get(p, 0.0) + float(h)

    def finalize(self) -> pd.DataFrame:
        if not self.mask:
            return pd.DataFrame(
                columns=["req_count", "unique", "tuples", "avg_hop",
                         "reuse", "sharing", "hop_norm"])
        uniq: dict[int, int] = {}
        tup: dict[int, int] = {}
        for (pc, _cl), b in self.mask.items():
            uniq[pc] = uniq.get(pc, 0) + 1
            tup[pc] = tup.get(pc, 0) + int(b).bit_count()

        pcs = list(uniq.keys())
        req = np.array([self.req.get(p, 0) for p in pcs], dtype=np.int64)
        unique = np.array([uniq[p] for p in pcs], dtype=np.int64).clip(min=1)
        tuples = np.array([tup[p] for p in pcs], dtype=np.int64)
        avg_hop = np.array([self.hopsum.get(p, 0.0) / max(1, self.req.get(p, 0))
                            for p in pcs], dtype=np.float64)

        diam = self.geom.diameter if self.geom.diameter > 1 else 2
        sig = pd.DataFrame({
            "req_count": req,
            "unique": unique,
            "tuples": tuples,
            "avg_hop": avg_hop,
            "reuse": (req // unique).astype(np.int64),
            "sharing": (tuples // unique).astype(np.int64),
            "hop_norm": np.clip((avg_hop - 1) / (diam - 1), 0, 1),
        }, index=pd.Index(pcs, name="pc"))
        return sig

    def hot_cachelines(self, hot_pc_set) -> set:
        """hot PC 가 touch 한 캐시라인 집합 (OR 규칙) — 누적 dict 에서 바로 유도."""
        hp = set(hot_pc_set)
        return {cl for (pc, cl) in self.mask.keys() if pc in hp}


def classify_signals(sig: pd.DataFrame, geom: MeshGeom,
                     footprint_clamp: bool = False) -> pd.DataFrame:
    """aggregate_pc_signals 결과에 tier / score 컬럼 추가."""
    reuse_i = sig["reuse"].to_numpy(dtype=np.int64)
    sharing_i = sig["sharing"].to_numpy(dtype=np.int64)
    hop_n = sig["hop_norm"].to_numpy(dtype=np.float64)

    tiers = _threshold_classify(reuse_i, sharing_i)

    if footprint_clamp:
        uniq = sig["unique"].to_numpy(dtype=np.float64)
        tiers = np.array([_clamp_by_footprint(t, u, geom.n_side, geom.num)
                          for t, u in zip(tiers, uniq)], dtype=object)

    out = sig.copy()
    out["tier"] = tiers
    out["rank"] = [TIER_RANK[t] for t in tiers]
    out["score"] = hotness_score(
        sig["reuse"].to_numpy(dtype=np.float64),
        sig["sharing"].to_numpy(dtype=np.float64),
        hop_n,
    )
    return out


def hot_pcs(classified: pd.DataFrame, hot_tiers=DEFAULT_HOT_TIERS) -> np.ndarray:
    """hot tier 로 분류된 PC 목록."""
    mask = classified["tier"].isin(set(hot_tiers))
    return classified.index[mask].to_numpy()
