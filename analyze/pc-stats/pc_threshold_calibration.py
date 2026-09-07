#!/usr/bin/env python3
"""Calibrate the windowed RDMA PC-table classifier from memory traces.

The dynamic-clustering RDMA PC table classifies each PC by reuse and
sharing. After the switch to a fully-windowed classifier, every signal
(unique addr = WSAddrs, tuple = UniqueAddrChip, ReqCount, HopCount)
decays on a fixed window, so the reuse/sharing magnitudes differ from the
lifetime numbers the thresholds were originally tuned on
(reuseCold=2, reuseWarm=4, reuseHot=15, reuseGlobal=30; sharingMulti=2,
sharingGlobal=10; minReqCount=100).

A memory trace row is exactly what the PC table sees on ReqFromOutside:
  owner   = home chiplet that hosts the PC-table entry
  chiplet = requestor chiplet
  pc      = instruction PC
  addr    = byte address (cacheline = addr >> 6)
  rdma==1 = cross-chiplet request (owner != chiplet), i.e. an RDMA request
So per (owner, pc):
  reuse   = requests / distinct cachelines
  sharing = distinct (cacheline, requestor) / distinct cachelines

This tool reconstructs both the lifetime and the windowed distributions of
reuse/sharing/req-count, shows where the current thresholds land, and
suggests windowed thresholds that preserve the request-weighted tier
populations. It reads existing traces only; it never runs a simulation.

Usage (via the analyze/run.sh wrapper, which sets PYTHONPATH):
  analyze/run.sh pc-stats/pc_threshold_calibration.py mesh-6x6/<ts>
  analyze/run.sh pc-stats/pc_threshold_calibration.py mesh-6x6   # newest run
Direct:
  pc_threshold_calibration.py --trace PATH.csv [--window-cycles 43000 ...]
  pc_threshold_calibration.py --trace-dir DIR [--platform mesh --dims 6x6]
"""

import argparse
import glob
import os
import re
import sys

import numpy as np
import pandas as pd

# Make the analyze-tree helpers importable whether launched via analyze/run.sh
# (which flattens every domain dir onto PYTHONPATH) or run directly.
_ANALYZE_ROOT = os.path.abspath(os.path.join(os.path.dirname(__file__), ".."))
for _p in (os.path.join(_ANALYZE_ROOT, "common"),
           os.path.abspath(os.path.join(_ANALYZE_ROOT, "..", "scripts"))):
    if _p not in sys.path:
        sys.path.insert(0, _p)

try:  # run-id -> results-tree path resolution
    from _paths import memory_trace_dir, run_config, run_tag
except Exception:
    memory_trace_dir = run_config = run_tag = None

# Current classifier thresholds (pctable.go) -- reuse axis cut points that
# actually separate tiers, plus the sharing cut points.
REUSE_CUTS = [2, 4, 15, 30]
SHARING_CUTS = [2, 10]
MIN_REQ_COUNT = 100

# samplingPeriod (cycles) * wsDecaySampleInterval default -> one decay step;
# the EMA time constant is ~interval*period/ln2. Default window ~= tau.
DEFAULT_WINDOW_CYCLES = 43000

# Steady-state ReqCount under per-window halving = sum reqw*(1/2)^k = 2*reqw,
# while the windowed unique estimate tracks ~one window. So the classifier's
# effective reuse ~= EMA_FACTOR * (per-window reuse). Sharing is unaffected
# (numerator and denominator are both windowed sketches decayed equally).
EMA_FACTOR = 2.0

DATA_TYPES = [0, 1, 2, 3, 4, 5]  # directory-stage data accesses (exclude evict 6-9)


def _read_csv_fast(path, want, dtypes, max_rows):
    """Read the projected columns. Uses pyarrow's CSV parser (single-thread,
    ~2x pandas' C engine) when reading the whole file; falls back to pandas
    for the --max-rows early-stop path or if pyarrow is unavailable.
    Set use_threads=True below for ~12x on these traces if you accept a
    multithreaded read."""
    if max_rows is None:
        try:
            import pyarrow as pa
            from pyarrow import csv as pacsv
        except ImportError:
            pa = None
        if pa is not None:
            _pa = {np.uint64: pa.uint64(), np.int16: pa.int16(), np.int8: pa.int8()}
            ctypes = {c: _pa[dtypes[c]] for c in dtypes if c in want}
            tbl = pacsv.read_csv(
                path,
                read_options=pacsv.ReadOptions(use_threads=False),
                convert_options=pacsv.ConvertOptions(
                    include_columns=want, column_types=ctypes),
            )
            return tbl.to_pandas()
    return pd.read_csv(path, skipinitialspace=True, usecols=want,
                       dtype=dtypes, nrows=max_rows)


