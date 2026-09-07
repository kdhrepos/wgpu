"""
Per-PC hotness over time, mirroring the runtime pctable.go decay scheme —
`.claude/worktrees/decoupled-cluster-size` version (not master, not the older
local-home-forced-lookup/93fdc01 state this file used to mirror).

That worktree changed classify() in two ways this file now follows:
  - No footprint clamp on the Hotness label. master demotes Warm/Hot/Global
    toward Cold when the windowed working set doesn't fit L2 (clampByFootprint);
    decoupled-cluster-size removed that entirely — capacity pressure instead
    grows a PC's separate cluster-size ladder (recomputeClusterSizes), which
    never touches Hotness. Tier is final once thresholdClassify (+ the
    NonShared hop-promotion) runs.
  - No Streaming pre-check. Streaming classification was removed from
    pctable.go's online classifier on all current branches (see memory
    project_streaming_classification_removed) — this file used to mirror an
    older commit that still had it (reuse < 1.5 -> Streaming); that branch is
    gone here too.

Scheme (see reference_pctable_hotness_algorithm memory):
  - reuse   = cum ReqCount / cum unique   (LIFETIME, never reset/decayed)
  - sharing = cum tuples   / cum unique   (LIFETIME)
  - avg_hop = cum HopCount / cum ReqCount  (LIFETIME)
  - ws_unique = distinct cachelines accessed in the trailing `--ws-window`
      windows -> the offline analog of WSAddrs (HLL DecayRanks every
      wsDecaySampleInterval=7 ticks halves the estimate). Computed and kept in
      the output for reference, but classify() no longer consumes it (no
      footprint clamp) — informational only now.
  - reclassify once per window (= samplingPeriod, --window-ns, 10k cycle default)
      from the accumulated counters -> CachedHotness. Same as the engine's
      Decay() reclassify hook.

Population = the rows pctable.ReqFromOutside() observes: L2-miss RDMA requests
(rdma == 1, type in {2,5}, pc != 0). Metrics are averaged over the home chiplets
(owner) like pc_stats_l2miss_per_owner.py, then classify() is run once per
(window, pc). classify() (threshold mode) is ported 1:1 from that worktree's
pctable.go.

Result: x = time window, y = hotness tier, one colored step-line per PC
(legend = PC decimal value).

Output under ~/mgvm/analyze/_out/pc-stats/{date}/pc_hotness_lifetime_w{window}_ws{ws}/
(per-script, per-(window,ws) dir; CSV and figure in split subfolders):
  - fig/{bench}_pc_hotness_per_time.png
  - csv/{bench}_pc_hotness_per_time.csv : long form
        window_start,window_end,pc,reuse,sharing,ws_unique,reqcount,avg_hop,
        hotness,hotness_level
"""

import os
import sys
import argparse
import numpy as np
import pandas as pd
from collections import defaultdict

parent_dir = os.path.abspath(os.path.join(os.path.dirname(__file__), "../../"))
if parent_dir not in sys.path:
    sys.path.append(parent_dir)
scripts_dir = os.path.abspath(os.path.join(os.path.dirname(__file__), "../../../scripts"))
if scripts_dir not in sys.path:
    sys.path.append(scripts_dir)

import matplotlib
matplotlib.use("Agg")
import matplotlib.pyplot as plt
from matplotlib.lines import Line2D

from concurrent.futures import ProcessPoolExecutor
from itertools import repeat
from params import benchmarks
from _paths import run_root as _run_root, memory_trace_dir as _mtd, run_tag as _run_tag
from plot_style import apply_paper_style, color_for, save_fig, SIZE_DOUBLE

username = os.path.expanduser('~')
_run_id = next((a for a in sys.argv[1:] if not a.startswith('-')), 'latest')

NS = 1e-9
USE_COLS = ['time', 'addr', 'pc', 'owner', 'rdma', 'type', 'chiplet']
READ_DTYPES = {'addr': 'uint64', 'pc': 'uint64', 'owner': 'int16',
               'chiplet': 'int16', 'rdma': 'int8', 'type': 'int8'}
