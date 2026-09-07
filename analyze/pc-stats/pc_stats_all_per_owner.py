import os
import sys
import pandas as pd

parent_dir = os.path.abspath(os.path.join(os.path.dirname(__file__), "../../"))
if parent_dir not in sys.path:
    sys.path.append(parent_dir)

scripts_dir = os.path.abspath(os.path.join(os.path.dirname(__file__), "../../../scripts"))
if scripts_dir not in sys.path:
    sys.path.append(scripts_dir)

from concurrent.futures import ProcessPoolExecutor
from itertools import repeat
from params import benchmarks

username = os.path.expanduser('~')

# --- run_id 결정: argv 첫 positional, 없으면 'latest' (Linux fork 전에 trace_dir 픽스) ---
# 값을 받는 플래그의 값(예: '--workers 8' 의 '8')을 run_id 로 오인하면 안 되므로 건너뛴다.
_VALUE_FLAGS = {'--group-size', '--group-by', '--access-type', '--workers'}


def _sniff_run_id(argv) -> str:
    skip = False
    for a in argv:
        if skip:
            skip = False
            continue
        if a.startswith('-'):
            skip = a in _VALUE_FLAGS          # '--flag value' 형태면 다음 토큰은 값
            continue
        return a
    return 'latest'


_run_id = _sniff_run_id(sys.argv[1:])
from _paths import run_root as _run_root, memory_trace_dir as _mtd, run_tag as _run_tag
date = _run_tag(_run_id)
trace_dir = str(_mtd(_run_id))

xDim = 6
yDim = 6

