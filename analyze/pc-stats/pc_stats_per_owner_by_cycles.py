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
_run_id = next((a for a in sys.argv[1:] if not a.startswith('-')), 'latest')
from _paths import run_root as _run_root, memory_trace_dir as _mtd, run_tag as _run_tag
date = _run_tag(_run_id)
trace_dir = str(_mtd(_run_id))
xDim = 6
yDim = 6

# 모드별 메타데이터: 필터 설명 / 출력 파일 접두사 (group_by suffix는 동적 부여) / 헤더 타이틀 prefix
MODE_INFO = {
    'all':        ('All memory accesses',              't_pc_stats_all',         'PC Stats for All Memory Accesses'),
    'l2miss':     ('L2 Miss (RDMA, type in [2,5])',    't_pc_stats_l2miss',      'PC Stats for L2 Miss (RDMA)'),
    'l2miss_pte': ('L2 Miss + PTE (is_ptw==True)',     't_pc_stats_l2miss_pte',  'PC Stats for L2 Miss (RDMA), PTE'),
    'rdma':       ('Remote access (rdma==True)',       't_pc_stats_rdma',        'PC Stats for RDMA (Remote)'),
}


def apply_mode_filter(df, mode):
    """모드에 따라 트레이스를 필터링."""
    if mode == 'all':
        return df
    if mode == 'l2miss':
        return df[(df['rdma'] == True) & (df['type'].isin([2, 5]))].copy()
    if mode == 'l2miss_pte':
        df = df[(df['rdma'] == True) & (df['type'].isin([2, 5]))].copy()
        return df[df['is_ptw'] == True]
    if mode == 'rdma':
        return df[df['rdma'] == True].copy()
    raise ValueError(f"Unknown mode: {mode}")


def process_benchmark(benchmark, args):
    trace_file = f"{trace_dir}/{benchmark}_mesh_{xDim}x{yDim}_memory_trace.csv"

    if not os.path.exists(trace_file):
        return None

    desc, _, _ = MODE_INFO[args.mode]
    group_col = args.group_by
    print(f"[{benchmark}] Analyzing PC stats ({desc}) per {group_col} up to {args.end_cycle} cycles...")

    try:
        # time 값을 포함하여 로드. l2miss_pte 모드는 is_ptw 컬럼 추가 필요.
        usecols = ['time', 'pc', 'addr', 'chiplet', 'rdma', 'owner', 'rw', 'type']
        if args.mode == 'l2miss_pte':
            usecols.append('is_ptw')
        df = pd.read_csv(trace_file, skipinitialspace=True)
        df = df.drop_duplicates()
        df = df[usecols]

        # 시작 시간(0 사이클)을 트레이스의 최소 시간으로 설정하여 상대적인 시간(사이클) 계산 가능하도록 정규화
        start_time = df['time'].min()
        df['time'] = df['time'] - start_time

        # 1 ns = 1e-9 seconds 이므로, 변환
        time_limit = args.end_cycle * 1e-9

        # 시간 범위 필터링은 모든 모드에 공통
        df = df[df['time'] <= time_limit].copy()

        # 모드별 필터링
        df = apply_mode_filter(df, args.mode)

        if df.empty:
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
        stats = df.groupby([group_col, 'pc']).agg(
            access_count=('addr', 'count'),
            unique_addrs=('addr', 'nunique'),
            read_count=('rw', lambda x: (x == 0).sum()),
            write_count=('rw', lambda x: (x == 1).sum()),
            avg_hop=('hop_count', 'mean'),
        ).reset_index()
        stats['access_per_addr'] = stats['access_count'] / stats['unique_addrs']

        # 4. 같은 PC에 대해 여러 group_col 값들에 대한 stat의 평균 (내부 이름은 중립)
        pc_agg = stats.groupby('pc').agg(
            avg_unique_addrs=('unique_addrs', 'mean'),
            avg_read_count=('read_count', 'mean'),
            avg_write_count=('write_count', 'mean'),
            avg_access_per_addr=('access_per_addr', 'mean'),
            avg_hop_count=('avg_hop', 'mean'),
            total_access_count=('access_count', 'sum')
        ).reset_index()

        pc_agg = pd.merge(pc_agg, avg_chiplets_per_addr, on='pc')

        total_unique_addrs_per_pc = df.groupby('pc')['addr'].nunique().reset_index(name='total_unique_addrs')
        pc_agg = pd.merge(pc_agg, total_unique_addrs_per_pc, on='pc')

        rw_sum = pc_agg['avg_read_count'] + pc_agg['avg_write_count']
        pc_agg['read_ratio_%']  = (pc_agg['avg_read_count']  / rw_sum * 100).round(2).fillna(0)
        pc_agg['write_ratio_%'] = (pc_agg['avg_write_count'] / rw_sum * 100).round(2).fillna(0)
        pc_agg['avg_access_per_addr'] = pc_agg['avg_access_per_addr'].round(2)
        pc_agg['avg_hop_count'] = pc_agg['avg_hop_count'].round(2)

        pc_agg = pc_agg.rename(columns={
            'avg_unique_addrs':    f'avg_unique_addrs_per_{group_col}',
            'avg_access_per_addr': f'avg_access_per_addr_per_{group_col}',
            'avg_hop_count':       f'avg_hop_count_per_{group_col}',
        })

        pc_agg.insert(0, 'benchmark', benchmark)

        column_order = [
            'benchmark', 'pc',
            'total_access_count', 'total_unique_addrs',
            f'avg_unique_addrs_per_{group_col}',
            f'avg_access_per_addr_per_{group_col}',
            'avg_requestor_chiplets', f'avg_hop_count_per_{group_col}',
            'read_ratio_%', 'write_ratio_%',
        ]
        pc_agg = pc_agg[column_order]

        pc_agg = pc_agg.sort_values(by='total_access_count', ascending=False)

        return pc_agg

    except Exception as e:
        print(f"[{benchmark}] Error: {e}")
        return None


