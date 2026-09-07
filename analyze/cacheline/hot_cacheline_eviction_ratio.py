"""Hotness 등급별 evict/overwrite 비율.

질문: "전체 cacheline 중 hot 라인이 얼마나 자주 evict/overwrite 되는가?
       PC 무관, cacheline 자체의 등급으로."

분류 기준 (cacheline 단위, type 0~5 demand access 기반):
  - access_count   : 그 cacheline 에 demand access 가 발생한 총 횟수
  - share_chiplets : 그 cacheline 에 접근한 unique chiplet 수

  Global  : share_chiplets >= --global-share (default = num_chiplets // 2)
  Hot     : 위 외, access_count >= 상위 --hot-pct % 분위 (default top 5%)
  Warm    : 위 외, access_count >= 상위 --warm-pct % 분위 (default top 25%)
  Cold    : 나머지

  Global 이 다른 등급보다 우선 (cross-chiplet 공유는 access count 와 별개 신호).

본 트레이스 포맷의 한계:
  master 포맷 type 6/7 의 addr 는 **evictor** (밀려난 victim 아님). 그래서
  "evict 당했다" 의 직접 측정은 불가. 본 스크립트는 차선책으로 "그 cacheline 이
  evictor 로 등장했다" = "그 라인이 cache miss 로 다시 들어오느라 누구를 밀어냈다"
  = "indirect proxy for thrashing pressure on that line" 로 본다.

  진짜 victim 분석은 새 type 8/9 트레이스 (cache.Block.InstallPC, 2026-06-23
  도입) 로 sweep 다시 돌려야 함. 참고: reference_memory_trace_format 메모.

사용:
  analyze/run.sh cacheline/hot_cacheline_eviction_ratio.py <trace.csv>
  analyze/run.sh cacheline/hot_cacheline_eviction_ratio.py <trace.csv> \\
      --hot-pct 5 --warm-pct 25 --global-share 18

출력:
  <trace_dir>/<base>_hot_cacheline_eviction_ratio.xlsx
  콘솔 — 등급별 summary
"""

import argparse
import os
import sys
import numpy as np
import pandas as pd

LINE_BYTES = 64
DEMAND_TYPES = {0, 1, 2, 3, 4, 5}
EVICT_TYPES = {8, 9}
DEFAULT_NUM_CHIPLETS = 36


def classify(access_count, share_chiplets, hot_count_thr, warm_count_thr, global_share_thr):
    """벡터 분류. 입력 series 두 개, 출력 같은 길이의 카테고리 series."""
    out = np.full(len(access_count), 'Cold', dtype=object)
    out[access_count >= warm_count_thr] = 'Warm'
    out[access_count >= hot_count_thr] = 'Hot'
    out[share_chiplets >= global_share_thr] = 'Global'   # override
    return out