def process_benchmark(benchmark, args):
    trace_file = f"{trace_dir}/{benchmark}_mesh_{xDim}x{yDim}_memory_trace.csv"

    if not os.path.exists(trace_file):
        return None

    group_col = args.group_by  # 'owner', 'chiplet', or 'pc' (global)
    print(f"[{benchmark}] Analyzing PC stats per {group_col}...")

    try:
        # 'type' 컬럼을 추가로 로드하여 L2 Miss 여부를 확인합니다.
        # 'is_ptw' 는 page-table walk(PTE) 접근 표시. 최신 trace에만 존재하므로
        # 누락된 경우 access-type 필터를 우회한다.
        wanted_cols = ['pc', 'addr', 'chiplet', 'rdma', 'owner', 'rw', 'type', 'data_id']
        header_cols = pd.read_csv(trace_file, nrows=0, skipinitialspace=True).columns.tolist()
        has_is_ptw = 'is_ptw' in header_cols
        if has_is_ptw:
            wanted_cols.append('is_ptw')

        df = pd.read_csv(trace_file, skipinitialspace=True)
        df = df.drop_duplicates()
        df = df[wanted_cols]

        if df.empty:
            return None

        # is_ptw 가 bool/문자/0-1 어떤 형태로 들어와도 통일
        if has_is_ptw:
            df['is_ptw'] = (
                df['is_ptw'].astype(str).str.strip().str.lower()
                .isin(['true', '1', 't', 'yes'])
            )

        if args.access_type == 'data':
            if not has_is_ptw:
                print(f"[{benchmark}] WARNING: trace has no 'is_ptw' column; "
                      f"--access-type=data falls back to all accesses.")
            else:
                df = df[~df['is_ptw']]
        elif args.access_type == 'pte':
            if not has_is_ptw:
                print(f"[{benchmark}] SKIP: trace has no 'is_ptw' column; "
                      f"--access-type=pte cannot be computed.")
                return None
            df = df[df['is_ptw']]

        if df.empty:
            print(f"[{benchmark}] No rows remain after access-type filter "
                  f"({args.access_type}); skipping.")
            return None

        if args.group_size != 0:
            group_size = args.group_size
            df['addr'] = df['addr'] // (64 * group_size)

        src_x, src_y = df['chiplet'] % xDim, df['chiplet'] // xDim
        dst_x, dst_y = df['owner']   % xDim, df['owner']   // xDim
        df['hop_count'] = (src_x - dst_x).abs() + (src_y - dst_y).abs()

        # 2. 캐시 라인(addr)별 평균 요청 칩렛 개수 (address-equal weight: 모든 addr 동등 가중)
        addr_chiplets = df.groupby(['pc', 'addr'])['chiplet'].nunique().reset_index(name='chiplet_count')
        avg_chiplets_per_addr = addr_chiplets.groupby('pc')['chiplet_count'].mean().reset_index(name='avg_requestor_chiplets')

        # 3. group_col(owner=홈 노드 / chiplet=요청자)와 'pc'별로 그룹화하여 통계 추출
        # unique_chiplets: 이 (group_col, pc) 그룹을 접근한 서로 다른 칩렛 수.
        #   PC 자체 단위의 칩렛 공유도(per-PC sharing degree)를 group_col별로 본 뒤
        #   pc_agg 단계에서 평균낸다. group_col='chiplet'이면 항상 1이 되어
        #   의미가 없지만 group_col='owner'일 때는 "이 홈 노드의 PC 데이터를
        #   몇 개의 요청 칩렛이 만졌나"의 평균이 된다.
        # group_col='pc'(global)이면 owner/chiplet 분할 없이 PC 단위로만 묶어
        #   전체 칩렛에 대한 캐시라인 메트릭을 그대로 집계한다. 이 경우 키가
        #   ['pc']로 중복 제거되어, 뒤따르는 pc_agg의 mean은 단일값을 그대로 통과시킨다.
        group_keys = list(dict.fromkeys([group_col, 'pc']))
        stats = df.groupby(group_keys).agg(
            access_count=('addr', 'count'),
            unique_addrs=('addr', 'nunique'),
            unique_chiplets=('chiplet', 'nunique'),
            read_count=('rw', lambda x: (x == 0).sum()),
            write_count=('rw', lambda x: (x == 1).sum()),
            avg_hop=('hop_count', 'mean'),
        ).reset_index()
        stats['access_per_addr'] = stats['access_count'] / stats['unique_addrs']

        # 4. 같은 PC에 대해 여러 group_col 값들에 대한 stat의 평균을 계산 (내부 이름은 중립)
        pc_agg = stats.groupby('pc').agg(
            avg_unique_addrs=('unique_addrs', 'mean'),
            avg_unique_chiplets=('unique_chiplets', 'mean'),
            avg_read_count=('read_count', 'mean'),
            avg_write_count=('write_count', 'mean'),
            avg_access_per_addr=('access_per_addr', 'mean'),
            avg_hop_count=('avg_hop', 'mean'),
            total_access_count=('access_count', 'sum')
        ).reset_index()

        pc_agg = pd.merge(pc_agg, avg_chiplets_per_addr, on='pc')

        total_unique_addrs_per_pc = df.groupby('pc')['addr'].nunique().reset_index(name='total_unique_addrs')
        pc_agg = pd.merge(pc_agg, total_unique_addrs_per_pc, on='pc')

        # PC가 접근하는 data_id 들의 정렬된 고유 목록 (-1 은 추적 불가 entry 라 제외)
        data_ids_per_pc = (
            df[df['data_id'] != -1]
            .groupby('pc')['data_id']
            .agg(lambda s: sorted(set(int(v) for v in s)))
            .reset_index(name='data_ids')
        )
        pc_agg = pd.merge(pc_agg, data_ids_per_pc, on='pc', how='left')
        pc_agg['data_ids'] = pc_agg['data_ids'].apply(lambda v: v if isinstance(v, list) else [])

        # Read/Write 비율 계산 (분모를 avg_read + avg_write로 잡아 avg_access 없이도 동일한 값)
        rw_sum = pc_agg['avg_read_count'] + pc_agg['avg_write_count']
        pc_agg['read_ratio_%']  = (pc_agg['avg_read_count']  / rw_sum * 100).round(2).fillna(0)
        pc_agg['write_ratio_%'] = (pc_agg['avg_write_count'] / rw_sum * 100).round(2).fillna(0)
        pc_agg['avg_access_per_addr'] = pc_agg['avg_access_per_addr'].round(2)
        pc_agg['avg_hop_count'] = pc_agg['avg_hop_count'].round(2)

        # 마지막에 _per_{group_col} suffix 부여
        pc_agg = pc_agg.rename(columns={
            'avg_unique_addrs':    f'avg_unique_addrs_per_{group_col}',
            'avg_unique_chiplets': f'avg_unique_chiplets_per_{group_col}',
            'avg_access_per_addr': f'avg_access_per_addr_per_{group_col}',
            'avg_hop_count':       f'avg_hop_count_per_{group_col}',
        })

        pc_agg.insert(0, 'benchmark', benchmark)

        column_order = [
            'benchmark', 'pc',
            'total_access_count', 'total_unique_addrs',
            f'avg_unique_addrs_per_{group_col}',
            f'avg_access_per_addr_per_{group_col}',
            'avg_requestor_chiplets',
            f'avg_unique_chiplets_per_{group_col}',
            f'avg_hop_count_per_{group_col}',
            'read_ratio_%', 'write_ratio_%',
            'data_ids',
        ]
        pc_agg = pc_agg[column_order]

        # 정렬: 총 접근 횟수 기준 내림차순
        pc_agg = pc_agg.sort_values(by='total_access_count', ascending=False)

        return pc_agg

    except Exception as e:
        print(f"[{benchmark}] Error: {e}")
        return None


