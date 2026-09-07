"""Warp stall from memory access — breakdown + figures from report CSVs.

Consumes the long-form report CSV (where, what, value) written under
~/mgvm/results/<config>/<run>/reports/<bench>_<config>.csv and quantifies how
much the warps (wavefronts) stall on memory.

Three complementary views (each a figure):

  1. cu_state         — per-CU cycle census. Fraction of CU cycles in
                        issue_active / mem_exposed / other_stalled / idle.
                        mem_exposed = the machine is stalled with only
                        outstanding memory in flight = exposed memory latency.
                        This is THE "how much warp stall from memory" number.
  2. wf_stall_reason  — per-wavefront stall census (running excluded). Shows
                        memory (vmem+lgkm) share of all non-issuing wf-cycles.
  3. vmem_breakdown   — where one vmem round-trip's time goes
                        (cu_queue / rob_entry / mem_roundtrip / rob_hold /
                        residual), with mem_roundtrip split local vs remote.

Plus a per-chiplet exposed-memory-ratio heatmap when a single report is given.

Usage (via analyze/run.sh so imports resolve):
  analyze/run.sh warp-stall/warp_stall_breakdown.py \
      /home/.../reports/gemm_mesh_6x6.csv
  analyze/run.sh warp-stall/warp_stall_breakdown.py mesh-6x6/latest --bench gemm spmv
Direct (single file, no PYTHONPATH needed):
  python3 warp_stall_breakdown.py /path/to/gemm_mesh_6x6.csv

Output: analyze/_out/warp-stall/<figs>.{png,pdf} + summary.csv
"""

from __future__ import annotations

import argparse
import math
import os
import re
import sys
from pathlib import Path

import numpy as np
import pandas as pd
import matplotlib.pyplot as plt

# --- optional analyze-tree helpers (present when run via run.sh). Fall back to
#     self-contained defaults so the script also runs on a raw CSV path. -------
_HERE = Path(__file__).resolve().parent
sys.path.insert(0, str(_HERE.parent / "common"))
try:
    from plot_style import apply_paper_style, color_for, save_fig, SIZE_DOUBLE
    from _paths import report_dir, run_root, run_tag, OUT_ROOT
    from motivation_common import file_suffix_for_config
    _HAVE_PATHS = True
except Exception:  # standalone fallback
    _HAVE_PATHS = False
    OUT_ROOT = _HERE.parent / "_out"
    _PALETTE = ["#0072B2", "#E69F00", "#009E73", "#D55E00",
                "#CC79A7", "#56B4E9", "#F0E442", "#999999"]
    SIZE_DOUBLE = (6.9, 2.4)

    def apply_paper_style():
        plt.rcParams.update({"figure.dpi": 130, "font.size": 9,
                             "axes.grid": True, "grid.alpha": 0.3,
                             "axes.axisbelow": True})

    def color_for(i):
        return _PALETTE[i % len(_PALETTE)]

    def save_fig(fig, out_dir, name, formats=("png",)):
        out_dir = Path(out_dir); out_dir.mkdir(parents=True, exist_ok=True)
        paths = []
        for ext in formats:
            p = out_dir / f"{name}.{ext}"
            fig.savefig(p, bbox_inches="tight"); paths.append(p)
        plt.close(fig)
        return paths


CU_STATES = ["cu_issue_active", "cu_mem_exposed", "cu_other_stalled", "cu_idle"]
CU_STATE_LBL = ["issue_active (hidden)", "mem_exposed (STALL)",
                "other_stalled", "idle"]

# wf stall reasons, running excluded (running = issued, not a stall)
STALL_REASONS = ["mem_wait_vmem", "mem_wait_lgkm", "fetch_inflight",
                 "exe_unit_busy", "barrier", "inst_buffer_empty", "other"]
STALL_LBL = ["mem_wait_vmem (VMEM)", "mem_wait_lgkm (scalar/LDS)",
             "fetch_inflight", "exe_unit_busy", "barrier",
             "inst_buffer_empty", "other"]