def load_trace(path, max_rows=None):
    """Load a memory trace and return the cross-chiplet data-access stream.

    Handles both the 9-column (time..chiplet) and 11-column
    (..data_id,is_ptw) schemas. Returns a frame with columns
    [owner, pc, chiplet, cacheline, cycles].
    """
    header = pd.read_csv(path, nrows=0, skipinitialspace=True).columns.tolist()
    want = ["time", "addr", "pc", "owner", "rdma", "type", "chiplet"]
    if "is_ptw" in header:
        want.append("is_ptw")
    dtypes = {
        "addr": np.uint64, "pc": np.uint64, "owner": np.int16,
        "chiplet": np.int16, "rdma": np.int8, "type": np.int8,
    }
    if "is_ptw" in header:
        dtypes["is_ptw"] = np.int8
    df = _read_csv_fast(path, want, dtypes, max_rows)

    # Filter first, then dedup: the predicate uses only dedup-key columns, so
    # filter and drop_duplicates commute; deduping the small cross-chiplet set
    # (~half the rows) is ~2x cheaper than deduping the full frame.
    df = df[(df["rdma"] == 1) & (df["pc"] != 0) & (df["type"].isin(DATA_TYPES))]
    if "is_ptw" in df.columns:
        df = df[df["is_ptw"] == 0]

    df = df.drop_duplicates().copy()
    df["cacheline"] = (df["addr"].to_numpy() >> np.uint64(6))
    # (cacheline, requestor) packed into one key for cheap tuple-nunique.
    df["cl_chip"] = (df["cacheline"] << np.uint64(6)) | df["chiplet"].astype(np.uint64)
    df["cycles"] = df["time"].to_numpy() * 1e9  # seconds -> cycles @ 1 GHz
    return df[["owner", "pc", "chiplet", "cacheline", "cl_chip", "cycles"]]


def lifetime_metrics(df):
    """Per (owner, pc): req, uniq, tuples, reuse, sharing over the whole trace."""
    g = df.groupby(["owner", "pc"], sort=False)
    out = g.agg(
        req=("cacheline", "size"),
        uniq=("cacheline", "nunique"),
        tuples=("cl_chip", "nunique"),
    ).reset_index()
    out["reuse"] = out["req"] / out["uniq"]
    out["sharing"] = out["tuples"] / out["uniq"]
    return out


