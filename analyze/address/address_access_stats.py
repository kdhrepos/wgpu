"""각 address(캐시라인)별 접근 횟수와 칩렛 간 공유 정도를 표(table)로 집계.

`pc_stats_*` 계열이 PC 단위로 통계를 뽑아 xlsx 로 내보내는 것과 동일한 골격이지만,
집계 단위가 PC 가 아니라 **address(캐시라인)** 이다. memory-trace 한 파일에서 주소별로

  - access_count   : 그 주소에 대한 총 접근 횟수
  - sharing_degree : 그 주소에 접근한 *서로 다른 칩렛* 의 수 (칩렛 간 공유 정도)
  - chiplet_mask   : 접근한 칩렛 집합을 비트마스크로 (bit i = 칩렛 i, num_chiplets<=64)
  - read/write_count, rdma_count, owner(홈 노드)

를 누적 집계한다. address_access_sharing.py 가 같은 값을 *산점도(PNG)* 로 그리는 데
비해, 이 스크립트는 다운스트림 분석이 가능한 *테이블(CSV/xlsx)* 을 내보낸다.

pc_stats_*/address_access_sharing.py 와 동일하게, 맨 위의 `date` 한 줄만 바꿔주면
해당 run 의 모든 벤치마크를 처리한다. 메모리-trace 가 수 GB 이므로 memory_footprint.py
와 같이 chunk 단위로 읽으며 누적한다.

    python3 address_access_stats.py                     # 캐시라인(64B) 단위, 전체 접근
    python3 address_access_stats.py --group-size 0      # raw byte address 단위
    python3 address_access_stats.py --access-type data  # PTW(페이지테이블) 접근 제외
    python3 address_access_stats.py --access-type pte   # PTW 접근만
    python3 address_access_stats.py --rdma-only         # 원격(RDMA) 접근만
    python3 address_access_stats.py --rdma-l2miss-only  # RDMA 로 서비스된 L2 miss 만
    python3 address_access_stats.py --top 100000        # 주소별 표를 접근수 상위 N 개로 제한
    python3 address_access_stats.py --benchmark spmv    # 한 벤치마크만

출력은 `./<date>/address_access_stats[_<filter>]/` 아래에 저장된다 (필터 조합별 분리):
  - <bench>_address_stats.csv : 주소별 한 행 (access_count 내림차순; --top 으로 캡 가능)
  - <bench>_sharing_dist.csv  : sharing_degree 별 주소 수 / 접근 수 분포
  - _summary.csv              : 벤치마크별 요약 (unique/shared 주소, 공유율 등)
  - _sharing_dist.xlsx        : 모든 벤치마크의 sharing_degree 분포 (long form, 한 파일)
"""

import os
import sys
import argparse
import traceback

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

L2_MISS_TYPES = {2, 5}   # address_access_sharing.py 와 동일: RDMA 로 서비스된 L2 miss 타입


# ---------------------------------------------------------------------------
# is_ptw 컬럼은 trace 버전에 따라 0/1 정수 또는 'true'/'false' 문자열일 수 있다.
# True = page-table-walk(PTE) 접근.
# ---------------------------------------------------------------------------
def _ptw_mask(series):
    if series.dtype.kind in 'iuf':
        return series != 0
    return series.astype(str).str.strip().str.lower().isin(['1', 'true', 't', 'yes'])


