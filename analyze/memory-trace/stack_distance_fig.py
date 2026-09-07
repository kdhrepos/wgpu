"""stack distance histogram (classic).

참조 이미지: _out/cacheline-metrics-figs/stack_distance/mis_stack_distance.png
- x: stack distance (log)
- y: access count (histplot)
- hue: heat_group { Top Hot 1% / Mid Hot 10% / Other }
- crimson / orange / silver, Top Hot median 점선
- PC 색깔 사용 안 함 (요청대로 클래식)

stack_distance.py와 독립적으로 raw memory trace 다시 읽고 cacheline 단위 BIT로
계산. (per-access SD는 CSV에 안 들어가 있어서.)

실행:
    analyze/run.sh memory-trace/stack_distance_fig.py mesh-6x6/latest
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
import seaborn as sns
from concurrent.futures import ProcessPoolExecutor

sys.path.append(os.path.abspath(os.path.join(os.path.dirname(__file__), "../common")))
sys.path.append(os.path.abspath(os.path.join(os.path.dirname(__file__), "../../scripts")))

from _paths import run_root, memory_trace_dir, run_config, run_tag
from params import benchmarks
from paddr_map import l2_bank_vec, l2_set_vec, NUM_WAYS

LINE_SHIFT = 6
HOT_PCT = 1
MID_PCT = 10

TRACE_DIR = None
OUT_DIR = None
XDIM = YDIM = 6


def compute_sd(blocks: np.ndarray) -> np.ndarray:
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


def process_benchmark(benchmark):
    trace = f"{TRACE_DIR}/{benchmark}_mesh_{XDIM}x{YDIM}_memory_trace.csv"
    if not os.path.exists(trace):
        return f"[{benchmark}] no trace"

    out_png = f"{OUT_DIR}/{benchmark}_stack_distance.png"

    print(f"[{benchmark}] load...", flush=True)
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

    # heat_group by access frequency
    counts = df.groupby('block').size().sort_values(ascending=False)
    total_unique = len(counts)
    top_n = max(1, int(total_unique * HOT_PCT / 100))
    mid_n = max(1, int(total_unique * MID_PCT / 100))
    hot_set = set(counts.iloc[:top_n].index.tolist())
    mid_set = set(counts.iloc[:mid_n].index.tolist())

    def heat(b):
        if b in hot_set:
            return 'Top Hot (Top 1%)'
        if b in mid_set:
            return 'Mid Hot (Top 10%)'
        return 'Other (Cold)'

    # (chiplet, bank, set) stream별 SD → 모든 stream union을 histogram.
    print(f"[{benchmark}] SD ({len(df):,} access, "
          f"{df['chiplet'].nunique()} chiplets, ways={NUM_WAYS})...",
          flush=True)
    sd_all = np.full(len(df), -1, dtype=np.int64)
    for _, g in df.groupby(['chiplet', 'l2_bank', 'l2_set'], sort=False):
        idx = g.index.to_numpy()
        sd_all[idx] = compute_sd(g['block'].to_numpy())
    df['stack_distance'] = sd_all
    df = df[df['stack_distance'] > 0]
    if df.empty:
        return f"[{benchmark}] no warm access"
    df['heat_group'] = df['block'].map(heat)

    palette = {'Top Hot (Top 1%)': 'crimson',
               'Mid Hot (Top 10%)': 'orange',
               'Other (Cold)': 'silver'}

    plt.figure(figsize=(12, 8))
    ax = sns.histplot(
        data=df, x='stack_distance', hue='heat_group',
        hue_order=['Other (Cold)', 'Mid Hot (Top 10%)', 'Top Hot (Top 1%)'],
        palette=palette, bins=100, log_scale=True, alpha=0.75,
        edgecolor='black', linewidth=0.4, multiple="stack",
    )

    # way 기준선: SD < ways → set 안에서 hit 후보
    way_line = plt.axvline(NUM_WAYS, color='black', linestyle=':',
                           linewidth=2, label=f'L2 ways = {NUM_WAYS}')

    top = df[df['heat_group'] == 'Top Hot (Top 1%)']
    median_line = None
    if not top.empty:
        med = top['stack_distance'].median()
        median_line = plt.axvline(med, color='crimson', linestyle='--',
                                  linewidth=2, label=f'Top Hot Median: {med:.0f}')

    handles, labels = ax.get_legend_handles_labels()
    order = ['Top Hot (Top 1%)', 'Mid Hot (Top 10%)', 'Other (Cold)']
    fh = [handles[labels.index(n)] for n in order if n in labels]
    fl = [n for n in order if n in labels]
    if median_line is not None:
        fh.append(median_line)
        fl.append(median_line.get_label())
    fh.append(way_line)
    fl.append(way_line.get_label())
    plt.legend(fh, fl, title="Access Rank",
               loc='upper left', bbox_to_anchor=(1.0, 1.0))

    plt.title(f"[{benchmark}] Stack Distance\n"
              f"Unique Cachelines: {total_unique:,}   "
              f"Accesses: {len(df):,}",
              fontweight='bold')
    plt.xlabel('Stack Distance (Log Scale)')
    plt.ylabel('Access Count')
    plt.grid(True, which='both', alpha=0.2)
    plt.tight_layout()

    os.makedirs(OUT_DIR, exist_ok=True)
    plt.savefig(out_png, dpi=400, bbox_inches='tight')
    plt.close()
    return f"[{benchmark}] saved {out_png}"


if __name__ == "__main__":
    ap = argparse.ArgumentParser()
    ap.add_argument('run_id', nargs='?', default='mesh-6x6')
    ap.add_argument('--workers', type=int,
                    default=int(os.environ.get("STACK_DIST_WORKERS", "8")))
    args = ap.parse_args()

    root = run_root(args.run_id)
    TRACE_DIR = str(memory_trace_dir(args.run_id))
    config, ts = run_config(args.run_id), run_tag(args.run_id)
    m = re.search(r'(\d+)x(\d+)', config)
    if m:
        XDIM, YDIM = int(m.group(1)), int(m.group(2))

    OUT_DIR = os.path.abspath(os.path.join(
        os.path.dirname(__file__), "..", "_out", "stack-distance-fig",
        f"{config}-{ts}"))
    os.makedirs(OUT_DIR, exist_ok=True)

    print(f"Run:   {config}/{ts}")
    print(f"Trace: {TRACE_DIR}")
    print(f"Out:   {OUT_DIR}")

    with ProcessPoolExecutor(max_workers=args.workers) as ex:
        for msg in ex.map(process_benchmark, benchmarks):
            print(msg, flush=True)