CHUNK = 6_000_000

# ── pctable.go constants (mirror mgpusim/rdma/pctable.go) ────────────────────
REUSE_COLD = 2
REUSE_WARM = 4
REUSE_HOT = 15
REUSE_GLOBAL = 30
SHARE_MULTI = 2
SHARE_GLOBAL = 10
NONSHARED_HOP_PROMOTE = 0.3

# ── protocol.Hotness enum values (protocol/distprotocol.go) ──────────────────
# STREAMING kept only for enum-value parity with the Go side; classify() below
# never produces it (see module docstring).
STREAMING, COLD, WARM, HOT, GLOBAL, WARM_NS, HOT_NS = -2, 0, 1, 2, 3, 4, 5

HOTNESS_NAME = {
    COLD: "Cold", WARM_NS: "Warm_NS", HOT_NS: "Hot_NS",
    WARM: "Warm", HOT: "Hot", GLOBAL: "Global",
}
# y-axis level in semantic hotness order (hotnessRank).
HOTNESS_LEVEL = {
    COLD: 0, WARM_NS: 1, HOT_NS: 2, WARM: 3, HOT: 4, GLOBAL: 5,
}
LEVEL_ORDER = [COLD, WARM_NS, HOT_NS, WARM, HOT, GLOBAL]


# ── classify() port (threshold mode) ────────────────────────────────────────
def _hop_norm(avg_hop, hop_ref):
    if hop_ref <= 1:
        return 0.0
    hn = (avg_hop - 1.0) / (hop_ref - 1.0)
    return min(1.0, max(0.0, hn))


def _threshold_classify(reuse, sharing):
    if reuse < REUSE_COLD:
        return COLD
    if sharing < SHARE_MULTI:                       # NonShared track
        if reuse < REUSE_WARM:
            return COLD
        if reuse < REUSE_HOT:
            return WARM_NS
        return HOT_NS
    if reuse < REUSE_WARM:                           # Shared track
        return COLD
    if reuse >= REUSE_GLOBAL and sharing >= SHARE_GLOBAL:
        return GLOBAL
    if reuse >= REUSE_HOT:
        return HOT
    return WARM


def classify(reuse, sharing, ws_unique, reqcount, avg_hop, xDim, num, hop_ref, min_req):
    """decoupled-cluster-size worktree's PCTable.classify port (threshold mode).
    `ws_unique, xDim, num` kept in the signature for caller compatibility
    (ws_unique is still computed and reported by the caller) but unused here —
    that worktree moved footprint pressure out of Hotness entirely (see module
    docstring). reuse/sharing/hop are lifetime, exactly like the engine."""
    if reqcount == 0:
        return COLD
    if reqcount < min_req:
        return COLD
    h = _threshold_classify(reuse, sharing)
    if h in (WARM_NS, HOT_NS) and _hop_norm(avg_hop, hop_ref) >= NONSHARED_HOP_PROMOTE:
        return {WARM_NS: WARM, HOT_NS: HOT}[h]
    return h


