"""Hot AND chiplet-shared cacheline 들이 얼마나 무분별하게 evict / overwrite 되는가.

기존 스크립트들과의 차이:
  - top_hot_evicted_ratio.py        : hot 만 봄, chiplet 무관, "한 번이라도 evict?" 이진
  - hot_data_eviction_share.py      : hot evict-share / access-share 리프트, chiplet 무관
  - hot_cacheline_eviction_ratio.py : Hotness 등급, evictor 만 다룸 (victim 측정 X)

본 스크립트는 (hot AND shared) 교집합을 골라서, **각 라인이 자기 거주 칩렛들 중
얼마나 많은 곳에서 쫓겨났는지 (chiplet_evict_coverage), 거주지당 평균 추방 횟수
(evicts_per_residency)** 를 본다. = "중요한 라인이 시스템 전반에서 thrashing
되는가" 의 직접 측정.

질문 풀이:
  Q1. "한 hot 라인이 chip0서 2회, chip1서 3회 evict 되면?"
      => (line, chip0)=2, (line, chip1)=3 으로 분해.
         per-line 합계: evict=5, evicting_chiplets=2.
         라인 거주 칩렛이 18 이라면 coverage = 2/18 = 0.11, evicts_per_residency = 5/18 = 0.28.

  Q2. "특정 칩렛이 그 데이터 자체를 안 건드렸으면?"
      => 분모(거주 칩렛 = demand-access 한 chiplet) 에서 자동 제외.
         그 칩렛엔 라인이 install 된 적도 없으니 evict 대상이 아님.

데이터 소스 (util/tracing/memoryaccesstracer.go):
  time,addr,pc,owner,rw,rdma,vector,type,chiplet,data_id,is_ptw
    type 0~5 demand access
    type 6   evict (master 포맷: addr = evictor 의 새 라인. 본 스크립트는
             evict event 가 *발생한 라인* 으로 본다 -> proxy)
    type 7   overwrite
    type 8/9 victim (2026-06-23 신규, 진짜 evicted 라인의 PA)
  6/7 vs 8/9 어느 쪽을 evict 신호로 쓸지는 --evict-source 로 선택.
  최신 sweep 은 8+9 가 정확. 구 sweep 은 6+7 만 있음 (proxy).

용법:
  analyze/run.sh top-n/hot_shared_eviction_pressure.py mesh-6x6/20260624-002942
  analyze/run.sh top-n/hot_shared_eviction_pressure.py latest --hot-pct 5 --share-thr 18
  analyze/run.sh top-n/hot_shared_eviction_pressure.py latest --evict-source 8,9

출력:
  _out/top-n/<run-id>/hot_shared_eviction_pressure.xlsx
    sheet 'summary'   : benchmark x 지표
    sheet 'per_line'  : Hot∩Shared 라인 별 상세 (top --dump-k worst-thrashed)
"""

import argparse
import os
import sys
import numpy as np
import pandas as pd
import matplotlib.pyplot as plt

parent_dir = os.path.abspath(os.path.join(os.path.dirname(__file__), "../../"))
if parent_dir not in sys.path:
    sys.path.append(parent_dir)
scripts_dir = os.path.abspath(os.path.join(os.path.dirname(__file__), "../../../scripts"))
if scripts_dir not in sys.path:
    sys.path.append(scripts_dir)

from concurrent.futures import ProcessPoolExecutor
from itertools import repeat
from params import benchmarks
from _paths import run_root, memory_trace_dir, run_tag

CACHELINE_BYTES = 64
DEMAND_TYPES = {0, 1, 2, 3, 4, 5}
VICTIM_TYPES = {8, 9}      
FILTER_CHOICES = ('all', 'data', 'pte')


def _apply_filter(df, mode):
    if mode == 'all':
        return df
    if mode == 'data':
        return df[df['is_ptw'] == 0]
    if mode == 'pte':
        return df[df['is_ptw'] == 1]
    raise ValueError(mode)


BIN_LABELS = ['0', '1', '2-4', '5-8', '9-16', '17+']
BIN_EDGES  = [-1, 0, 1, 4, 8, 16, np.iinfo(np.int64).max]