if __name__ == "__main__":
    import argparse
    parser = argparse.ArgumentParser(description="Analyze PC stats per Owner or Chiplet.")
    parser.add_argument('run_id', nargs='?', default='latest',
                        help="run-id (latest, <config>, <config>/<timestamp>, ...)")
    parser.add_argument('--group-size', type=int, default=1, help='Group size for address grouping')
    parser.add_argument('--group-by', choices=['owner', 'chiplet', 'pc'], default='owner',
                        help="grouping key for per-PC aggregation: 'owner' (data home node, default), "
                             "'chiplet' (requester), or 'pc' (global: no owner/chiplet split, "
                             "metrics over all chiplets per PC)")
    parser.add_argument('--access-type', choices=['all', 'data', 'pte'], default='all',
                        help="memory access subset to aggregate. 'all' (default) keeps every row, "
                             "'data' filters out PTW (page-table walk) accesses, "
                             "'pte' keeps only PTW accesses. Requires the trace to have "
                             "the 'is_ptw' column (data/pte modes).")
    parser.add_argument('--workers', type=int, default=None,
                        help="parallel worker processes (default: CPU count). "
                             "Lower it when large traces make memory the bottleneck; "
                             "each worker loads one benchmark's trace.")
    args = parser.parse_args()
    if args.workers is not None and args.workers < 1:
        parser.error("--workers must be >= 1")

    with ProcessPoolExecutor(max_workers=args.workers) as executor:
        results = list(executor.map(process_benchmark, benchmarks, repeat(args)))

    results = [r for r in results if r is not None]

    if results:
        final_df = pd.concat(results, ignore_index=True)

        result_dir = f"{username}/mgvm/analyze/_out/pc-stats/{date}"
        os.makedirs(result_dir, exist_ok=True)

        suffix = f"_{args.group_size}" if args.group_size != 0 else ""
        access_suffix = "" if args.access_type == 'all' else f"_{args.access_type}"
        output_path = f"{result_dir}/t_pc_stats_all_per_{args.group_by}{suffix}{access_suffix}.xlsx"

        final_df.to_excel(output_path, index=False)

        print("\n" + "="*80)
        print(f"{f'PC Stats for All Memory Accesses per {args.group_by}':^80}")
        print("="*80)
        print(f"Total Rows: {len(final_df)}")
        print(f"Saved results to: {output_path}")
        print("="*80)
        print(final_df.head(15).to_string(index=False))
    else:
        print("No results generated.")