if __name__ == "__main__":
    import argparse
    parser = argparse.ArgumentParser(description="Analyze PC stats per Owner or Chiplet up to specific cycle (multiple filter modes).")
    parser.add_argument('run_id', nargs='?', default='latest',
                        help="run-id (latest, <config>, <config>/<timestamp>, ...)")
    parser.add_argument('--group-size', type=int, default=1, help='Group size for address grouping')
    # n 사이클을 입력받기 위한 인자 (--end-cycle)
    parser.add_argument('--end-cycle', type=float, required=True, help='End cycle limit for the analysis')
    parser.add_argument('--mode', choices=list(MODE_INFO.keys()), default='l2miss',
                        help='Filter mode: all | l2miss | l2miss_pte | rdma (default: l2miss)')
    parser.add_argument('--group-by', choices=['owner', 'chiplet'], default='owner',
                        help="grouping key for per-PC aggregation: 'owner' (data home node, default) or 'chiplet' (requester)")
    args = parser.parse_args()

    with ProcessPoolExecutor() as executor:
        results = list(executor.map(process_benchmark, benchmarks, repeat(args)))

    results = [r for r in results if r is not None]

    if results:
        final_df = pd.concat(results, ignore_index=True)

        result_dir = f"{username}/mgvm/analyze/_out/pc-stats/{date}/cycles"
        os.makedirs(result_dir, exist_ok=True)

        _, file_prefix, title = MODE_INFO[args.mode]
        suffix = f"_{args.group_size}" if args.group_size != 0 else ""
        output_path = f"{result_dir}/{file_prefix}_per_{args.group_by}_up_to_{int(args.end_cycle)}cycles{suffix}.xlsx"

        final_df.to_excel(output_path, index=False)

        print("\n" + "="*80)
        print(f"{f'{title} per {args.group_by} up to {args.end_cycle} Cycles':^80}")
        print("="*80)
        print(f"Total Rows: {len(final_df)}")
        print(f"Saved results to: {output_path}")
        print("="*80)
        print(final_df.head(15).to_string(index=False))
    else:
        print("No results generated.")