# ── trace load (streamed, memory-safe for multi-GB traces) ───────────────────
def _load(trace_file, xDim, window_ns, t0=None):
    """Return filtered rows with win + hop, streamed in chunks. t0 defaults to
    this filtered stream's own first-seen time (original behavior); pass an
    explicit t0 to align window indices with a different event stream's
    origin (e.g. sliding_wss_hotness.py aligning to the full-demand min time,
    which precedes the first RDMA-miss event)."""
    parts = []
    reader = pd.read_csv(trace_file, usecols=USE_COLS, dtype=READ_DTYPES,
                         skipinitialspace=True, chunksize=CHUNK, engine='c')
    for chunk in reader:
        chunk = chunk[(chunk['rdma'] == 1) & (chunk['type'].isin([2, 5]))
                      & (chunk['pc'] != 0)]
        if chunk.empty:
            continue
        if t0 is None:
            t0 = chunk['time'].iloc[0]
        win = ((chunk['time'].to_numpy() - t0) / NS // window_ns).astype('int32')
        sx = (chunk['chiplet'].to_numpy() % xDim)
        sy = (chunk['chiplet'].to_numpy() // xDim)
        dx = (chunk['owner'].to_numpy() % xDim)
        dy = (chunk['owner'].to_numpy() // xDim)
        hop = np.abs(sx - dx) + np.abs(sy - dy)
        parts.append(pd.DataFrame({
            'win': win, 'pc': chunk['pc'].to_numpy(), 'owner': chunk['owner'].to_numpy(),
            'chiplet': chunk['chiplet'].to_numpy(), 'addr': chunk['addr'].to_numpy(),
            'hop': hop.astype('int16'),
        }))
    if not parts:
        return None
    return pd.concat(parts, ignore_index=True)


# ── per (pc,owner) lifetime cumulative series ────────────────────────────────
def _owner_cumulative(df):
    """Per (pc,owner,win) window-local req/hop/new-unique/new-tuple, then
    cumulative over each (pc,owner)'s active windows -> lifetime counters."""
    base = df.groupby(['pc', 'owner', 'win']).agg(
        req=('addr', 'size'), hop=('hop', 'sum')).reset_index()

    fu = df.drop_duplicates(['pc', 'owner', 'addr'])            # first-seen addr
    nu = fu.groupby(['pc', 'owner', 'win']).size().rename('new_u').reset_index()
    ft = df.drop_duplicates(['pc', 'owner', 'addr', 'chiplet'])  # first-seen tuple
    nt = ft.groupby(['pc', 'owner', 'win']).size().rename('new_t').reset_index()

    base = (base.merge(nu, on=['pc', 'owner', 'win'], how='left')
                .merge(nt, on=['pc', 'owner', 'win'], how='left'))
    base[['new_u', 'new_t']] = base[['new_u', 'new_t']].fillna(0)
    base = base.sort_values(['pc', 'owner', 'win'])
    g = base.groupby(['pc', 'owner'])
    base['cum_req'] = g['req'].cumsum()
    base['cum_hop'] = g['hop'].cumsum()
    base['cum_unique'] = g['new_u'].cumsum()
    base['cum_tuple'] = g['new_t'].cumsum()
    return base


def _sliding_ws(ev_addr, ev_win, qwins, W):
    """Trailing-W-window distinct cacheline count at each query window (the
    offline analog of WSAddrs rank-decay). Two-pointer over time-sorted events;
    qwins ascending. O(events + queries)."""
    n = len(ev_win)
    cnt = defaultdict(int)
    distinct = 0
    lo = hi = 0
    out = np.empty(len(qwins), dtype='int64')
    for i, w in enumerate(qwins):
        floor = w - W + 1
        while hi < n and ev_win[hi] <= w:
            a = ev_addr[hi]
            if cnt[a] == 0:
                distinct += 1
            cnt[a] += 1
            hi += 1
        while lo < hi and ev_win[lo] < floor:
            a = ev_addr[lo]
            cnt[a] -= 1
            if cnt[a] == 0:
                distinct -= 1
            lo += 1
        out[i] = distinct
    return out


# ── assemble per (pc,window) hotness time series ─────────────────────────────
def _pc_timeseries(df, base, xDim, yDim, ws_window, min_req, hop_ref, window_ns):
    num = xDim * yDim
    # distinct (pc,owner,addr,win) access events for the sliding WS
    aw = df.drop_duplicates(['pc', 'owner', 'addr', 'win'])[['pc', 'owner', 'addr', 'win']]
    aw = aw.sort_values(['pc', 'owner', 'win'])
    ev_by_po = {k: (g['addr'].to_numpy(), g['win'].to_numpy())
                for k, g in aw.groupby(['pc', 'owner'])}

    pc_active = {pc: np.array(sorted(s.unique()))
                 for pc, s in df.groupby('pc')['win']}

    rows = []
    KEYS = ['cum_req', 'cum_hop', 'cum_unique', 'cum_tuple']
    for pc, bpc in base.groupby('pc'):
        qwins = pc_active[pc]
        cum_stacks = {k: [] for k in KEYS}
        ws_stack = []
        for owner, bo in bpc.groupby('owner'):
            s = bo.set_index('win')[KEYS].reindex(qwins).ffill()
            for k in KEYS:
                cum_stacks[k].append(s[k].to_numpy())
            ea, ew = ev_by_po[(pc, owner)]
            ws_stack.append(_sliding_ws(ea, ew, qwins, ws_window).astype('float64'))
        # macro-average across owners active so far (NaN = owner not yet seen)
        cr = np.nanmean(np.vstack(cum_stacks['cum_req']), axis=0)
        ch = np.nanmean(np.vstack(cum_stacks['cum_hop']), axis=0)
        cu = np.nanmean(np.vstack(cum_stacks['cum_unique']), axis=0)
        ct = np.nanmean(np.vstack(cum_stacks['cum_tuple']), axis=0)
        ws = np.nanmean(np.vstack(ws_stack), axis=0)
        for i, w in enumerate(qwins):
            if not (cu[i] > 0):
                continue
            reuse = cr[i] / cu[i]
            sharing = ct[i] / cu[i]
            hop = ch[i] / cr[i] if cr[i] > 0 else 0.0
            h = classify(reuse, sharing, ws[i], cr[i], hop, xDim, num, hop_ref, min_req)
            rows.append((int(w), int(pc), reuse, sharing, ws[i], cr[i], hop, h))

    out = pd.DataFrame(rows, columns=[
        'win', 'pc', 'reuse', 'sharing', 'ws_unique', 'reqcount', 'avg_hop', 'hotness'])
    out['hotness_level'] = out['hotness'].map(HOTNESS_LEVEL)
    out['window_start'] = out['win'] * window_ns
    out['window_end'] = (out['win'] + 1) * window_ns
    return out


def _plot(pc_win, benchmark, top_pcs, window_ns, fig_dir):
    apply_paper_style()
    fig, ax = plt.subplots(figsize=SIZE_DOUBLE)

    handles = []
    for i, pc in enumerate(top_pcs):
        sub = pc_win[pc_win['pc'] == pc].sort_values('window_start')
        if sub.empty:
            continue
        c = color_for(i)
        ax.plot(sub['window_start'], sub['hotness_level'],
                drawstyle='steps-post', marker='o', ms=3, lw=1.2, color=c)
        handles.append(Line2D([0], [0], color=c, marker='o', ms=3, lw=1.2,
                              label=str(int(pc))))

    ax.set_yticks([HOTNESS_LEVEL[h] for h in LEVEL_ORDER])
    ax.set_yticklabels([HOTNESS_NAME[h] for h in LEVEL_ORDER])
    ax.set_ylim(-0.4, 6.4)
    ax.set_xlabel(f"Time window start (ns, {window_ns} ns/window @ 1 GHz)")
    ax.set_ylabel("PC hotness (lifetime metrics)")
    ax.set_title(f"{benchmark}: per-PC hotness over time")
    ax.grid(True, axis='y', ls=':', alpha=0.4)
    ax.legend(handles=handles, title="PC (dec)", fontsize=6, ncol=2,
              loc='center left', bbox_to_anchor=(1.01, 0.5))
    save_fig(fig, fig_dir, f"{benchmark}_pc_hotness_per_time")


def process_benchmark(benchmark, args):
    trace_file = f"{args.trace_dir}/{benchmark}_mesh_{args.xDim}x{args.yDim}_memory_trace.csv"
    if not os.path.exists(trace_file):
        return None
    print(f"[{benchmark}] lifetime PC hotness, {args.window_ns}ns window, "
          f"ws={args.ws_window} windows...", flush=True)

    try:
        df = _load(trace_file, args.xDim, args.window_ns)
        if df is None or df.empty:
            return None

        hop_ref = max(1, (args.xDim - 1) + (args.yDim - 1))
        base = _owner_cumulative(df)
        pc_win = _pc_timeseries(df, base, args.xDim, args.yDim, args.ws_window,
                                args.min_req, hop_ref, args.window_ns)
        if pc_win.empty:
            return None

        totals = pc_win.groupby('pc')['reqcount'].max().sort_values(ascending=False)
        top_pcs = list(totals.head(args.top_n).index)

        base_dir = (f"{username}/mgvm/analyze/_out/pc-stats/{args.date}"
                    f"/pc_hotness_lifetime_w{args.window_ns}_ws{args.ws_window}")
        csv_dir, fig_dir = f"{base_dir}/csv", f"{base_dir}/fig"
        cols = ['window_start', 'window_end', 'pc', 'reuse', 'sharing', 'ws_unique',
                'reqcount', 'avg_hop', 'hotness', 'hotness_level']
        out = pc_win[cols].copy()
        out['hotness'] = out['hotness'].map(HOTNESS_NAME)
        out = out.sort_values(['window_start', 'pc'])
        os.makedirs(csv_dir, exist_ok=True)
        out.round(3).to_csv(f"{csv_dir}/{benchmark}_pc_hotness_per_time.csv", index=False)

        _plot(pc_win, benchmark, top_pcs, args.window_ns, fig_dir)

        return {"benchmark": benchmark, "windows": int(pc_win['win'].nunique()),
                "pcs": int(pc_win['pc'].nunique())}
    except Exception as e:
        import traceback
        print(f"[{benchmark}] Error: {e}\n{traceback.format_exc()}")
        return None


if __name__ == "__main__":
    parser = argparse.ArgumentParser(
        description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    parser.add_argument('run_id', nargs='?', default=_run_id,
                        help='run id (e.g. mesh-6x6/latest, 20260624-002942, latest)')
    parser.add_argument('--xDim', type=int, default=6)
    parser.add_argument('--yDim', type=int, default=6)
    parser.add_argument('--window-ns', type=int, default=10000,
                        help='window = samplingPeriod (1 ns == 1 cycle @ 1 GHz)')
    parser.add_argument('--ws-window', type=int, default=7,
                        help='trailing windows for the footprint working set '
                             '(= wsDecaySampleInterval; the windowed WSAddrs analog)')
    parser.add_argument('--top-n', type=int, default=8,
                        help='number of hottest PCs to draw (by lifetime access)')
    parser.add_argument('--min-req', type=int, default=100,
                        help='min per-owner lifetime requests for non-Cold (pctable default 100)')
    parser.add_argument('--workers', type=int,
                        default=int(os.environ.get("PC_HOTNESS_WORKERS", "4")),
                        help='parallel benchmarks (keep low: multi-GB traces load in RAM)')
    parser.add_argument('--benchmarks', default=None,
                        help='comma-separated subset (default: all in params.benchmarks)')
    args = parser.parse_args()

    args.date = _run_tag(args.run_id)
    args.trace_dir = str(_mtd(args.run_id))
    print(f"Target Directory: {args.trace_dir}")

    bset = benchmarks if args.benchmarks is None else args.benchmarks.split(',')

    summaries = []
    with ProcessPoolExecutor(max_workers=args.workers) as ex:
        for r in ex.map(process_benchmark, bset, repeat(args)):
            if r is not None:
                summaries.append(r)

    if not summaries:
        print(f"No trace files found under {args.trace_dir}; aborting.")
        sys.exit(1)

    print("\n--- Summary ---")
    print(pd.DataFrame(summaries).sort_values("benchmark").to_string(index=False))
    _base = (f"{username}/mgvm/analyze/_out/pc-stats/{args.date}"
             f"/pc_hotness_lifetime_w{args.window_ns}_ws{args.ws_window}")
    print(f"\nCSVs    -> {_base}/csv")
    print(f"Figures -> {_base}/fig")