AVG_LABELS = ['0', '(0,1]', '(1,2]', '(2,4]', '(4,8]', '8+']
AVG_EDGES  = [-0.001, 0.0, 1.0, 2.0, 4.0, 8.0, np.finfo(np.float64).max]


def _bucketize(counts, labels=BIN_LABELS, edges=BIN_EDGES):
    """count/avg 시리즈 -> 라벨별 비율(%) dict + 총 개수."""
    n = len(counts)
    if n == 0:
        return {f'pct_{lbl}': None for lbl in labels}, 0
    bins = pd.cut(counts, bins=edges, labels=labels)
    dist = bins.value_counts().reindex(labels, fill_value=0)
    return {f'pct_{lbl}': round(v / n * 100, 2) for lbl, v in dist.items()}, n


def _compute_one(df, mode, hot_pct, share_thr, evict_types, dump_k):
    sub = _apply_filter(df, mode)
    if sub.empty:
        return None, None

    demand = sub[sub['type'].isin(DEMAND_TYPES)]
    evict = sub[sub['type'].isin(evict_types)]
    if demand.empty:
        return None, None

    line_demand_total = demand.groupby('cacheline', sort=False).size()
    line_share = demand.groupby('cacheline', sort=False)['chiplet'].nunique()

    hot_thr_count = np.percentile(line_demand_total.values, 100 - hot_pct)
    is_hot = line_demand_total >= hot_thr_count
    is_shared = line_share >= share_thr
    hot_lines = line_demand_total.index[(is_hot & is_shared).values]
    if len(hot_lines) == 0:
        return None, None

    # (cacheline, chiplet) 거주지
    dem_pair = demand.groupby(['cacheline', 'chiplet'], sort=False).size()
    if not evict.empty:
        evc_pair = evict.groupby(['cacheline', 'chiplet'], sort=False).size()
    else:
        evc_pair = pd.Series(dtype='int64')

    # Hot 라인 residency 만 골라 evict 카운트 부착 (없으면 0)
    hot_set = set(hot_lines.tolist())
    dem_pair_hot = dem_pair[dem_pair.index.get_level_values(0).isin(hot_set)]
    evc_per_resid = evc_pair.reindex(dem_pair_hot.index, fill_value=0).astype('int64')

    # per-line 합산
    evc_per_line = evc_per_resid.groupby(level=0).sum().reindex(hot_lines, fill_value=0)
    touching     = dem_pair_hot.groupby(level=0).size().reindex(hot_lines, fill_value=1)
    avg_per_line = evc_per_line / touching   # avg evict per touching chiplet

    line_bins, n_lines  = _bucketize(evc_per_line)
    resid_bins, n_resid = _bucketize(evc_per_resid)
    avg_bins,   _       = _bucketize(avg_per_line, labels=AVG_LABELS, edges=AVG_EDGES)

    row = {'filter': mode, 'hot_lines': n_lines, 'hot_residencies': n_resid}
    row.update({f'line_{k}': v for k, v in line_bins.items()})
    row.update({f'resid_{k}': v for k, v in resid_bins.items()})
    row.update({f'avg_{k}': v for k, v in avg_bins.items()})

    # per-line dump (worst-thrashed)
    dump = None
    if dump_k > 0:
        per_line_df = pd.DataFrame({
            'evict_count':       evc_per_line,
            'avg_evict_per_chip': avg_per_line.round(3),
            'access_count':      line_demand_total.reindex(hot_lines),
            'touching_chiplets': touching,
        })
        dump = (per_line_df.sort_values('evict_count', ascending=False)
                          .head(dump_k).reset_index()
                          .rename(columns={'cacheline': 'cacheline_id'}))
        dump.insert(0, 'filter', mode)
    return row, dump


