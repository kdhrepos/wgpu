"""Per-chiplet data volume accessed over time, from the memory trace.

For each benchmark: split the trace's [t_min, t_max] into N equal time
windows.  For every (chiplet, window) count the distinct cachelines the
chiplet's L2 touched via demand requests (type 0-5), convert to MB
(unique_lines * 64B), and render one heatmap per benchmark.

    x = time window   y = chiplet id   color = MB of distinct data touched

Output: analyze/_out/memory-trace/chiplet-access-over-time/<config>-<tag>/
    <bench>_out.png    heatmap
    <bench>_out.csv    chiplet x window matrix (MB)
    all_benchmarks_long_out.csv

Usage:
    analyze/run.sh memory-trace/chiplet_access_over_time.py <run-id>
    analyze/run.sh memory-trace/chiplet_access_over_time.py mesh-6x6/latest --bins 120 -j 3
"""

from __future__ import annotations

import argparse
import os
from concurrent.futures import ProcessPoolExecutor
from itertools import repeat

import numpy as np
import pandas as pd
import matplotlib.pyplot as plt

from _paths import memory_trace_dir, run_config, run_tag, OUT_ROOT
from motivation_common import file_suffix_for_config, trace_chunks
from pc_hotness import mesh_dims_from_config
from plot_style import apply_paper_style, save_fig
from params import benchmarks

USECOLS = ['time', 'addr', 'type', 'chiplet']
DEMAND_LO, DEMAND_HI = 0, 5           # type 0-5 = demand read/write, exclude evict/PTW
LINE_BYTES = 64
LINE_MB = LINE_BYTES / (1024.0 ** 2)
DEFAULT_BINS = 100


def _load(trace_file: str):
    ts, ln, chs = [], [], []
    for chunk in trace_chunks(trace_file, usecols=USECOLS):
        chunk = chunk.drop_duplicates()
        t = chunk['type'].to_numpy(dtype=np.int8)
        m = (t >= DEMAND_LO) & (t <= DEMAND_HI)
        if not m.any():
            continue
        ts.append(chunk['time'].to_numpy(dtype=np.float64)[m])
        ln.append(chunk['addr'].to_numpy(dtype=np.uint64)[m] >> np.uint64(6))
        chs.append(chunk['chiplet'].to_numpy(dtype=np.int16)[m])
    if not ts:
        return None
    return np.concatenate(ts), np.concatenate(ln), np.concatenate(chs)


def process_benchmark(bench: str, trace_dir: str, suffix: str,
                      n_chiplets: int, nbins: int):
    trace_file = f"{trace_dir}/{bench}_{suffix}_memory_trace.csv"
    if not os.path.exists(trace_file):
        print(f"[{bench}] no trace, skip", flush=True)
        return None

    print(f"[{bench}] load ...", flush=True)
    loaded = _load(trace_file)
    if loaded is None:
        print(f"[{bench}] no demand events, skip", flush=True)
        return None
    times, lines, chips = loaded

    t_min = float(times.min())
    t_max = float(times.max())
    span = t_max - t_min
    if span <= 0:
        bin_idx = np.zeros(times.shape, dtype=np.int64)
    else:
        bin_idx = np.floor((times - t_min) / span * nbins).astype(np.int64)
        np.clip(bin_idx, 0, nbins - 1, out=bin_idx)
    del times

    d = pd.DataFrame({'c': chips, 'b': bin_idx.astype(np.int32), 'l': lines})
    d = d.drop_duplicates()
    g = d.groupby(['c', 'b'], sort=False).size()

    mat = np.zeros((n_chiplets, nbins), dtype=np.float64)
    ci = g.index.get_level_values('c').to_numpy()
    bi = g.index.get_level_values('b').to_numpy()
    keep = (ci >= 0) & (ci < n_chiplets) & (bi >= 0) & (bi < nbins)
    mat[ci[keep], bi[keep]] = g.to_numpy()[keep] * LINE_MB

    print(f"[{bench}] done  events={len(lines):,}  "
          f"peak_cell={mat.max():.2f} MB", flush=True)
    return bench, mat, t_min, t_max


