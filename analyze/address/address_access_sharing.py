"""각 address(캐시라인) 별 접근 횟수와 칩렛 간 공유 정도를 산점도로 시각화.

memory-trace(`<bench>_mesh_<x>x<y>_memory_trace.csv`)를 읽어 주소별로
  - access_count  : 해당 주소에 대한 총 접근 횟수
  - sharing_degree: 그 주소에 접근한 *서로 다른 칩렛* 의 수 (칩렛 간 공유 정도)
를 집계한 뒤, 벤치마크마다 한 장의 산점도를 그린다.

  x축 = address(10진수, line-aligned byte address)
  y축 = access_count (기본 log scale)
  색  = sharing_degree (공유 정도가 클수록 진해짐, YlGnBu)

pc_stats_* 스크립트들과 동일하게, 맨 위의 `date` 한 줄만 바꿔주면
해당 run 의 모든 벤치마크에 대해 그림을 뽑는다.

    python3 address_access_sharing.py                    # 캐시라인(64B) 단위, PTW 제외
    python3 address_access_sharing.py --group-size 0     # raw byte address 단위
    python3 address_access_sharing.py --include-ptw       # 페이지테이블 접근 포함
    python3 address_access_sharing.py --rdma-only         # 원격(RDMA) 접근만
    python3 address_access_sharing.py --rdma-l2miss-only  # RDMA 로 서비스된 L2 miss 만
    python3 address_access_sharing.py --benchmark spmv    # 한 벤치마크만

출력은 `./<date>/address_access_sharing[_<filter>]/` 아래에 저장된다.
date 마다, 그리고 필터 조합마다 폴더가 분리되어 서로 덮어쓰지 않는다.

메모리-trace 가 수 GB ~ 수십 GB 이므로 memory_footprint.py 와 동일하게
chunk 단위로 읽으며 누적 집계한다.
"""

import os
import sys
import argparse
import traceback

import matplotlib
matplotlib.use("Agg")  # ProcessPool worker 안에서 그리므로 headless 백엔드 강제
import matplotlib.pyplot as plt
import matplotlib.ticker as mticker
from matplotlib.colors import Normalize, LinearSegmentedColormap
import numpy as np
import pandas as pd

from concurrent.futures import ProcessPoolExecutor
from itertools import repeat

parent_dir = os.path.abspath(os.path.join(os.path.dirname(__file__), "../../"))
if parent_dir not in sys.path:
    sys.path.append(parent_dir)

scripts_dir = os.path.abspath(os.path.join(os.path.dirname(__file__), "../../../scripts"))
if scripts_dir not in sys.path:
    sys.path.append(scripts_dir)

from params import benchmarks

username = os.path.expanduser('~')

# run_id 결정: argv 첫 positional, 없으면 'latest'
_run_id = next((a for a in sys.argv[1:] if not a.startswith('-')), 'latest')
from _paths import run_root as _run_root, memory_trace_dir as _mtd, run_tag as _run_tag
date = _run_tag(_run_id)
trace_dir = str(_mtd(_run_id))

xDim = 6
yDim = 6
num_chiplets = xDim * yDim

CHUNK_SIZE = 4_000_000   # memory_footprint.py 와 동일한 chunk 크기
COMPACT_EVERY = 24       # 누적 partial 들을 합쳐 메모리를 줄이는 주기(chunk 수)

L2_MISS_TYPES = {2, 5}   # pc_data_id_access.py 와 동일: RDMA 로 서비스된 L2 miss 타입


# ---------------------------------------------------------------------------
# 논문용 plot 스타일
# ---------------------------------------------------------------------------
def _set_paper_style():
    plt.rcParams.update({
        'savefig.dpi': 300,
        'figure.dpi': 120,
        'font.family': 'DejaVu Sans',
        'font.size': 11,
        'axes.titlesize': 14,
        'axes.titleweight': 'bold',
        'axes.labelsize': 12,
        'xtick.labelsize': 9,
        'ytick.labelsize': 10,
        'axes.linewidth': 0.8,
        'axes.grid': True,
        'grid.alpha': 0.30,
        'grid.linewidth': 0.5,
        'legend.frameon': False,
    })


_set_paper_style()