BREAKDOWN = ["breakdown_1_cu_queue_wait", "breakdown_2_rob_entry_wait",
             "breakdown_3_mem_roundtrip_total", "breakdown_4_rob_inorder_hold",
             "breakdown_5_return_residual"]
BREAKDOWN_LBL = ["1 cu_queue_wait", "2 rob_entry_wait",
                 "3 mem_roundtrip", "4 rob_inorder_hold", "5 return_residual"]

NS = 1e9  # seconds -> ns


def load_report(csv_path) -> pd.DataFrame:
    """Long-form report CSV -> (where, what, value) frame, value numeric.

    The head 'Parameters' block has variable field count (comma-separated
    paths), so parse with engine='python' + on_bad_lines='skip', mirroring
    common/motivation_common.load_report.
    """
    df = pd.read_csv(csv_path, header=0, names=["where", "what", "value"],
                     skipinitialspace=True, engine="python",
                     on_bad_lines="skip",
                     dtype={"where": str, "what": str, "value": str})
    df["where"] = df["where"].astype(str).str.strip()
    df["what"] = df["what"].astype(str).str.strip()
    df["value"] = pd.to_numeric(df["value"], errors="coerce")
    return df


def agg_series(df: pd.DataFrame) -> pd.Series:
    """driver.all_CUs (or GPU.all) aggregate rows as what->value."""
    a = df[df["where"].isin(["driver.all_CUs", "GPU.all"])]
    return a.set_index("what")["value"]


def cu_state_fractions(agg: pd.Series):
    vals = np.array([agg.get(s, 0.0) for s in CU_STATES], dtype=float)
    tot = vals.sum()
    return vals / tot if tot > 0 else vals, tot


def wf_stall_fractions(agg: pd.Series):
    vals = np.array([agg.get("stall_" + r, 0.0) for r in STALL_REASONS],
                    dtype=float)
    tot = vals.sum()
    frac = vals / tot if tot > 0 else vals
    mem_share = frac[0] + frac[1]  # vmem + lgkm
    return frac, tot, mem_share


def vmem_breakdown_ns(agg: pd.Series):
    vals = np.array([agg.get(b, 0.0) for b in BREAKDOWN], dtype=float) * NS
    total = agg.get("breakdown_total_vmem_latency", np.nan) * NS
    # mem_roundtrip local vs remote split (count-weighted averages)
    lc = agg.get("ds_l2_local_count", 0.0);  la = agg.get("ds_l2_local_dram_avg", 0.0)
    rc = agg.get("ds_l2_remote_count", 0.0); ra = agg.get("ds_l2_remote_network_avg", 0.0)
    remote_frac = (rc * ra) / ((lc * la) + (rc * ra)) if (lc + rc) > 0 else np.nan
    return vals, total, remote_frac


def per_chiplet_exposed(df: pd.DataFrame):
    """6x6 (square-mesh) grid of mem_exposed / total CU cycles per chiplet."""
    cu = df[df["what"].isin(CU_STATES) &
            df["where"].str.contains("chiplet_", na=False)].copy()
    chip = cu["where"].str.extract(r"chiplet_(\d+)")[0]
    cu = cu[chip.notna()]
    cu["chip"] = chip[chip.notna()].astype(int)
    if cu.empty:
        return None
    piv = cu.groupby(["chip", "what"])["value"].sum().unstack("what")
    tot = piv[CU_STATES].sum(axis=1)
    ratio = (piv["cu_mem_exposed"] / tot).reindex(range(int(piv.index.max()) + 1))
    n = len(ratio)
    dim = int(round(math.sqrt(n)))
    if dim * dim != n:
        return None  # non-square: skip heatmap (per repo convention squares only)
    return ratio.values.reshape(dim, dim)