def plot_bench(bench, mat, t_min, t_max, n_chiplets, nbins, out_dir):
    apply_paper_style()
    span_ms = max((t_max - t_min) * 1e3, 1e-9)

    fig, ax = plt.subplots(figsize=(7.2, 4.4))
    im = ax.imshow(mat, aspect='auto', origin='lower', cmap='magma',
                   interpolation='nearest',
                   extent=[0.0, span_ms, -0.5, n_chiplets - 0.5])
    ax.grid(False)
    ax.set_xlabel('Time (ms)')
    ax.set_ylabel('Chiplet ID')
    ax.set_title(f'{bench}: distinct data accessed per chiplet per '
                 f'{span_ms / nbins:.3g} ms window')
    step = max(1, n_chiplets // 12)
    ax.set_yticks(range(0, n_chiplets, step))

    cb = fig.colorbar(im, ax=ax, pad=0.02)
    cb.set_label('Distinct data touched (MB / window)')

    save_fig(fig, out_dir, f'{bench}_out')


def plot_total_touched(bench, mat, xdim, n_chiplets, out_dir):
    """Per-chiplet total data touched over the whole run: bar + mesh-layout grid.
    total = sum over windows (a line touched in K windows counts K times)."""
    apply_paper_style()
    vals = mat.sum(axis=1)
    ydim = n_chiplets // xdim
    grid = np.full((ydim, xdim), np.nan)
    for c in range(n_chiplets):
        grid[c // xdim, c % xdim] = vals[c]

    fig, (ax0, ax1) = plt.subplots(1, 2, figsize=(7.4, 3.0),
                                   gridspec_kw={'width_ratios': [1.7, 1.0]})
    ax0.bar(range(n_chiplets), vals, color='#0072B2', width=0.8)
    ax0.set_xlabel('Chiplet ID')
    ax0.set_ylabel('Total data touched (MB)')
    ax0.set_xlim(-0.7, n_chiplets - 0.3)
    ax0.set_xticks(range(0, n_chiplets, max(1, n_chiplets // 12)))
    ax0.axhline(vals.mean(), color='#D55E00', lw=0.8, ls='--',
                label=f'mean {vals.mean():.1f}')
    ax0.legend()

    im = ax1.imshow(grid, origin='lower', cmap='magma', aspect='equal')
    ax1.set_title('mesh layout')
    ax1.set_xlabel('x')
    ax1.set_ylabel('y')
    ax1.set_xticks(range(xdim))
    ax1.set_yticks(range(ydim))
    ax1.grid(False)
    fig.colorbar(im, ax=ax1, fraction=0.046, pad=0.04).set_label('MB')

    fig.suptitle(f'{bench}: total data touched per chiplet over full run', y=1.03)
    save_fig(fig, out_dir, f'{bench}_total_touched_out')


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument('run_id', nargs='?', default='mesh-6x6/latest')
    ap.add_argument('--bins', type=int, default=DEFAULT_BINS,
                    help=f'number of equal time windows (default {DEFAULT_BINS})')
    ap.add_argument('-j', '--workers', type=int, default=2,
                    help='benchmark-parallel workers (trace loads are GB, keep low)')
    args = ap.parse_args()

    trace_dir = str(memory_trace_dir(args.run_id))
    config = run_config(args.run_id)
    suffix = file_suffix_for_config(config)
    xdim, n_chiplets = mesh_dims_from_config(config)
    tag = run_tag(args.run_id)
    nbins = max(2, args.bins)

    out_dir = OUT_ROOT / "memory-trace" / "chiplet-access-over-time" / f"{config}-{tag}"
    out_dir.mkdir(parents=True, exist_ok=True)

    print(f"trace_dir = {trace_dir}")
    print(f"out_dir   = {out_dir}")

    if args.workers <= 1:
        raw = [process_benchmark(b, trace_dir, suffix, n_chiplets, nbins)
               for b in benchmarks]
    else:
        with ProcessPoolExecutor(max_workers=args.workers) as ex:
            raw = list(ex.map(process_benchmark, benchmarks,
                              repeat(trace_dir), repeat(suffix),
                              repeat(n_chiplets), repeat(nbins)))

    long_rows = []
    total_rows = []
    made = 0
    for r in raw:
        if r is None:
            continue
        bench, mat, t_min, t_max = r
        made += 1

        bench_dir = out_dir / bench
        bench_dir.mkdir(parents=True, exist_ok=True)

        edges_ms = np.linspace(0.0, (t_max - t_min) * 1e3, nbins + 1)
        wide = pd.DataFrame(mat, index=[f"c{i}" for i in range(n_chiplets)],
                            columns=[f"w{j}" for j in range(nbins)])
        wide.index.name = 'chiplet'
        wide.to_csv(bench_dir / f"{bench}_out.csv")

        for c in range(n_chiplets):
            total_rows.append({'benchmark': bench, 'chiplet': c,
                               'total_touched_mb': round(float(mat[c].sum()), 6)})
            for j in range(nbins):
                long_rows.append({
                    'benchmark': bench, 'chiplet': c, 'window': j,
                    't_start_ms': edges_ms[j], 't_end_ms': edges_ms[j + 1],
                    'data_mb': mat[c, j],
                })

        plot_bench(bench, mat, t_min, t_max, n_chiplets, nbins, bench_dir)
        plot_total_touched(bench, mat, xdim, n_chiplets, bench_dir)
        print(f"[{bench}] -> {bench_dir}", flush=True)

    if not made:
        print("no usable benchmarks")
        return

    pd.DataFrame(long_rows).to_csv(out_dir / "all_benchmarks_long_out.csv",
                                   index=False)
    pd.DataFrame(total_rows).to_csv(out_dir / "per_chiplet_total_touched_out.csv",
                                    index=False)
    print(f"\nwrote {made} benchmarks to {out_dir}")


if __name__ == "__main__":
    main()