# ---------------------------------------------------------------------------
# 집계: trace 한 파일 -> 주소별 통계 DataFrame
# ---------------------------------------------------------------------------
def _compute_addr_stats(trace_file, group_size, access_type, rdma_only,
                        rdma_l2miss_only, has_is_ptw):
    """주소별 (access_count, sharing_degree, chiplet_mask, rw/rdma 카운트, owner) 집계.

    sharing_degree 는 전 chunk 에 걸쳐 distinct (addr, chiplet) 쌍을 모은 뒤 addr 별
    distinct chiplet 수로 구한다 (chiplet 비트마스크 OR 와 동치). 카운트류는 chunk 마다
    groupby-sum 으로 누적하고, owner(홈 노드)는 addr 별 first 로 유지한다.
    """
    use_cols = ['addr', 'chiplet', 'rw', 'rdma', 'owner']
    dtypes = {'addr': 'uint64', 'chiplet': 'int16', 'rw': 'int8',
              'rdma': 'int8', 'owner': 'int16'}
    if rdma_l2miss_only:
        use_cols.append('type')
        dtypes['type'] = 'int8'
    if access_type != 'all' and has_is_ptw:
        use_cols.append('is_ptw')  # dtype 는 추론(문자열 trace 호환)

    count_parts = []   # list[DataFrame] addr -> 부분 카운트 (access/read/write/rdma)
    pair_parts = []    # list[DataFrame] distinct (addr, chiplet)
    owner_parts = []   # list[DataFrame] addr -> owner(first)

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
        if access_type != 'all' and has_is_ptw:
            ptw = _ptw_mask(chunk['is_ptw'])
            chunk = chunk[~ptw] if access_type == 'data' else chunk[ptw]
        if rdma_l2miss_only:
            chunk = chunk[(chunk['rdma'] == 1) & (chunk['type'].isin(L2_MISS_TYPES))]
        elif rdma_only:
            chunk = chunk[chunk['rdma'] == 1]
        if chunk.empty:
            continue
        if group_size:
            chunk['addr'] = chunk['addr'] // (64 * group_size)

        chunk['is_read'] = (chunk['rw'] == 0)
        chunk['is_write'] = (chunk['rw'] == 1)
        chunk['is_rdma'] = (chunk['rdma'] == 1)

        count_parts.append(chunk.groupby('addr').agg(
            access_count=('addr', 'size'),
            read_count=('is_read', 'sum'),
            write_count=('is_write', 'sum'),
            rdma_count=('is_rdma', 'sum'),
        ))
        pair_parts.append(chunk[['addr', 'chiplet']].drop_duplicates())
        owner_parts.append(chunk[['addr', 'owner']].drop_duplicates('addr'))

        n += 1
        if n % COMPACT_EVERY == 0:
            count_parts = [pd.concat(count_parts).groupby(level=0).sum()]
            pair_parts = [pd.concat(pair_parts, ignore_index=True).drop_duplicates()]
            owner_parts = [pd.concat(owner_parts, ignore_index=True)
                           .drop_duplicates('addr')]

    if not count_parts:
        return None

    counts = pd.concat(count_parts).groupby(level=0).sum()

    pairs = pd.concat(pair_parts, ignore_index=True).drop_duplicates()
    # distinct 쌍이므로 비트 합 == OR. num_chiplets<=64 가정.
    pairs['bit'] = (np.uint64(1) << pairs['chiplet'].astype('uint64'))
    share = pairs.groupby('addr').agg(
        sharing_degree=('chiplet', 'nunique'),
        chiplet_mask=('bit', 'sum'),
    )

    owner = (pd.concat(owner_parts, ignore_index=True)
             .drop_duplicates('addr').set_index('addr')['owner'])

    stats = counts.join(share, how='left').join(owner, how='left').reset_index()
    stats['sharing_degree'] = stats['sharing_degree'].astype('int32')
    stats['chiplet_mask'] = stats['chiplet_mask'].astype('uint64')
    for c in ('access_count', 'read_count', 'write_count', 'rdma_count'):
        stats[c] = stats[c].astype('int64')

    # 10진수 표시용: line index -> line 의 base byte address
    mult = (64 * group_size) if group_size else 1
    stats['byte_addr'] = (stats['addr'] * mult).astype('uint64')
    stats['read_ratio_%'] = (stats['read_count'] /
                             stats['access_count'] * 100).round(2)
    return stats


def _sharing_distribution(stats):
    """sharing_degree 별 주소 수 / 접근 수 분포 (long form)."""
    dist = stats.groupby('sharing_degree').agg(
        n_addrs=('addr', 'size'),
        total_access=('access_count', 'sum'),
    ).reset_index()
    dist['n_addrs_%'] = (dist['n_addrs'] / dist['n_addrs'].sum() * 100).round(3)
    dist['total_access_%'] = (dist['total_access'] /
                              dist['total_access'].sum() * 100).round(3)
    return dist