# --------------------------------------------------------------------------- #
# figures
# --------------------------------------------------------------------------- #
def fig_stacked(labels, mat, seg_labels, ylabel, title, out, name, formats,
                as_percent=True, annotate_idx=None):
    """mat: (n_bench, n_seg) fractions or absolute. Stacked bar per bench."""
    apply_paper_style()
    n = len(labels)
    fig, ax = plt.subplots(figsize=(max(SIZE_DOUBLE[0], 1.1 * n + 1.5),
                                    SIZE_DOUBLE[1]))
    x = np.arange(n)
    bottom = np.zeros(n)
    scale = 100.0 if as_percent else 1.0
    for j, seg in enumerate(seg_labels):
        h = mat[:, j] * scale
        ax.bar(x, h, bottom=bottom, width=0.62, label=seg,
               color=color_for(j), edgecolor="white", linewidth=0.4)
        if annotate_idx == j:
            for i in range(n):
                if h[i] <= 0:
                    continue
                ax.text(x[i], bottom[i] + h[i] / 2,
                        f"{h[i]:.0f}%" if as_percent else f"{h[i]:.2g}",
                        ha="center", va="center", fontsize=8,
                        fontweight="bold", color="white")
        bottom += h
    ax.set_xlim(-0.7, n - 0.3)
    rot, ha = (40, "right") if n > 3 else (0, "center")
    ax.set_xticks(x); ax.set_xticklabels(labels, rotation=rot, ha=ha)
    ax.set_ylabel(ylabel); ax.set_title(title, fontsize=9)
    if as_percent:
        ax.set_ylim(0, 100)
    ax.legend(fontsize=6.5, ncol=1, framealpha=0.9,
              loc="upper left", bbox_to_anchor=(1.01, 1.0))
    ax.grid(axis="x", visible=False)
    return save_fig(fig, out, name, formats)


def fig_heatmap(grid, bench, out, name, formats):
    apply_paper_style()
    fig, ax = plt.subplots(figsize=(3.6, 3.0))
    im = ax.imshow(grid * 100, cmap="magma", vmin=0,
                   vmax=max(1.0, np.nanmax(grid) * 100))
    for (i, j), v in np.ndenumerate(grid):
        ax.text(j, i, f"{v*100:.0f}", ha="center", va="center",
                fontsize=6.5, color="white" if v < np.nanmax(grid) * 0.6 else "black")
    ax.set_title(f"{bench}: exposed-mem stall % per chiplet", fontsize=8.5)
    ax.set_xticks([]); ax.set_yticks([])
    cb = fig.colorbar(im, ax=ax, fraction=0.046, pad=0.04)
    cb.set_label("% CU cycles mem-exposed", fontsize=7)
    ax.grid(False)
    return save_fig(fig, out, name, formats)


# --------------------------------------------------------------------------- #
def _params_benchmarks() -> list[str]:
    """The canonical sweep benchmark list from ../scripts/params.py."""
    try:
        from params import benchmarks
    except Exception as e:
        sys.exit(f"[err] cannot import params.benchmarks: {e}")
    return list(benchmarks)


def resolve_inputs(args) -> list[tuple[str, Path]]:
    """-> list of (bench_label, csv_path).

    Each input is either a report CSV path, or a run-id (config/timestamp).
    For a run-id, iterate params.benchmarks (or --bench override) and build
    <bench>_<config-suffix>.csv, exactly like the other analyze scripts.
    """
    out = []
    for a in args.inputs:
        p = Path(a).expanduser()
        if p.is_file() and p.suffix == ".csv":
            lbl = re.sub(r"_[a-z0-9]+_\d+x\d+$", "", p.stem)
            out.append((lbl, p))
            continue
        if not _HAVE_PATHS:
            sys.exit(f"[err] not a CSV file and analyze helpers unavailable: {a}")
        rdir = report_dir(a)
        config = rdir.parent.parent.name           # e.g. 'mesh-6x6'
        suffix = file_suffix_for_config(config)     # -> 'mesh_6x6'
        benches = args.bench if args.bench else _params_benchmarks()
        for b in benches:
            f = rdir / f"{b}_{suffix}.csv"
            if f.exists():
                out.append((b, f))
            else:
                print(f"[warn] missing {f.name} under {rdir}")
    # de-dup preserving order
    seen = set(); uniq = []
    for lbl, p in out:
        if p not in seen:
            uniq.append((lbl, p)); seen.add(p)
    return uniq