def windowed_metrics(df, window_cycles):
    """Per (owner, pc): representative windowed reuse/sharing/req.

    Computes per (owner, pc, window) reuse/sharing, then reduces over
    windows to a per-entry representative (median of the active windows).
    'effective reuse' scales the per-window reuse by EMA_FACTOR to match the
    classifier's steady-state ReqCount accumulation.
    """
    w = df.copy()
    w["win"] = (w["cycles"] // window_cycles).astype(np.int64)
    g = w.groupby(["owner", "pc", "win"], sort=False)
    per = g.agg(
        reqw=("cacheline", "size"),
        uniqw=("cacheline", "nunique"),
        tuplesw=("cl_chip", "nunique"),
    ).reset_index()
    per["reusew"] = per["reqw"] / per["uniqw"]
    per["sharingw"] = per["tuplesw"] / per["uniqw"]

    rep = per.groupby(["owner", "pc"], sort=False).agg(
        reuse_win=("reusew", "median"),
        sharing_win=("sharingw", "median"),
        reqw_med=("reqw", "median"),
        reqw_p90=("reqw", lambda s: np.percentile(s, 90)),
        n_windows=("win", "nunique"),
    ).reset_index()
    rep["reuse_eff"] = rep["reuse_win"] * EMA_FACTOR
    rep["reqcount_eff"] = rep["reqw_med"] * EMA_FACTOR
    return rep


def weighted_quantile(values, q, weights):
    """Weighted quantile (q in [0,1])."""
    values = np.asarray(values, dtype=float)
    weights = np.asarray(weights, dtype=float)
    order = np.argsort(values)
    values, weights = values[order], weights[order]
    cw = np.cumsum(weights)
    if cw[-1] <= 0:
        return float("nan")
    cw = cw / cw[-1]
    return float(np.interp(q, cw, values))


def weighted_frac_below(values, cut, weights):
    values = np.asarray(values, dtype=float)
    weights = np.asarray(weights, dtype=float)
    tot = weights.sum()
    return float(weights[values < cut].sum() / tot) if tot > 0 else float("nan")


def scale_cuts(life_vals, win_vals, cur_cuts, weights):
    """Scale current cut points by the request-weighted median shift
    (windowed / lifetime). Preserves the relative spacing of the tiers
    (the design intent) while matching the new windowed magnitude. Robust
    to skewed distributions where a population-preserving map would collapse
    all cuts onto one quantile.

    Returns (scaled_cuts, scale).
    """
    life_med = weighted_quantile(life_vals, 0.5, weights)
    win_med = weighted_quantile(win_vals, 0.5, weights)
    scale = win_med / life_med if life_med > 0 else float("nan")
    return [c * scale for c in cur_cuts], scale


def pctiles(s, ps=(50, 90, 99)):
    return {f"p{p}": float(np.percentile(s, p)) for p in ps} if len(s) else {}


def analyze_one(path, windows, out_dir, make_plot=True, max_rows=None):
    name = os.path.basename(path).replace("_memory_trace.csv", "")
    out = []  # buffered so parallel workers don't interleave stdout
    df = load_trace(path, max_rows=max_rows)
    if df.empty:
        return None, None, [f"  {name}: no cross-chiplet data rows, skip"]

    life = lifetime_metrics(df)
    n_pc = life["pc"].nunique()
    n_owner = life["owner"].nunique()
    pc_per_owner = life.groupby("owner")["pc"].nunique()
    over_cap = int((pc_per_owner > 10).sum())

    primary_w = windows[0]
    win = windowed_metrics(df, primary_w)
    merged = life.merge(win, on=["owner", "pc"], how="inner")
    wgt = merged["req"].to_numpy()  # request-weighted calibration

    reuse_cut_win, reuse_scale = scale_cuts(
        merged["reuse"].to_numpy(), merged["reuse_eff"].to_numpy(), REUSE_CUTS, wgt)
    sharing_cut_win, sharing_scale = scale_cuts(
        merged["sharing"].to_numpy(), merged["sharing_win"].to_numpy(), SHARING_CUTS, wgt)
    # minReqCount: keep the same request-weighted population below the gate.
    minreq_win = weighted_quantile(
        merged["reqcount_eff"].to_numpy(),
        weighted_frac_below(merged["req"].to_numpy(), MIN_REQ_COUNT, wgt),
        wgt)

    out.append(f"  {name}: {len(df):,} rows | PCs={n_pc} owners={n_owner} "
               f"pc/owner={int(pc_per_owner.median())} (>{10}-cap owners: {over_cap})")
    out.append(f"    lifetime reuse   {pctiles(life['reuse'])}")
    out.append(f"    windowed reuse   {pctiles(merged['reuse_eff'])} (eff, W={primary_w})")
    out.append(f"    lifetime sharing {pctiles(life['sharing'])}")
    out.append(f"    windowed sharing {pctiles(merged['sharing_win'])}")
    out.append(f"    -> suggested reuse cuts {[round(x,2) for x in reuse_cut_win]} "
               f"(was {REUSE_CUTS}, scale x{reuse_scale:.2f})")
    out.append(f"    -> suggested sharing cuts {[round(x,2) for x in sharing_cut_win]} "
               f"(was {SHARING_CUTS}, scale x{sharing_scale:.2f})")
    out.append(f"    -> suggested minReqCount {round(minreq_win,1)} (was {MIN_REQ_COUNT})")

    # window sensitivity
    sens = []
    for W in windows:
        # primary window already computed above -- don't redo the groupby
        m = merged if W == primary_w else life.merge(
            windowed_metrics(df, W), on=["owner", "pc"], how="inner")
        sens.append({
            "window_cycles": W,
            "reuse_eff_p50": float(np.percentile(m["reuse_eff"], 50)),
            "reuse_eff_p90": float(np.percentile(m["reuse_eff"], 90)),
            "sharing_win_p50": float(np.percentile(m["sharing_win"], 50)),
            "sharing_win_p90": float(np.percentile(m["sharing_win"], 90)),
        })

    os.makedirs(out_dir, exist_ok=True)
    if make_plot:
        out.append(f"    plot -> "
                   f"{_plot(name, merged, reuse_cut_win, sharing_cut_win, out_dir)}")

    row = {
        "benchmark": name, "n_pc": n_pc, "n_owner": n_owner,
        "pc_per_owner_med": int(pc_per_owner.median()), "owners_over_cap": over_cap,
        "window_cycles": primary_w,
    }
    for k, v in pctiles(life["reuse"]).items():
        row[f"life_reuse_{k}"] = v
    for k, v in pctiles(merged["reuse_eff"]).items():
        row[f"win_reuse_{k}"] = v
    for k, v in pctiles(life["sharing"]).items():
        row[f"life_sharing_{k}"] = v
    for k, v in pctiles(merged["sharing_win"]).items():
        row[f"win_sharing_{k}"] = v
    row["reuse_scale"] = round(reuse_scale, 3)
    row["sharing_scale"] = round(sharing_scale, 3)
    for i, c in enumerate(reuse_cut_win):
        row[f"sugg_reuse_cut_{REUSE_CUTS[i]}"] = round(c, 3)
    for i, c in enumerate(sharing_cut_win):
        row[f"sugg_sharing_cut_{SHARING_CUTS[i]}"] = round(c, 3)
    row["sugg_min_req_count"] = round(minreq_win, 1)
    return row, sens, out


def _plot(name, merged, reuse_cuts, sharing_cuts, out_dir):
    import matplotlib
    matplotlib.use("Agg")
    import matplotlib.pyplot as plt

    fig, ax = plt.subplots(1, 2, figsize=(12, 4.5))
    for a, col, cur, sug, title in [
        (ax[0], "reuse_eff", REUSE_CUTS, reuse_cuts, "windowed reuse (eff)"),
        (ax[1], "sharing_win", SHARING_CUTS, sharing_cuts, "windowed sharing"),
    ]:
        vals = merged[col].replace([np.inf, -np.inf], np.nan).dropna()
        vals = vals[vals > 0]
        if vals.empty:
            continue
        bins = np.logspace(np.log10(max(vals.min(), 1e-3)),
                           np.log10(vals.max() + 1e-9), 40)
        a.hist(vals, bins=bins, weights=merged.loc[vals.index, "req"],
               color="#5B8FF9", alpha=0.75)
        a.set_xscale("log")
        for c in cur:
            a.axvline(c, color="#E86452", ls="--", lw=1, label="current" if c == cur[0] else None)
        for c in sug:
            a.axvline(c, color="#3BA272", ls=":", lw=1.5, label="suggested" if c == sug[0] else None)
        a.set_title(f"{name}: {title}")
        a.set_xlabel(col)
        a.set_ylabel("request-weighted count")
        a.legend(fontsize=8)
    fig.tight_layout()
    p = os.path.join(out_dir, f"{name}_threshold_calibration.png")
    fig.savefig(p, dpi=120)
    plt.close(fig)
    return p


def _platform_dims(config):
    """Extract ('mesh', '6x6') from a config name like 'mesh-6x6[-oracle]'."""
    m = re.search(r"(\d+)x(\d+)", config or "")
    if not m:
        return None, None
    return config[: m.start()].rstrip("-"), f"{m.group(1)}x{m.group(2)}"


def resolve_traces(args):
    """Return (trace_files, run_tag). Precedence: --trace > --trace-dir >
    positional run_id (resolved to the run's memory-traces dir via _paths)."""
    if args.trace:
        return [args.trace], None

    trace_dir, tag = args.trace_dir, None
    platform, dims = args.platform, args.dims
    if trace_dir is None and args.run_id:
        if memory_trace_dir is None:
            raise SystemExit("run_id given but _paths helper is not importable; "
                             "launch via analyze/run.sh or pass --trace-dir")
        trace_dir = str(memory_trace_dir(args.run_id))
        tag = run_tag(args.run_id)
        p, d = _platform_dims(run_config(args.run_id))
        platform, dims = platform or p, dims or d
    if trace_dir is None:
        return [], None

    platform, dims = platform or "mesh", dims or "6x6"
    files = sorted(glob.glob(
        os.path.join(trace_dir, f"*_{platform}_{dims}_memory_trace.csv")))
    if not files:  # fall back to any memory trace in the dir
        files = sorted(glob.glob(os.path.join(trace_dir, "*_memory_trace.csv")))
    return files, tag


def main():
    ap = argparse.ArgumentParser(description=__doc__,
                                 formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("run_id", nargs="?",
                    help="results run id, e.g. 'mesh-6x6/<ts>' or 'mesh-6x6' "
                         "(newest) — resolved via _paths when run through run.sh")
    ap.add_argument("--trace", help="single memory_trace.csv (overrides run_id)")
    ap.add_argument("--trace-dir", help="dir of *_memory_trace.csv (overrides run_id)")
    ap.add_argument("--platform", default=None)
    ap.add_argument("--dims", default=None)
    ap.add_argument("--window-cycles", type=int, nargs="+",
                    default=[DEFAULT_WINDOW_CYCLES],
                    help="decay window(s) in cycles; first is primary")
    ap.add_argument("--max-rows", type=int, default=None,
                    help="cap rows per trace (for quick testing on huge traces)")
    ap.add_argument("--no-plot", action="store_true")
    ap.add_argument("--jobs", "-j", type=int, default=None,
                    help="parallel worker processes, one benchmark each "
                         "(default: min(#traces, cpu)). Each worker reads "
                         "single-threaded; processes give the parallelism. "
                         "Lower this if peak RAM is a concern -- big traces "
                         "load ~30GB each.")
    ap.add_argument("--out", default=os.path.expanduser(
        "~/mgvm/analyze/_out/pc-stats/threshold-calibration"))
    args = ap.parse_args()

    traces, tag = resolve_traces(args)
    if not traces:
        ap.error("no trace files found (pass a run_id, --trace, or --trace-dir)")

    out_dir = os.path.join(args.out, tag) if tag else args.out
    os.makedirs(out_dir, exist_ok=True)

    jobs = args.jobs or min(len(traces), os.cpu_count() or 1)
    jobs = max(1, min(jobs, len(traces)))
    plot = not args.no_plot
    print(f"analyzing {len(traces)} trace(s) with {jobs} parallel worker(s)")

    results = [None] * len(traces)  # keep trace order for a stable CSV
    if jobs > 1:
        import concurrent.futures as cf
        with cf.ProcessPoolExecutor(max_workers=jobs) as ex:
            fut = {ex.submit(analyze_one, p, args.window_cycles, out_dir,
                             plot, args.max_rows): i
                   for i, p in enumerate(traces)}
            for f in cf.as_completed(fut):
                i = fut[f]
                try:
                    results[i] = f.result()
                    print("\n".join(results[i][2]), flush=True)
                except Exception as e:  # isolate a bad file, keep the rest
                    print(f"  {os.path.basename(traces[i])}: FAILED -- {e}",
                          flush=True)
    else:
        for i, p in enumerate(traces):
            results[i] = analyze_one(p, args.window_cycles, out_dir, plot,
                                     args.max_rows)
            print("\n".join(results[i][2]), flush=True)

    rows, sens_all = [], []
    for res in results:
        if res is None or res[0] is None:  # missing/failed or empty-trace skip
            continue
        row, sens, _ = res
        rows.append(row)
        for s in sens:
            s["benchmark"] = row["benchmark"]
            sens_all.append(s)

    if rows:
        os.makedirs(out_dir, exist_ok=True)
        summ = os.path.join(out_dir, "threshold_calibration_summary.csv")
        pd.DataFrame(rows).to_csv(summ, index=False)
        print(f"\nsummary -> {summ}")
        if len(args.window_cycles) > 1:
            sp = os.path.join(out_dir, "window_sensitivity.csv")
            pd.DataFrame(sens_all).to_csv(sp, index=False)
            print(f"window sensitivity -> {sp}")


if __name__ == "__main__":
    main()