# ---------------------------------------------------------------------------
# 집계
# ---------------------------------------------------------------------------
def _compute_stats(trace_file, group_size, include_ptw, rdma_only, rdma_l2miss_only):
    """trace 한 파일에서 (addr, access_count, sharing_degree, byte_addr) 테이블 생성.

    sharing_degree 는 (addr, chiplet) distinct pair 를 전 chunk 에 걸쳐 모은 뒤
    addr 별 distinct chiplet 수로 계산한다 (chiplet 비트마스크 OR 와 동치이지만
    drop_duplicates + nunique 만으로 vectorized 하게 처리된다).

    필터(모두 chunk 단위로 적용):
      include_ptw=False    -> is_ptw == 1 (페이지테이블 접근) 제거
      rdma_only            -> rdma == 1 (원격 접근)만
      rdma_l2miss_only     -> rdma == 1 AND type in L2_MISS_TYPES 만
    """
    use_cols = ['addr', 'chiplet']
    dtypes = {'addr': 'uint64', 'chiplet': 'int16'}
    if not include_ptw:
        use_cols.append('is_ptw')
        dtypes['is_ptw'] = 'int8'
    if rdma_only or rdma_l2miss_only:
        use_cols.append('rdma')
        dtypes['rdma'] = 'int8'
    if rdma_l2miss_only:
        use_cols.append('type')
        dtypes['type'] = 'int8'

    count_parts = []   # list[Series]   addr -> 부분 access_count
    pair_parts = []    # list[DataFrame] distinct (addr, chiplet)

    reader = pd.read_csv(
        trace_file,
        usecols=use_cols,
        dtype=dtypes,
        skipinitialspace=True,
        chunksize=CHUNK_SIZE,
        engine='c',
    )

    n = 0
    for chunk in reader:
        if not include_ptw:
            chunk = chunk[chunk['is_ptw'] == 0]
        if rdma_l2miss_only:
            chunk = chunk[(chunk['rdma'] == 1) & (chunk['type'].isin(L2_MISS_TYPES))]
        elif rdma_only:
            chunk = chunk[chunk['rdma'] == 1]
        if chunk.empty:
            continue
        if group_size:
            chunk['addr'] = chunk['addr'] // (64 * group_size)

        count_parts.append(chunk.groupby('addr').size())
        pair_parts.append(chunk[['addr', 'chiplet']].drop_duplicates())

        n += 1
        if n % COMPACT_EVERY == 0:
            count_parts = [pd.concat(count_parts).groupby(level=0).sum()]
            pair_parts = [pd.concat(pair_parts, ignore_index=True).drop_duplicates()]

    if not count_parts:
        return None

    counts = pd.concat(count_parts).groupby(level=0).sum()
    pairs = pd.concat(pair_parts, ignore_index=True).drop_duplicates()
    sharing = pairs.groupby('addr')['chiplet'].nunique()

    stats = pd.DataFrame({'access_count': counts, 'sharing_degree': sharing})
    stats.index.name = 'addr'
    stats = stats.reset_index()
    stats['access_count'] = stats['access_count'].astype('int64')
    stats['sharing_degree'] = stats['sharing_degree'].astype('int32')

    # 10진수 표시용: line index -> line 의 base byte address
    mult = (64 * group_size) if group_size else 1
    stats['byte_addr'] = (stats['addr'] * mult).astype('float64')
    return stats


def _subsample(stats, max_points, seed):
    """점이 너무 많으면 산점도가 무거워지므로 표본 추출.

    공유(sharing_degree > 1)되는 '흥미로운' 점은 모두 보존하고,
    단독 접근(degree == 1) 점만 표본으로 줄인다. (no silent cap: 버린 수 반환)
    """
    if max_points <= 0 or len(stats) <= max_points:
        return stats, 0

    shared = stats[stats['sharing_degree'] > 1]
    singles = stats[stats['sharing_degree'] <= 1]

    budget = max_points - len(shared)
    if budget <= 0:
        # 공유 점만으로도 한도를 초과 -> 공유 정도가 높은 순으로 유지
        kept = shared.sort_values('sharing_degree', ascending=False).head(max_points)
    else:
        if len(singles) > budget:
            singles = singles.sample(n=budget, random_state=seed)
        kept = pd.concat([shared, singles])

    return kept, len(stats) - len(kept)