def main():
    ap = argparse.ArgumentParser(description=__doc__,
                                 formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("inputs", nargs="+",
                    help="report CSV path(s), or a run-id (mesh-6x6/latest) + --bench")
    ap.add_argument("--bench", nargs="*", default=None,
                    help="benchmark names when a run-id is given")
    ap.add_argument("--out", default=None, help="output dir (default _out/warp-stall)")
    ap.add_argument("--formats", default="png",
                    help="comma-separated: png,pdf")
    args = ap.parse_args()

    formats = tuple(f.strip() for f in args.formats.split(",") if f.strip())
    if args.out:
        out = Path(args.out).expanduser()
    else:
        tag = "adhoc"
        if _HAVE_PATHS:
            try:
                tag = run_tag(args.inputs[0])
            except Exception:
                tag = "adhoc"
        out = Path(OUT_ROOT) / "warp-stall" / "warp-stall-breakdown" / tag
    out.mkdir(parents=True, exist_ok=True)

    inputs = resolve_inputs(args)
    if not inputs:
        sys.exit("[err] no report CSVs resolved")

    labels, cu_mat, wf_mat, bd_mat = [], [], [], []
    summary_rows = []
    single_grid = None

    for lbl, path in inputs:
        df = load_report(path)
        agg = agg_series(df)
        if agg.empty:
            print(f"[warn] {path.name}: no driver.all_CUs aggregate; "
                  "was -report-all set? skipping.")
            continue
        cu_frac, cu_tot = cu_state_fractions(agg)
        wf_frac, wf_tot, mem_share = wf_stall_fractions(agg)
        bd_ns, bd_total, remote_frac = vmem_breakdown_ns(agg)

        labels.append(lbl)
        cu_mat.append(cu_frac)
        wf_mat.append(wf_frac)
        bd_mat.append(bd_ns)

        summary_rows.append(dict(
            bench=lbl,
            exposed_mem_ratio=cu_frac[1],
            latency_hiding_ratio=agg.get("latency_hiding_ratio", np.nan),
            cu_occupancy=agg.get("cu_occupancy", np.nan),
            wf_mem_stall_share=mem_share,
            vmem_avg_latency_ns=agg.get("vmem_avg_latency", np.nan) * NS,
            mem_roundtrip_ns=bd_ns[2],
            roundtrip_remote_frac=remote_frac,
        ))
        print(f"{lbl:>16}: exposed-mem {cu_frac[1]*100:5.1f}% of CU cycles | "
              f"lat-hiding {agg.get('latency_hiding_ratio', float('nan')):.2f} | "
              f"wf mem-stall share {mem_share*100:4.1f}% | "
              f"vmem {agg.get('vmem_avg_latency', 0)*NS:6.0f}ns "
              f"(remote {remote_frac*100:.0f}%)")

        if len(inputs) == 1:
            single_grid = per_chiplet_exposed(df)

    if not labels:
        sys.exit("[err] no usable reports")

    cu_mat = np.array(cu_mat); wf_mat = np.array(wf_mat); bd_mat = np.array(bd_mat)

    written = []
    written += fig_stacked(
        labels, cu_mat, CU_STATE_LBL,
        "CU cycles (%)", "CU state census (memory-exposed stall)",
        out, "cu_state_census", formats, as_percent=True, annotate_idx=1)
    written += fig_stacked(
        labels, wf_mat, STALL_LBL,
        "wavefront stall cycles (%)",
        "Wavefront stall-reason census (running excluded)",
        out, "wf_stall_reason", formats, as_percent=True)
    written += fig_stacked(
        labels, bd_mat, BREAKDOWN_LBL,
        "vmem latency (ns)", "Vector-memory latency breakdown",
        out, "vmem_latency_breakdown", formats, as_percent=False)
    if single_grid is not None:
        written += fig_heatmap(single_grid, labels[0], out,
                               "exposed_mem_per_chiplet", formats)

    summ = pd.DataFrame(summary_rows)
    summ_path = out / "summary.csv"
    summ.to_csv(summ_path, index=False)
    written.append(summ_path)

    print("\nWrote:")
    for p in written:
        print("  ", p)


if __name__ == "__main__":
    main()