def process_benchmark(benchmark, args):
    trace_file = os.path.join(args.trace_dir,
                              f"{benchmark}_mesh_{args.xDim}x{args.yDim}_memory_trace.csv")
    if not os.path.exists(trace_file):
        return None, None

    print(f"[{benchmark}] hot-shared eviction pressure ...", flush=True)
    try:
        df = pd.read_csv(
            trace_file, skipinitialspace=True,
            dtype={'addr': 'uint64', 'chiplet': 'int16',
                   'type': 'int8', 'is_ptw': 'int8'},
        )
        df = df.drop_duplicates()
        df = df[['addr', 'chiplet', 'type', 'is_ptw']]
        if df.empty:
            return None, None
        df['cacheline'] = df['addr'].to_numpy() >> 6
        df.drop(columns=['addr'], inplace=True)

        rows, dumps = [], []
        for mode in args.filters:
            row, dump = _compute_one(
                df, mode,
                hot_pct=args.hot_pct,
                share_thr=args.share_thr,
                evict_types=args.evict_types,
                dump_k=args.dump_k,
            )
            if row is None:
                continue
            row = {'benchmark': benchmark, **row}
            rows.append(row)
            if dump is not None and len(dump):
                dump.insert(0, 'benchmark', benchmark)
                dumps.append(dump)
        if not rows:
            return None, None
        return pd.DataFrame(rows), (pd.concat(dumps, ignore_index=True) if dumps else None)
    except Exception as e:
        print(f"[{benchmark}] Error: {e}", flush=True)
        return None, None


