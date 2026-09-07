"""
oracle_pc_states.json v7 generator.

xlsx (`t_pc_stats_l2miss_per_owner_1.xlsx`)를 1차 소스로 하여
mgpusim/rdma/pctable.go::classify 파이프라인을 그대로 미러링한다.

v6/legacy 대비 핵심 차이:
  STEP 2 Streaming 분기를 STEP 3 base_classify 보다 먼저 적용.
  is_sync(U<4) 필터는 Streaming 이 아닌 PC 에만 적용 (broadcast
  singleton 만 떨궈낸다; matrixtranspose 본체 streaming PC 들은 살린다).

xlsx 에 없는 벤치마크는 legacy JSON 의 라벨을 그대로 승계한다.
"""

import json
import os
import sys
from pathlib import Path

import pandas as pd

ROOT = Path(__file__).resolve().parents[1]
RDMA = ROOT / "mgpusim" / "rdma"
XLSX = Path("/home/ehdgus7279/mgvm/analyze/cacheline-metrics/scripts/"
            "t_pc_stats_l2miss_per_owner_1.xlsx")
LEGACY = RDMA / "oracle_pc_states.legacy.json"
OUT_V7 = RDMA / "oracle_pc_states_v7.json"
OUT_ACTIVE = RDMA / "oracle_pc_states.json"

L2_LINES = 65536               # 4 MB / 64 B per chiplet
FANIN = {"Global": 8.0, "Hot": 8.0, "Warm": 3.0}
SHARED = ("Global", "Hot", "Warm")
STREAMING_THRESHOLD = 1.5      # reuse < 1.5 → Streaming
WARMUP_MIN_ACCESS = 100        # ReqCount < 100 → Cold (warmup gate)


def base_classify(reuse: float, sharing: float) -> str:
    if reuse < 2:
        return "Cold"
    if sharing < 2:
        if reuse < 4:
            return "Cold"
        if reuse < 15:
            return "Warm_NonShared"
        return "Hot_NonShared"
    if reuse < 4:
        return "Cold"
    if reuse >= 30 and sharing >= 10:
        return "Global"
    if reuse >= 15:
        return "Hot"
    return "Warm"


def fanin(tier: str) -> float:
    return FANIN.get(tier, 1.0)


def eff_footprint(tier: str, unique: float) -> float:
    return unique * fanin(tier)


def demote(tier: str) -> str:
    return {"Global": "Hot", "Hot": "Warm", "Warm": "Cold"}[tier]


def per_pc_clamp(tier: str, unique: float) -> str:
    if tier in SHARED:
        while tier in SHARED and eff_footprint(tier, unique) >= L2_LINES:
            tier = demote(tier)
        return tier
    if tier in ("Warm_NonShared", "Hot_NonShared"):
        return "Cold" if unique >= L2_LINES else tier
    return tier


def classify_bench(bench_df: pd.DataFrame) -> list[dict]:
    recs: list[dict] = []
    for _, r in bench_df.iterrows():
        reuse = float(r["reuse"])
        sharing = float(r["sharing"])
        unique = float(r["U"])
        access = float(r["total_access_count"])

        # STEP 1: warmup gate.
        if access < WARMUP_MIN_ACCESS:
            continue

        # STEP 2: Streaming (NEW).  Bypass is_sync filter for Streaming so
        # matrixtranspose-like benches survive.
        if reuse < STREAMING_THRESHOLD:
            recs.append(dict(pc=int(r["pc"]), U=unique, reuse=reuse,
                             sharing=sharing, base="Streaming",
                             final="Streaming"))
            continue

        # STEP 2.5: broadcast singleton filter (U<4) only for non-Streaming PCs.
        # These are kernel-arg constants that fan out trivially.
        if unique < 4.0:
            continue

        # STEP 3: base classify.
        t0 = base_classify(reuse, sharing)
        if t0 == "Cold":
            continue

        # STEP 4: per-PC footprint clamp.
        t1 = per_pc_clamp(t0, unique)
        if t1 == "Cold":
            continue

        recs.append(dict(pc=int(r["pc"]), U=unique, reuse=reuse,
                         sharing=sharing, base=t0, final=t1))

    # STEP 5: cross-PC packing on shared track.
    def ssum() -> float:
        return sum(eff_footprint(x["final"], x["U"])
                   for x in recs if x["final"] in SHARED)

    while ssum() > L2_LINES:
        cands = [x for x in recs if x["final"] in SHARED]
        if not cands:
            break
        # demote lowest-reuse SHARED PC first
        victim = min(cands, key=lambda x: x["reuse"])
        victim["final"] = demote(victim["final"])

    # STEP 6 (NonShared+hop promote) requires per-PC hop telemetry which the
    # xlsx does not carry — skipped here.  The online classify() applies it
    # on the fly when hop data is available; for the static oracle we keep
    # NonShared as-is.

    return recs


def main() -> int:
    if not XLSX.exists():
        print(f"missing xlsx: {XLSX}", file=sys.stderr)
        return 1
    if not LEGACY.exists():
        print(f"missing legacy fallback: {LEGACY}", file=sys.stderr)
        return 1

    df = pd.read_excel(XLSX).rename(columns={
        "avg_access_per_cacheline": "reuse",
        "avg_requestor_chiplets": "sharing",
        "avg_unique_addrs_per_owner": "U",
    })
    legacy = json.loads(LEGACY.read_text())

    out: dict[str, dict[str, str]] = {}
    xlsx_benches = sorted(df.benchmark.unique())
    summary = []

    for b in xlsx_benches:
        recs = classify_bench(df[df.benchmark == b])
        bj = {str(x["pc"]): x["final"]
              for x in recs if x["final"] != "Cold"}
        if bj:
            out[b] = bj
        nstream = sum(1 for x in recs if x["final"] == "Streaming")
        nshared = sum(1 for x in recs if x["final"] in SHARED)
        nns = sum(1 for x in recs
                  if x["final"] in ("Warm_NonShared", "Hot_NonShared"))
        s_pct = (sum(eff_footprint(x["final"], x["U"])
                     for x in recs if x["final"] in SHARED)
                 / L2_LINES)
        summary.append((b, len(recs), len(bj), nstream, nshared, nns, s_pct))

    # Preserve benches that the xlsx does not cover (naming drift like
    # gpt3_175B_gemm vs gpt3_gemm, or bench classes only present in legacy).
    for b, entries in legacy.items():
        if b not in xlsx_benches and entries:
            out[b] = dict(entries)

    OUT_V7.write_text(json.dumps(out, indent=2, sort_keys=True) + "\n")
    OUT_ACTIVE.write_text(json.dumps(out, indent=2, sort_keys=True) + "\n")

    print(f"wrote {OUT_V7}")
    print(f"wrote {OUT_ACTIVE}  (active)")
    print()
    print(f"{'bench':>22} {'cand':>5} {'kept':>5} "
          f"{'stream':>6} {'shared':>6} {'nshrd':>5} {'sLH/L2':>7}")
    for b, nc, nk, ns, nsh, nns, s in summary:
        print(f"{b:>22} {nc:>5} {nk:>5} {ns:>6} {nsh:>6} {nns:>5} "
              f"{s:>7.2f}")
    print()
    preserved = sorted(b for b in legacy if b not in xlsx_benches)
    print(f"preserved from legacy ({len(preserved)}): {preserved}")
    print()
    print(f"total benchmarks in active JSON: {len(out)}")
    return 0


if __name__ == "__main__":
    sys.exit(main())
