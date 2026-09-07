"""set-level stack distance CDF.

stack_distance.py가 만든 `*_set_summary.csv`를 읽음:
    columns: chiplet, l2_bank, l2_set, access_count, unique_blocks,
             sd_max, sd_p95, sd_p50, sd_mean

set 한 개당 점 하나로 보고, "그 set의 SD 통계가 x 이하인 set 비율"을 CDF로.

x축 = stack distance threshold (log)
y축 = CDF (set 비율)
black 점선 = NUM_WAYS (16) → 그 지점의 y값 = "16-way 안에서 다 잡히는 set 비율"

벤치마크별 figure 한 장. set의 sd_max / sd_p95 / sd_p50 3선.

실행:
    analyze/run.sh memory-trace/stack_distance_set_cdf_fig.py mesh-6x6/latest
"""

import os
import re
import sys
import argparse
import numpy as np
import pandas as pd
import matplotlib
matplotlib.use('Agg')
import matplotlib.pyplot as plt
from concurrent.futures import ProcessPoolExecutor

sys.path.append(os.path.abspath(os.path.join(os.path.dirname(__file__), "../common")))
sys.path.append(os.path.abspath(os.path.join(os.path.dirname(__file__), "../../scripts")))

from _paths import run_root, memory_trace_dir, run_config, run_tag
from params import benchmarks
from paddr_map import l2_bank_vec, l2_set_vec, NUM_WAYS


LINE_SHIFT = 6

# fork된 worker에 상속될 globals
CSV_DIR = None
TRACE_DIR = None
XDIM = YDIM = 6


# ----------------- raw trace에서 set_summary 즉석 생성 -----------------
def _compute_sd(blocks: np.ndarray) -> np.ndarray:
    """BIT-based LRU stack distance. cold = -1."""
    n = len(blocks)
    out = np.full(n, -1, dtype=np.int64)
    size = n + 2
    bit = np.zeros(size, dtype=np.int64)
    last = {}

    def upd(i, v):
        i += 1
        while i < size:
            bit[i] += v
            i += i & -i

    def qry(i):
        i += 1
        s = 0
        while i > 0:
            s += bit[i]
            i -= i & -i
        return s

    for i in range(n):
        b = int(blocks[i])
        if b in last:
            prev = last[b]
            out[i] = qry(i - 1) - qry(prev)
            upd(prev, -1)
        upd(i, 1)
        last[b] = i
    return out


def build_set_summary(benchmark):
    """raw trace 읽어 set_summary CSV 생성. 이미 있으면 skip."""
    out_csv = f"{CSV_DIR}/{benchmark}_set_summary.csv"
    if os.path.exists(out_csv):
        return f"[{benchmark}] set_summary exists"

    trace = f"{TRACE_DIR}/{benchmark}_mesh_{XDIM}x{YDIM}_memory_trace.csv"
    if not os.path.exists(trace):
        return f"[{benchmark}] no trace"

    cols = ['time', 'addr', 'type', 'chiplet']
    dtype = {'time': 'float64', 'addr': 'uint64',
             'type': 'int8', 'chiplet': 'int16'}
    try:
        df = pd.read_csv(trace, usecols=cols, dtype=dtype, skipinitialspace=True)
    except Exception as e:
        return f"[{benchmark}] read fail: {e}"

    df = df.drop_duplicates()
    df = df[~df['type'].isin([6, 7, 8, 9])]
    if df.empty:
        return f"[{benchmark}] empty"

    df = df.sort_values('time', kind='mergesort').reset_index(drop=True)
    df['block'] = df['addr'].to_numpy() >> LINE_SHIFT
    df['l2_bank'] = l2_bank_vec(df['addr'].to_numpy())
    df['l2_set'] = l2_set_vec(df['addr'].to_numpy())

    print(f"[{benchmark}] computing SD per (chiplet,bank,set)... "
          f"{len(df):,} access", flush=True)
    sd_all = np.full(len(df), -1, dtype=np.int64)
    for _, g in df.groupby(['chiplet', 'l2_bank', 'l2_set'], sort=False):
        idx = g.index.to_numpy()
        sd_all[idx] = _compute_sd(g['block'].to_numpy())
    df['stack_distance'] = sd_all

    warm = df[df['stack_distance'] >= 0]
    if warm.empty:
        return f"[{benchmark}] no warm access"

    set_sum = warm.groupby(['chiplet', 'l2_bank', 'l2_set'],
                           as_index=False).agg(
        access_count=('stack_distance', 'size'),
        sd_max=('stack_distance', 'max'),
        sd_p95=('stack_distance', lambda s: int(np.percentile(s, 95))),
        sd_p50=('stack_distance', 'median'),
        sd_mean=('stack_distance', 'mean'),
    )
    unique = df.groupby(['chiplet', 'l2_bank', 'l2_set'],
                        as_index=False)['block'].nunique()
    unique = unique.rename(columns={'block': 'unique_blocks'})
    set_sum = set_sum.merge(unique, on=['chiplet', 'l2_bank', 'l2_set'])
    os.makedirs(CSV_DIR, exist_ok=True)
    set_sum.to_csv(out_csv, index=False)
    return f"[{benchmark}] built set_summary -> {out_csv}"