# ---------------------------------------------------------------------------
# 벤치마크 1개 처리 (worker)
# ---------------------------------------------------------------------------
def process_benchmark(benchmark, args):
    trace_file = f"{trace_dir}/{benchmark}_mesh_{xDim}x{yDim}_memory_trace.csv"
    if not os.path.exists(trace_file):
        print(f"[{benchmark}] no trace ({trace_file}), skip")
        return None

    header = pd.read_csv(trace_file, nrows=0, skipinitialspace=True).columns.tolist()
    has_is_ptw = 'is_ptw' in header
    if args.access_type == 'pte' and not has_is_ptw:
        print(f"[{benchmark}] SKIP: trace has no 'is_ptw' column; "
              f"--access-type=pte cannot be computed.")
        return None
    if args.access_type == 'data' and not has_is_ptw:
        print(f"[{benchmark}] WARNING: no 'is_ptw' column; "
              f"--access-type=data falls back to all accesses.")

    print(f"[{benchmark}] computing per-address access & sharing ...", flush=True)
    try:
        stats = _compute_addr_stats(
            trace_file, args.group_size, args.access_type,
            args.rdma_only, args.rdma_l2miss_only, has_is_ptw)
        if stats is None or stats.empty:
            print(f"[{benchmark}] empty after filtering, skip")
            return None

        n_lines = len(stats)
        n_shared = int((stats['sharing_degree'] > 1).sum())
        total_access = int(stats['access_count'].sum())

        # sharing 분포 (전체 주소 기준; --top 캡 전에 계산)
        dist = _sharing_distribution(stats)
        dist.insert(0, 'benchmark', benchmark)
        dist.to_csv(os.path.join(args.out_dir,
                    f'{benchmark}_sharing_dist.csv'), index=False)

        # 주소별 표 (접근수 내림차순; --top 으로 행 수 제한 가능)
        col_order = ['addr', 'byte_addr', 'owner', 'access_count',
                     'sharing_degree', 'chiplet_mask', 'read_count',
                     'write_count', 'read_ratio_%', 'rdma_count']
        out = stats.sort_values('access_count', ascending=False)[col_order]
        if args.top > 0:
            out = out.head(args.top)
        out.insert(0, 'benchmark', benchmark)
        out.to_csv(os.path.join(args.out_dir,
                   f'{benchmark}_address_stats.csv'), index=False)

        print(f"[{benchmark}] {n_lines:,} lines, "
              f"shared(>1)={n_shared:,}, max_sharing={int(stats['sharing_degree'].max())}",
              flush=True)

        return {
            'benchmark': benchmark,
            'unique_lines': n_lines,
            'shared_lines': n_shared,
            'shared_ratio_%': round(n_shared / n_lines * 100, 2) if n_lines else 0.0,
            'private_lines': n_lines - n_shared,
            'max_sharing': int(stats['sharing_degree'].max()),
            'avg_sharing': round(float(stats['sharing_degree'].mean()), 3),
            'total_access': total_access,
            'avg_access_per_line': round(total_access / n_lines, 3) if n_lines else 0.0,
            '_dist': dist,
        }
    except Exception as e:
        print(f"[{benchmark}] Error: {e}")
        traceback.print_exc()
        return None


if __name__ == "__main__":
    parser = argparse.ArgumentParser(
        description="Per-address access count & inter-chiplet sharing degree table.")
    parser.add_argument('run_id', nargs='?', default='latest',
                        help="run-id (latest, <config>, <config>/<timestamp>, ...)")
    parser.add_argument('--group-size', type=int, default=1,
                        help='cache-line group size: addr // (64 * group_size). '
                             '1 = 64B cache line (default), 0 = raw byte address')
    parser.add_argument('--access-type', choices=['all', 'data', 'pte'], default='all',
                        help="memory access subset. 'all' (default), 'data' drops PTW "
                             "(is_ptw==1), 'pte' keeps only PTW accesses.")
    parser.add_argument('--rdma-only', action='store_true',
                        help='count only RDMA (remote) accesses (rdma == 1)')
    parser.add_argument('--rdma-l2miss-only', action='store_true',
                        help='count only RDMA-served L2 misses (rdma == 1 AND type in {2,5})')
    parser.add_argument('--top', type=int, default=0,
                        help='cap the per-address CSV to top-N by access_count '
                             '(0 = all rows; summary/dist still use the full set)')
    parser.add_argument('--benchmark', default=None,
                        help='only this benchmark (default: all in params.benchmarks)')
    parser.add_argument('--workers', type=int,
                        default=int(os.environ.get('AAS_WORKERS', str(len(benchmarks)))),
                        help='parallel workers (each reads a multi-GB CSV)')
    args = parser.parse_args()

    if args.rdma_only and args.rdma_l2miss_only:
        parser.error("--rdma-only and --rdma-l2miss-only are mutually exclusive")

    bench_list = [args.benchmark] if args.benchmark else list(benchmarks)

    # 필터 조합마다 폴더가 달라지도록 suffix 구성.
    suffix = ""
    if args.group_size != 1:
        suffix += f"_g{args.group_size}"
    if args.access_type != 'all':
        suffix += f"_{args.access_type}"
    if args.rdma_l2miss_only:
        suffix += "_rdma-l2miss"
    elif args.rdma_only:
        suffix += "_rdma"

    args.out_dir = f"{username}/mgvm/analyze/_out/address/{date}/address_access_stats{suffix}"
    os.makedirs(args.out_dir, exist_ok=True)

    print(f"Trace dir : {trace_dir}")
    print(f"Output dir: {args.out_dir}")
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
        dist_all = pd.concat([r.pop('_dist') for r in results], ignore_index=True)
        dist_all.to_excel(os.path.join(args.out_dir, "_sharing_dist.xlsx"), index=False)

        summary = pd.DataFrame(results).sort_values('shared_ratio_%', ascending=False)
        summary.to_csv(os.path.join(args.out_dir, "_summary.csv"), index=False)
        print(summary.to_string(index=False))
        print(f"\nPer-address CSVs + _summary.csv + _sharing_dist.xlsx under: {args.out_dir}")
    else:
        print("No results generated (no traces found?).")