if __name__ == "__main__":
    ap = argparse.ArgumentParser(
        description="Hot AND chiplet-shared cacheline 들의 cross-chiplet 추방 압력 측정."
    )
    ap.add_argument('run_id', nargs='?', default='latest',
                    help="run-id (latest, <config>, <config>/<timestamp>, ...)")
    ap.add_argument('--xDim', type=int, default=6)
    ap.add_argument('--yDim', type=int, default=6)
    ap.add_argument('--hot-pct', type=float, default=5.0,
                    help="Hot 임계: demand-access 상위 백분위 (default 5)")
    ap.add_argument('--share-thr', type=int, default=18,
                    help="Shared 임계: 그 라인을 demand-access 한 칩렛 수 (default 18 = 36/2)")
    ap.add_argument('--evict-source', default='8,9',
                    help="evict event type 들. '8,9' (victim, 기본) or '6,7' (evictor proxy, 옛 sweep)")
    ap.add_argument('--filter', dest='filters',
                    type=lambda s: [x.strip() for x in s.split(',')],
                    default=['data'],
                    help=f"comma-separated subset of {FILTER_CHOICES}. default: data")
    ap.add_argument('--dump-k', type=int, default=50,
                    help="per_line 시트에 덤프할 worst-thrashed 라인 수 (default 50, 0=off)")
    ap.add_argument('--plot-view', choices=['line', 'resid', 'avg_resid'], default='resid',
                    help="100%% 누적 막대 차트의 단위: "
                         "resid=(line,chiplet) 쌍 evict 수, "
                         "avg_resid=라인당 평균 evict 수(접근 칩렛 수로 나눔), "
                         "line=라인 전체 합산 (default resid)")
    args = ap.parse_args()

    for f in args.filters:
        if f not in FILTER_CHOICES:
            ap.error(f"--filter must be subset of {FILTER_CHOICES}; got {f}")
    if not (0 < args.hot_pct <= 100):
        ap.error(f"--hot-pct in (0,100]; got {args.hot_pct}")
    try:
        args.evict_types = {int(x) for x in args.evict_source.split(',')}
    except ValueError:
        ap.error(f"--evict-source must be comma-separated ints; got {args.evict_source}")

    run = run_root(args.run_id)
    args.trace_dir = str(memory_trace_dir(args.run_id))
    print(f"run = {run}", flush=True)
    print(f"trace dir = {args.trace_dir}", flush=True)
    print(f"hot_pct={args.hot_pct}  share_thr={args.share_thr}  evict_types={sorted(args.evict_types)}", flush=True)

    with ProcessPoolExecutor() as ex:
        out = list(ex.map(process_benchmark, benchmarks, repeat(args)))
    summaries = [s for s, _ in out if s is not None]
    dumps = [d for _, d in out if d is not None]
    if not summaries:
        print("No results.")
        sys.exit(0)

    summary = pd.concat(summaries, ignore_index=True)
    per_line = pd.concat(dumps, ignore_index=True) if dumps else pd.DataFrame()

    username = os.path.expanduser('~')
    out_dir = f"{username}/mgvm/analyze/_out/top-n/{run_tag(args.run_id)}"
    os.makedirs(out_dir, exist_ok=True)
    out_path = os.path.join(out_dir, "hot_shared_eviction_pressure.xlsx")
    with pd.ExcelWriter(out_path) as w:
        summary.to_excel(w, sheet_name='summary', index=False)
        if not per_line.empty:
            per_line.to_excel(w, sheet_name='per_line', index=False)

    print("\n" + "=" * 100)
    print(f"{'Hot AND chiplet-shared eviction pressure':^100}")
    print("=" * 100)
    print(summary.to_string(index=False))
    print(f"\nSaved: {out_path}")

    # ── 100% stacked bar ──────────────────────────────────────────────────
    from plot_style import apply_paper_style, SEVERITY_6, SIZE_WIDE
    apply_paper_style()

    if args.plot_view == 'avg_resid':
        active_labels = AVG_LABELS
        prefix        = 'avg_pct_'
        legend_title  = 'Avg evictions\nper chiplet'
        ylabel        = 'Fraction of Hot Cachelines (%)'
    elif args.plot_view == 'resid':
        active_labels = BIN_LABELS
        prefix        = 'resid_pct_'
        legend_title  = 'Eviction count\nper residency'
        ylabel        = 'Fraction of Hot Residencies (%)'
    else:
        active_labels = BIN_LABELS
        prefix        = 'line_pct_'
        legend_title  = 'Total eviction\ncount per line'
        ylabel        = 'Fraction of Hot Cachelines (%)'

    bin_cols = [f"{prefix}{lbl}" for lbl in active_labels]
    miss = [c for c in bin_cols if c not in summary.columns]
    if miss:
        print(f"plot skipped (missing cols: {miss})")
        sys.exit(0)

    plot_df = summary if 'filter' not in summary.columns else \
              summary[summary['filter'] == args.filters[0]]
    plot_df = plot_df.set_index('benchmark')[bin_cols].fillna(0)

    n_bench = len(plot_df)
    bar_w   = 0.65
    fig_w   = max(SIZE_WIDE[0], 0.38 * n_bench + 1.8)
    fig, ax = plt.subplots(figsize=(fig_w, SIZE_WIDE[1]))

    x      = np.arange(n_bench)
    bottom = np.zeros(n_bench)
    bars_handles = []
    for col, c, lbl in zip(bin_cols, SEVERITY_6, active_labels):
        vals = plot_df[col].to_numpy()
        b = ax.bar(x, vals, width=bar_w, bottom=bottom,
                   color=c, edgecolor='white', linewidth=0.4, label=lbl)
        bars_handles.append(b)
        bottom += vals

    ax.set_xticks(x)
    ax.set_xticklabels(plot_df.index, rotation=45, ha='right', fontsize=7)
    ax.set_xlim(-0.5, n_bench - 0.5)
    ax.set_ylim(0, 103)
    ax.set_yticks([0, 25, 50, 75, 100])
    ax.yaxis.grid(True, linestyle='--', linewidth=0.4, color='#cccccc', alpha=1)
    ax.set_axisbelow(True)
    ax.set_ylabel(ylabel)
    ax.set_xlabel('Benchmark')

    leg = ax.legend(
        loc='upper left', bbox_to_anchor=(1.01, 1.0),
        title=legend_title, title_fontsize=7,
        fontsize=7, frameon=False,
        borderpad=0, labelspacing=0.3,
    )

    fig_name = f"hot_evict_buckets_{args.plot_view}"
    p = os.path.join(out_dir, f"{fig_name}.png")
    fig.savefig(p, dpi=400, bbox_inches='tight', pad_inches=0.03)
    print(f"figure: {p}")
    plt.close(fig)