def cdf_xy(arr):
    a = np.sort(np.asarray(arr, dtype='float64'))
    a = a[a > 0]
    if len(a) == 0:
        return None, None
    y = np.arange(1, len(a) + 1) / len(a)
    return a, y


def plot_one(csv_path, benchmark, out_path):
    df = pd.read_csv(csv_path)
    if df.empty:
        print(f"[{benchmark}] empty")
        return

    plt.figure(figsize=(10, 6))
    for col, label, color, ls in [
        ('sd_max', 'set max SD', 'crimson', '-'),
        ('sd_p95', 'set 95p SD', 'orange', '-'),
        ('sd_p50', 'set median SD', 'steelblue', '-'),
    ]:
        x, y = cdf_xy(df[col])
        if x is None:
            continue
        plt.step(x, y, where='post', color=color, linewidth=1.8,
                 linestyle=ls, label=label)

    plt.axvline(NUM_WAYS, color='black', linestyle=':', linewidth=2,
                label=f'L2 ways = {NUM_WAYS}')

    plt.xscale('log')
    plt.ylim(0, 1.02)
    plt.xlabel('Stack Distance (log)')
    plt.ylabel('Fraction of sets (CDF)')
    plt.title(f'[{benchmark}] Set-level Stack Distance CDF\n'
              f'sets: {len(df):,}    way line crossing = sets fully fit in L2',
              fontweight='bold')
    plt.grid(True, which='both', alpha=0.3)
    plt.legend(loc='lower right')
    plt.tight_layout()
    os.makedirs(os.path.dirname(out_path), exist_ok=True)
    plt.savefig(out_path, dpi=400, bbox_inches='tight')
    plt.close()
    print(f"saved {out_path}")


def plot_combined(csv_dir, out_path, metric='sd_p95'):
    import seaborn as sns
    palette = sns.color_palette("tab20", len(benchmarks))
    plt.figure(figsize=(12, 7))
    drew = 0
    for i, b in enumerate(benchmarks):
        csv = f"{csv_dir}/{b}_set_summary.csv"
        if not os.path.exists(csv):
            continue
        df = pd.read_csv(csv)
        x, y = cdf_xy(df[metric])
        if x is None:
            continue
        plt.step(x, y, where='post', color=palette[i],
                 linewidth=1.4, label=b)
        drew += 1
    if drew == 0:
        print("no set_summary csv found")
        return
    plt.axvline(NUM_WAYS, color='black', linestyle=':', linewidth=2,
                label=f'L2 ways = {NUM_WAYS}')
    plt.xscale('log')
    plt.ylim(0, 1.02)
    plt.xlabel(f'{metric} per set (log)')
    plt.ylabel('Fraction of sets (CDF)')
    plt.title(f'Set-level stack distance CDF ({metric}) across benchmarks',
              fontweight='bold')
    plt.grid(True, which='both', alpha=0.3)
    plt.legend(fontsize=8, ncol=2, loc='lower right')
    plt.tight_layout()
    os.makedirs(os.path.dirname(out_path), exist_ok=True)
    plt.savefig(out_path, dpi=400, bbox_inches='tight')
    plt.close()
    print(f"saved {out_path}")


if __name__ == "__main__":
    ap = argparse.ArgumentParser()
    ap.add_argument('run_id', nargs='?', default='mesh-6x6')
    ap.add_argument('--csv-dir', type=str, default=None)
    ap.add_argument('--workers', type=int,
                    default=int(os.environ.get("SET_CDF_WORKERS", "8")))
    args = ap.parse_args()

    root = run_root(args.run_id)
    config, ts = run_config(args.run_id), run_tag(args.run_id)
    tag = f"{config}-{ts}"

    CSV_DIR = args.csv_dir or os.path.abspath(os.path.join(
        os.path.dirname(__file__), "..", "_out", "stack-distance", tag))
    TRACE_DIR = str(memory_trace_dir(args.run_id))
    m = re.search(r'(\d+)x(\d+)', config)
    if m:
        XDIM, YDIM = int(m.group(1)), int(m.group(2))
    out_root = os.path.abspath(os.path.join(
        os.path.dirname(__file__), "..", "_out", "stack-distance-set-cdf-fig", tag))
    os.makedirs(out_root, exist_ok=True)
    print(f"CSV in:  {CSV_DIR}")
    print(f"Trace:   {TRACE_DIR}")
    print(f"Fig out: {out_root}")
    print(f"Grid:    {XDIM}x{YDIM}   workers={args.workers}")

    # 누락된 set_summary CSV는 raw trace에서 즉석 생성 (병렬)
    print("\n=== set_summary 확인/생성 ===")
    with ProcessPoolExecutor(max_workers=args.workers) as ex:
        for msg in ex.map(build_set_summary, benchmarks):
            print(msg, flush=True)

    # 플롯
    print("\n=== CDF plot ===")
    for b in benchmarks:
        csv = f"{CSV_DIR}/{b}_set_summary.csv"
        if not os.path.exists(csv):
            print(f"[{b}] still no set_summary, skip")
            continue
        plot_one(csv, b, f"{out_root}/{b}_set_cdf.png")

    plot_combined(CSV_DIR, f"{out_root}/_combined_p95.png", metric='sd_p95')
    plot_combined(CSV_DIR, f"{out_root}/_combined_max.png", metric='sd_max')