# ---------------------------------------------------------------------------
# plot
# ---------------------------------------------------------------------------
def _plot(stats, benchmark, out_dir, group_size, y_log, n_lines, n_shared,
          max_sharing, dropped, filter_label=""):
    # 공유 정도가 높은(=진한) 점을 마지막에 그려 위로 올라오게 한다.
    stats = stats.sort_values('sharing_degree', kind='mergesort')

    x = stats['byte_addr'].to_numpy()
    y = stats['access_count'].to_numpy()
    c = stats['sharing_degree'].to_numpy()

    fig, ax = plt.subplots(figsize=(8.0, 5.0))

    # YlGnBu: 공유 정도가 클수록 진한 남색. 단, 최저단(거의 흰색)은 흰 배경에
    # 묻혀 단독-접근(private) 점이 안 보이므로 0.15~1.0 구간만 잘라 쓴다.
    _base = plt.get_cmap('YlGnBu')
    cmap = LinearSegmentedColormap.from_list(
        'YlGnBu_trunc', _base(np.linspace(0.15, 1.0, 256)))
    norm = Normalize(vmin=1, vmax=num_chiplets)

    sc = ax.scatter(
        x, y,
        c=c, cmap=cmap, norm=norm,
        s=7, linewidths=0, alpha=0.85,
        rasterized=True,  # PNG/PDF 용량 절감 + 렌더 가속
    )

    if y_log:
        ax.set_yscale('log')

    line_unit = 'cache line, 64B' if group_size == 1 else (
        f'{group_size} cache lines' if group_size else 'byte')
    ax.set_xlabel(f'Address (decimal, {line_unit} granularity)')
    ax.set_ylabel('Access count' + (' (log scale)' if y_log else ''))
    ax.set_title(f'{benchmark}  ·  {filter_label}' if filter_label else benchmark)

    # x축: 10진수, 천단위 구분, 눈금은 6개 정도만 (모든 주소를 찍지 않음)
    ax.xaxis.set_major_locator(mticker.MaxNLocator(nbins=6))
    ax.xaxis.set_major_formatter(mticker.FuncFormatter(lambda v, _p: f'{int(v):,}'))
    plt.setp(ax.get_xticklabels(), rotation=30, ha='right')
    ax.margins(x=0.02)

    # colorbar (정수 눈금)
    cb = fig.colorbar(sc, ax=ax, pad=0.02)
    cb.set_label(f'Inter-chiplet sharing\n(# distinct chiplets / {num_chiplets})')
    cb.locator = mticker.MaxNLocator(integer=True)
    cb.update_ticks()

    # 요약 주석 (버린 점이 있으면 명시)
    note = (f'lines: {n_lines:,}   shared(>1): {n_shared:,}   '
            f'max sharing: {max_sharing}')
    if dropped:
        note += f'\n(plotted {n_lines - dropped:,}; {dropped:,} singletons sub-sampled)'
    ax.text(0.01, 0.99, note, transform=ax.transAxes, ha='left', va='top',
            fontsize=8, color='0.25',
            bbox=dict(boxstyle='round,pad=0.3', fc='white', ec='0.8', alpha=0.7))

    for spine in ('top', 'right'):
        ax.spines[spine].set_visible(False)

    fig.tight_layout()

    out_png = os.path.join(out_dir, f'{benchmark}_address_access_sharing.png')
    fig.savefig(out_png, bbox_inches='tight')
    plt.close(fig)
    return out_png


# ---------------------------------------------------------------------------
# 벤치마크 1개 처리 (worker)
# ---------------------------------------------------------------------------
def process_benchmark(benchmark, args):
    trace_file = f"{trace_dir}/{benchmark}_mesh_{xDim}x{yDim}_memory_trace.csv"
    if not os.path.exists(trace_file):
        print(f"[{benchmark}] no trace ({trace_file}), skip")
        return None

    print(f"[{benchmark}] computing per-address access & sharing ...", flush=True)
    try:
        stats = _compute_stats(trace_file, args.group_size, args.include_ptw,
                               args.rdma_only, args.rdma_l2miss_only)
        if stats is None or stats.empty:
            print(f"[{benchmark}] empty after filtering, skip")
            return None

        n_lines = len(stats)
        n_shared = int((stats['sharing_degree'] > 1).sum())
        max_sharing = int(stats['sharing_degree'].max())

        seed = sum(map(ord, benchmark))  # 결정론적 표본
        plot_stats, dropped = _subsample(stats, args.max_points, seed)

        out_png = _plot(
            plot_stats, benchmark, args.out_dir, args.group_size,
            y_log=not args.linear_y, n_lines=n_lines, n_shared=n_shared,
            max_sharing=max_sharing, dropped=dropped, filter_label=args.filter_label,
        )
        print(f"[{benchmark}] saved {out_png}", flush=True)

        return {
            'benchmark': benchmark,
            'unique_lines': n_lines,
            'shared_lines': n_shared,
            'shared_ratio_%': round(n_shared / n_lines * 100, 2) if n_lines else 0.0,
            'max_sharing': max_sharing,
            'plotted_points': n_lines - dropped,
            'output': out_png,
        }
    except Exception as e:
        print(f"[{benchmark}] Error: {e}")
        traceback.print_exc()
        return None