def analyze(trace_path, hot_pct, warm_pct, global_share):
    print(f"읽는 중: {trace_path}", flush=True)
    df = pd.read_csv(
        trace_path,
        dtype={'addr': 'uint64', 'chiplet': 'int16',
               'type': 'int8', 'is_ptw': 'int8'},
        skipinitialspace=True,
    )
    df = df.drop_duplicates()
    df = df[['addr', 'chiplet', 'type', 'is_ptw']]
    print(f"전체 row: {len(df):,}", flush=True)

    df = df[df['is_ptw'] == 0]
    df['cacheline'] = df['addr'].to_numpy() >> 6

    demand = df[df['type'].isin(DEMAND_TYPES)]
    evict = df[df['type'].isin(EVICT_TYPES)]

    print(f"demand access (0~5): {len(demand):,}")
    print(f"evict/overwrite (6/7): {len(evict):,}")

    # per-cacheline 통계
    g = demand.groupby('cacheline', sort=False)
    access_count = g.size()
    share_chiplets = g['chiplet'].nunique()

    per_line = pd.DataFrame({
        'access_count': access_count,
        'share_chiplets': share_chiplets,
    }).fillna(0)

    # cacheline 별 evictor 등장 횟수
    if not evict.empty:
        evict_count = evict.groupby('cacheline').size()
        per_line['evict_trigger_count'] = evict_count
    else:
        per_line['evict_trigger_count'] = 0
    per_line['evict_trigger_count'] = per_line['evict_trigger_count'].fillna(0).astype('int64')
    per_line['was_evict_trigger'] = per_line['evict_trigger_count'] > 0

    # 임계값 = 백분위
    if len(per_line) > 0:
        hot_thr = np.percentile(per_line['access_count'],  100 - hot_pct)
        warm_thr = np.percentile(per_line['access_count'], 100 - warm_pct)
    else:
        hot_thr = warm_thr = 0

    print(f"\nhot threshold  (top {hot_pct}%):  access_count >= {hot_thr:.0f}")
    print(f"warm threshold (top {warm_pct}%): access_count >= {warm_thr:.0f}")
    print(f"global threshold:                 share_chiplets >= {global_share}")

    per_line['hotness'] = classify(
        per_line['access_count'].to_numpy(),
        per_line['share_chiplets'].to_numpy(),
        hot_thr, warm_thr, global_share,
    )

    # 등급별 집계
    summary_rows = []
    for level in ['Global', 'Hot', 'Warm', 'Cold']:
        sub = per_line[per_line['hotness'] == level]
        n = len(sub)
        n_trig = int(sub['was_evict_trigger'].sum())
        trig_events = int(sub['evict_trigger_count'].sum())
        accs = int(sub['access_count'].sum())
        summary_rows.append({
            'hotness': level,
            'unique_cachelines': n,
            'evict_triggered_cachelines': n_trig,
            'cacheline_trigger_ratio': round(n_trig / n, 6) if n else 0.0,
            'total_evict_trigger_events': trig_events,
            'total_demand_accesses': accs,
            'event_trigger_ratio': round(trig_events / accs, 6) if accs else 0.0,
        })
    summary = pd.DataFrame(summary_rows)

    # save
    out_dir = os.path.dirname(os.path.abspath(trace_path))
    base = os.path.splitext(os.path.basename(trace_path))[0]
    xlsx = os.path.join(out_dir, f"{base}_hot_cacheline_eviction_ratio.xlsx")
    with pd.ExcelWriter(xlsx) as w:
        summary.to_excel(w, sheet_name='summary', index=False)
        per_line.sort_values(
            ['hotness', 'evict_trigger_count'], ascending=[True, False]
        ).to_excel(w, sheet_name='per_cacheline')

    print("\n" + "=" * 100)
    print("Hotness 등급별 evict/overwrite 비율")
    print("=" * 100)
    print(summary.to_string(index=False))
    print("=" * 100)
    print(f"\ntable: {xlsx}")
    print("cacheline_trigger_ratio = 그 등급 라인 중 evictor 로 한 번이라도 등장한 비율")
    print("event_trigger_ratio     = 그 등급 라인의 access 중 evict-trigger 비율")

    if evict.empty:
        print("\n!!! type 6/7 row 0개 → 모든 비율 0. 더 큰 input 또는 작은 cache 로 다시 sweep 필요.")


if __name__ == "__main__":
    ap = argparse.ArgumentParser(description=__doc__.split('\n')[0])
    ap.add_argument('trace', help="memory_trace.csv 경로")
    ap.add_argument('--hot-pct', type=float, default=5.0,
                    help="Hot 등급 access_count 상위 백분위 (default 5)")
    ap.add_argument('--warm-pct', type=float, default=25.0,
                    help="Warm 등급 access_count 상위 백분위 (default 25)")
    ap.add_argument('--global-share', type=int,
                    default=DEFAULT_NUM_CHIPLETS // 2,
                    help=f"Global 등급 share_chiplets 임계값 (default {DEFAULT_NUM_CHIPLETS//2})")
    args = ap.parse_args()
    if not os.path.exists(args.trace):
        print(f"파일 없음: {args.trace}", file=sys.stderr)
        sys.exit(1)
    if not (0 < args.hot_pct <= args.warm_pct <= 100):
        ap.error("--hot-pct < --warm-pct <= 100 여야 함")
    analyze(args.trace, args.hot_pct, args.warm_pct, args.global_share)