if __name__ == "__main__":
    parser = argparse.ArgumentParser(
        description="Per-address access count & inter-chiplet sharing scatter plots.")
    parser.add_argument('run_id', nargs='?', default='latest',
                        help="run-id (latest, <config>, <config>/<timestamp>, ...)")
    parser.add_argument('--group-size', type=int, default=1,
                        help='cache-line group size: addr // (64 * group_size). '
                             '1 = 64B cache line (default), 0 = raw byte address')
    parser.add_argument('--include-ptw', action='store_true',
                        help='include page-table-walk accesses (default: drop is_ptw==1)')
    parser.add_argument('--rdma-only', action='store_true',
                        help='count only RDMA (remote) accesses (rdma == 1)')
    parser.add_argument('--rdma-l2miss-only', action='store_true',
                        help='count only RDMA-served L2 misses (rdma == 1 AND type in {2,5})')
    parser.add_argument('--linear-y', action='store_true',
                        help='use a linear y-axis instead of log scale')
    parser.add_argument('--max-points', type=int, default=200_000,
                        help='max scatter points per benchmark; shared lines are kept, '
                             'singletons sub-sampled (0 = no cap)')
    parser.add_argument('--benchmark', default=None,
                        help='only this benchmark (default: all in params.benchmarks)')
    parser.add_argument('--workers', type=int,
                        default=int(os.environ.get('AAS_WORKERS', '10')),
                        help='parallel workers (each reads a multi-GB CSV; default 4)')
    args = parser.parse_args()

    args.workers = len(benchmarks)

    if args.rdma_only and args.rdma_l2miss_only:
        parser.error("--rdma-only and --rdma-l2miss-only are mutually exclusive")

    bench_list = [args.benchmark] if args.benchmark else list(benchmarks)

    # 필터 조합마다 폴더/제목이 달라지도록 suffix 와 label 을 구성한다.
    suffix = ""
    if args.group_size != 1:
        suffix += f"_g{args.group_size}"
    if args.rdma_l2miss_only:
        suffix += "_rdma-l2miss"
        args.filter_label = "RDMA L2-miss"
    elif args.rdma_only:
        suffix += "_rdma"
        args.filter_label = "RDMA only"
    else:
        args.filter_label = ""
    if args.include_ptw:
        suffix += "_with-ptw"
    if args.linear_y:
        suffix += "_liny"

    # date 별로 서로 다른 폴더 아래에, 필터 조합별 하위 폴더로 저장.
    args.out_dir = f"{username}/mgvm/analyze/_out/address/{date}/address_access_sharing{suffix}"
    os.makedirs(args.out_dir, exist_ok=True)

    print(f"Trace dir : {trace_dir}")
    print(f"Output dir: {args.out_dir}")
    print(f"Filter    : {args.filter_label or 'all accesses'}")
    print(f"Benchmarks: {len(bench_list)}  |  workers: {args.workers}")

    results = []
    with ProcessPoolExecutor(max_workers=max(1, args.workers)) as executor:
        for res in executor.map(process_benchmark, bench_list, repeat(args)):
            if res is not None:
                results.append(res)

    print("\n" + "=" * 80)
    print(f"{'Per-Address Access & Inter-chiplet Sharing':^80}")
    print("=" * 80)
    if results:
        summary = pd.DataFrame(results).sort_values('shared_ratio_%', ascending=False)
        print(summary.to_string(index=False))
        summary.to_csv(os.path.join(args.out_dir, "_summary.csv"), index=False)
        print(f"\nFigures + _summary.csv saved under: {args.out_dir}")
    else:
        print("No results generated (no traces found?).")
