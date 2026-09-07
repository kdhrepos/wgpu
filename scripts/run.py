import os
import re
import sys
import time
import signal
import argparse
import subprocess
from dataclasses import dataclass
from typing import Optional, List, Dict

from params import benchmarks, benchmark_configs
from manifest import (
    Manifest, BenchmarkEntry, manifest_path_in, slurm_backlink_path,
)

# 기본 옵션 설정
base_options = (
    ' -timing '
    '-no-progress-bar '
    '-report-all '
    '-mem-allocator-type=lasp '
    '-use-lasp-mem-alloc '
    '-scheduling=lasp '
)

username = os.path.expanduser('~')

# Tree root = parent of this scripts/ dir. When a worktree's run.sh invokes
# this worktree's run.py, the sample binaries it launches must come from the
# SAME tree that compile.py just built (this worktree) — not the shared
# ~/mgvm checkout. Results go to ~/mgvm/results; slurm dirs go under this
# worktree root (tree_root/slurm/), not under ~/mgvm.
tree_root = os.path.abspath(os.path.join(os.path.dirname(os.path.abspath(__file__)), ".."))

DIST_TRACE_PLATFORMS = ("dynamic-cluster", "dynamic-clustering")


@dataclass
class RunPaths:
    result_dir: str
    config_dir: str
    run_root: str
    report_dir: str
    # Curated subset of aggregate metrics (driver totals, chiplet_all,
    # per-chiplet l2 hit-rate) emitted alongside the full report.
    core_report_dir: str
    # Per-percent checkpoint report directories, keyed by integer percentage.
    # Empty dict when --checkpoint is unset.
    checkpoint_report_dirs: Dict[int, str]
    # Per-percent checkpoint core report directories (same key set as above).
    checkpoint_core_report_dirs: Dict[int, str]
    pc_state_trace_dir: str
    access_count_dir: str
    memory_trace_dir: str
    pte_trace_dir: str
    dist_trace_dir: str
    data_allocation_log_dir: Optional[str]
    slurm_out_dir: str
    slurm_err_dir: str
    # CPU/mem-profile output dirs (slurm/<dir>/cpu-prof-<suffix> / mem-prof-<suffix>); None unless --profile.
    cpu_profile_dir: Optional[str]
    mem_profile_dir: Optional[str]
    # mgpusim/cmd/footprint output (--footprint mode): per-benchmark
    # shared_lines.csv + summary.json land under here, one subdir each.
    footprint_dir: str
    footprint_out_dir: str
    footprint_err_dir: str


def size_suffix(args) -> str:
    if args.platform == "staticcluster":
        return f"{args.cluster_size}"
    return f"{args.xDim}x{args.yDim}"


def build_jobname(benchmark, args) -> str:
    """Slurm job name: benchmark + platform + size, plus a tag for each
    enabled / non-default option so `squeue` distinguishes concurrent sweeps."""
    parts = [benchmark, args.platform, size_suffix(args)]

    if args.oracle_pc_table:
        parts.append("oracle")
    elif (args.platform == "dynamic-clustering"
          and args.pc_classifier == "score"):
        parts.append("score")
    if args.small:
        parts.append("small")
    if args.round_robin:
        parts.append("rr")
    if args.pte_caching:
        parts.append("ptec")
    if not args.fast_h2d:
        parts.append("noh2d")
    if args.checkpoint:
        parts.append("ckpt" + "_".join(str(p) for p in args.checkpoint))
    if args.num_req_per_cycle != 16:
        parts.append(f"nrpc{args.num_req_per_cycle}")
    if args.switch_latency != 32:
        parts.append(f"swl{args.switch_latency}")
    if args.time_limit > 0:
        parts.append(f"tl{args.time_limit}")
    if args.memory_trace:
        parts.append("mt")
    if args.pte_trace:
        parts.append("ptt")
    if args.access_count:
        parts.append("ac")
    if args.pc_state_trace:
        parts.append("pcs")
    if args.log_data_allocation:
        parts.append("lda")

    return "_".join(parts)


def build_config_id(args) -> str:
    if args.platform == "staticcluster":
        cid = f"{args.platform}-{args.cluster_size}"
    else:
        cid = f"{args.platform}-{args.xDim}x{args.yDim}"
    if args.platform == "dynamic-clustering":
        if args.oracle_pc_table:
            cid += "-oracle"
        else:
            if args.shared_pc_table:
                cid += "-shared"
            if args.pc_classifier == "score":
                cid += "-score"
    if args.small:
        cid += "-small"
    if args.round_robin:
        cid += "-round-robin"
    return cid


def compute_run_paths(args, current_time: str) -> RunPaths:
    result_dir = f"{username}/mgvm/results"
    config_dir = f"{result_dir}/{build_config_id(args)}"
    date_part, _, time_part = current_time.partition("-")
    run_root = f"{config_dir}/{date_part}/{time_part}"

    report_dir = f"{run_root}/reports"
    core_report_dir = f"{run_root}/reports-core"
    memory_trace_dir = f"{run_root}/memory-traces"
    pte_trace_dir = f"{run_root}/pte-traces"
    access_count_dir = f"{run_root}/access-counts"
    pc_state_trace_dir = f"{run_root}/pc-state-traces"
    dist_trace_dir = f"{run_root}/dist-traces"
    data_allocation_log_dir = (
        f"{run_root}/data-allocation-logs" if args.log_data_allocation else None
    )

    checkpoint_report_dirs: Dict[int, str] = {}
    checkpoint_core_report_dirs: Dict[int, str] = {}
    for pct in args.checkpoint:
        checkpoint_report_dirs[pct] = f"{run_root}/reports-{pct}%"
        checkpoint_core_report_dirs[pct] = f"{run_root}/reports-{pct}%-core"

    suffix = size_suffix(args)
    slurm_base = f"{tree_root}/slurm/{args.slurm_dir}"
    slurm_out_dir = f"{slurm_base}/outputs-{suffix}"
    slurm_err_dir = f"{slurm_base}/errors-{suffix}"
    cpu_profile_dir = f"{slurm_base}/cpu-prof-{suffix}" if args.profile else None
    mem_profile_dir = f"{slurm_base}/mem-prof-{suffix}" if args.profile else None

    footprint_dir = f"{run_root}/footprint"
    footprint_out_dir = f"{slurm_base}/footprint-outputs-{suffix}"
    footprint_err_dir = f"{slurm_base}/footprint-errors-{suffix}"

    return RunPaths(
        result_dir=result_dir,
        config_dir=config_dir,
        run_root=run_root,
        report_dir=report_dir,
        core_report_dir=core_report_dir,
        checkpoint_report_dirs=checkpoint_report_dirs,
        checkpoint_core_report_dirs=checkpoint_core_report_dirs,
        pc_state_trace_dir=pc_state_trace_dir,
        access_count_dir=access_count_dir,
        memory_trace_dir=memory_trace_dir,
        pte_trace_dir=pte_trace_dir,
        dist_trace_dir=dist_trace_dir,
        data_allocation_log_dir=data_allocation_log_dir,
        slurm_out_dir=slurm_out_dir,
        slurm_err_dir=slurm_err_dir,
        cpu_profile_dir=cpu_profile_dir,
        mem_profile_dir=mem_profile_dir,
        footprint_dir=footprint_dir,
        footprint_out_dir=footprint_out_dir,
        footprint_err_dir=footprint_err_dir,
    )


def parse_checkpoint_percents(raw: str) -> List[int]:
    """Parse the ``--checkpoint`` flag value (e.g. "25,50,75") into a sorted
    list of unique integer percentages in the valid 1-99 range.

    Empty string returns []. Float-style legacy input (e.g. "0.25") is rejected
    with an explanatory error to nudge users toward the new integer form.
    """
    raw = (raw or "").strip()
    if not raw:
        return []
    seen = set()
    out: List[int] = []
    for piece in raw.split(','):
        piece = piece.strip()
        if not piece:
            continue
        if '.' in piece:
            raise ValueError(
                f"--checkpoint expects integer percentages (e.g. 25,50,75), "
                f"got fractional value {piece!r}. Use 25 instead of 0.25."
            )
        try:
            v = int(piece)
        except ValueError as e:
            raise ValueError(
                f"--checkpoint percentage {piece!r} is not an integer: {e}"
            ) from e
        if v <= 0 or v >= 100:
            raise ValueError(
                f"--checkpoint percentage out of range: {v} (expected 1-99)"
            )
        if v in seen:
            continue
        seen.add(v)
        out.append(v)
    out.sort()
    return out


def ensure_run_dirs(args, paths: RunPaths) -> None:
    # --footprint builds a completely different command (mgpusim/cmd/footprint,
    # not the timing-sim binary) with none of -metric-file-name/-trace-*/
    # -cpuprofile/etc, so none of the dirs below apply — only the footprint
    # output + its own slurm out/err dirs.
    if args.footprint:
        os.makedirs(paths.footprint_dir, exist_ok=True)
        os.makedirs(paths.footprint_out_dir, exist_ok=True)
        os.makedirs(paths.footprint_err_dir, exist_ok=True)
        return

    os.makedirs(paths.report_dir, exist_ok=True)
    os.makedirs(paths.core_report_dir, exist_ok=True)
    for d in paths.checkpoint_report_dirs.values():
        os.makedirs(d, exist_ok=True)
    for d in paths.checkpoint_core_report_dirs.values():
        os.makedirs(d, exist_ok=True)
    os.makedirs(paths.slurm_out_dir, exist_ok=True)
    os.makedirs(paths.slurm_err_dir, exist_ok=True)
    if paths.cpu_profile_dir:
        os.makedirs(paths.cpu_profile_dir, exist_ok=True)
    if paths.mem_profile_dir:
        os.makedirs(paths.mem_profile_dir, exist_ok=True)
    if args.access_count:
        os.makedirs(paths.access_count_dir, exist_ok=True)
    if args.memory_trace:
        os.makedirs(paths.memory_trace_dir, exist_ok=True)
    if args.pte_trace:
        os.makedirs(paths.pte_trace_dir, exist_ok=True)
    if args.platform in DIST_TRACE_PLATFORMS:
        os.makedirs(paths.dist_trace_dir, exist_ok=True)
    if args.pc_state_trace:
        os.makedirs(paths.pc_state_trace_dir, exist_ok=True)
    if args.log_data_allocation:
        os.makedirs(paths.data_allocation_log_dir, exist_ok=True)


def build_benchmark_entry(benchmark, args, paths: RunPaths) -> Optional[BenchmarkEntry]:
    if benchmark not in benchmark_configs:
        print(f"[WARNING] Unknown benchmark: {benchmark}")
        return None

    config = benchmark_configs[benchmark]
    binary_name = config.get('name', benchmark)
    alias = config.get('alias', binary_name)
    base_dir = f"{tree_root}/mgpusim/samples"

    cmd = (
        f"{base_dir}/{binary_name}/{binary_name}"
        f"{base_options}"
        f" -benchmark={alias}"
        f" -platform-type={args.platform}"
        f" -xDim={args.xDim}"
        f" -yDim={args.yDim}"
        f" -sched-partition \"{config.get('sched-partition', 'Xdiv')}\""
        f" -data-partitioning \"{config.get('data-partitioning', 'kernel-wide')}\""
        f" -data-strides \"{config.get('data-strides', '1')}\""
        f" -col-size-for-lasp {config.get('col-size-for-lasp', 0)}"
    )

    if config.get('params'):
        cmd += f" {config['params']}"
    if args.pte_caching:
        cmd += " -use-pte-caching "
    if args.fast_h2d:
        cmd += " -fast-h2d "
    if args.log_data_allocation:
        cmd += " -log-data-allocation"
        cmd += (
            f" -log-data-allocation-filename={paths.data_allocation_log_dir}"
            f"/{benchmark}_{args.platform}_{args.xDim}x{args.yDim}_data_allocation_log"
        )
    if args.shared_pc_table:
        cmd += " -use-shared-pc-table "
    if args.oracle_pc_table:
        cmd += " -use-oracle-pc-table "

    # Non-oracle runtime PCTable (plain online table or the shared
    # zero-overhead-sync table): pick the classifier (and, for score mode,
    # its weights). Irrelevant when the oracle table is used.
    if not args.oracle_pc_table and args.platform == "dynamic-clustering":
        cmd += f" -pc-classifier={args.pc_classifier}"
        if args.pc_classifier == "score":
            cmd += (
                f" -pc-score-w-reuse={args.pc_score_w_reuse}"
                f" -pc-score-w-share={args.pc_score_w_share}"
                f" -pc-score-w-hop={args.pc_score_w_hop}"
                f" -pc-score-hop-ref={args.pc_score_hop_ref}"
                f" -pc-score-warmup-shrink={args.pc_score_warmup_shrink}"
            )

    cmd += f" -max-inst={config.get('max_inst', 0)}"
    if args.checkpoint:
        cmd += f" -checkpoint={','.join(str(p) for p in args.checkpoint)}"
    cmd += f" -num-req-per-cycle={args.num_req_per_cycle}"
    cmd += f" -switch-latency={args.switch_latency}"
    cmd += f" -cluster-size={args.cluster_size}"
    cmd += f" -time-limit={args.time_limit}"

    if args.memory_trace:
        cmd += (
            f" -trace-memory-access=true"
            f"  -trace-memory-access-filename={paths.memory_trace_dir}"
            f"/{benchmark}_{args.platform}_{args.xDim}x{args.yDim}_memory_trace"
        )
    if args.pte_trace:
        cmd += (
            f" -trace-pte-access=true"
            f" -trace-pte-access-filename={paths.pte_trace_dir}"
            f"/{benchmark}_{args.platform}_{args.xDim}x{args.yDim}_pte_trace"
        )
    if args.platform in DIST_TRACE_PLATFORMS:
        cmd += (
            f" -trace-dist-filename={paths.dist_trace_dir}"
            f"/{benchmark}_{args.platform}_{args.xDim}x{args.yDim}_dist_trace"
        )
    if args.verbose:
        cmd += " -verbose=True "
    if paths.cpu_profile_dir:
        cmd += (
            f" -cpuprofile={paths.cpu_profile_dir}"
            f"/{benchmark}_{args.platform}_{args.xDim}x{args.yDim}.prof"
        )
    if paths.mem_profile_dir:
        cmd += (
            f" -memprofile={paths.mem_profile_dir}"
            f"/{benchmark}_{args.platform}_{args.xDim}x{args.yDim}.mprof"
        )

    metric_prefix = (
        f"{paths.report_dir}/{benchmark}_{args.platform}_{args.xDim}x{args.yDim}"
    )
    cmd += f" -metric-file-name={metric_prefix} "

    # Curated subset of aggregate metrics — emitted to a sibling
    # reports-core/ directory so the main reports/ stays untouched.
    core_prefix = (
        f"{paths.core_report_dir}/{benchmark}_{args.platform}_{args.xDim}x{args.yDim}"
    )
    cmd += f" -core-file-name={core_prefix} "

    # Dump each checkpoint CSV directly into its parallel reports-<n>%/
    # directory via the Go-side -checkpoint-file-name flag. The Go side
    # takes a comma-separated list aligned 1:1 with -checkpoint (sorted
    # ascending), so we walk args.checkpoint (already sorted) and build
    # matching path lists. This avoids the previous post-run mv hack,
    # which only fired after the *full* sim finished and left the
    # checkpoint CSV stranded in reports/ for any still-running job.
    post_cmd = ""
    if args.checkpoint:
        size_suf = f"{args.platform}_{args.xDim}x{args.yDim}"
        ckpt_prefixes = [
            f"{paths.checkpoint_report_dirs[p]}/{benchmark}_{size_suf}"
            for p in args.checkpoint
        ]
        cmd += f" -checkpoint-file-name={','.join(ckpt_prefixes)} "
        ckpt_core_prefixes = [
            f"{paths.checkpoint_core_report_dirs[p]}/{benchmark}_{size_suf}"
            for p in args.checkpoint
        ]
        cmd += f" -checkpoint-core-file-name={','.join(ckpt_core_prefixes)} "

    slurm_out = f"{paths.slurm_out_dir}/{alias}.out"
    slurm_err = f"{paths.slurm_err_dir}/{alias}.err"
    sbatch_jobname = build_jobname(benchmark, args)

    return BenchmarkEntry(
        name=benchmark,
        binary_name=binary_name,
        alias=alias,
        cmd=cmd,
        expected_csv=f"{metric_prefix}.csv",
        slurm_out=slurm_out,
        slurm_err=slurm_err,
        sbatch_jobname=sbatch_jobname,
        post_cmd=post_cmd,
    )


# -zipf-s=X lives inside a benchmark's 'params' string for the timing-sim
# binary (each samples/<bench>/main.go defines its own -zipf-s flag; see
# e.g. samples/spmv/main.go). mgpusim/cmd/footprint applies it once, globally,
# via its own top-level -zipf-s flag instead (footprint.Replay.Flags doesn't
# declare one), so it must be pulled out of 'params' rather than passed
# through verbatim — leaving it in would make the replay's flag.Parse fail
# with "flag provided but not defined". Anchored on a preceding
# whitespace/start so it doesn't also match inside e.g. bptree's
# '-bptree-zipf-s=0' (a distinct, benchmark-specific flag).
_ZIPF_S_RE = re.compile(r'(?:^|\s)-zipf-s=([0-9.eE+-]+)')


def extract_zipf_s(params: str) -> (str, str):
    """Returns (zipf_s_value, params_with_zipf_s_removed)."""
    m = _ZIPF_S_RE.search(params or '')
    zipf_s = m.group(1) if m else '0'
    stripped = _ZIPF_S_RE.sub(' ', params or '').strip()
    return zipf_s, stripped


def build_footprint_entry(benchmark, args, paths: RunPaths) -> Optional[BenchmarkEntry]:
    """mgpusim/cmd/footprint entry: replays a benchmark's address stream on
    the host (no timing sim) to get an untruncated shared-footprint. Only
    benchmarks with a registered footprint.Replay succeed — others fail at
    the Go binary with a clear "no replay for X" in the job's .err, same as
    an unsupported flag combo would for the timing-sim entries above; this
    function does not pre-filter, to avoid drifting out of sync with the Go
    registry (mgpusim/cmd/footprint/main.go's blank imports)."""
    if benchmark not in benchmark_configs:
        print(f"[WARNING] Unknown benchmark: {benchmark}")
        return None

    config = benchmark_configs[benchmark]
    footprint_bin = f"{tree_root}/mgpusim/cmd/footprint/footprint"
    zipf_s, stripped_params = extract_zipf_s(config.get('params', ''))

    cmd = (
        f"{footprint_bin}"
        f" -benchmark={benchmark}"
        f" -xDim={args.xDim}"
        f" -yDim={args.yDim}"
        f" -sched-partition \"{config.get('sched-partition', 'Xdiv')}\""
        f" -zipf-s={zipf_s}"
        f" -params=\"{stripped_params}\""
        f" -window-sizes=\"{args.footprint_window_sizes}\""
        f" -stack-distance-thresholds-mb=\"{args.footprint_stack_distance_mb}\""
        f" -output-dir={paths.footprint_dir}"
    )

    slurm_out = f"{paths.footprint_out_dir}/{benchmark}.out"
    slurm_err = f"{paths.footprint_err_dir}/{benchmark}.err"

    return BenchmarkEntry(
        name=benchmark,
        binary_name="footprint",
        alias=benchmark,
        cmd=cmd,
        expected_csv=(
            f"{paths.footprint_dir}/{benchmark}_{args.xDim}x{args.yDim}"
            f"/summary.json"
        ),
        slurm_out=slurm_out,
        slurm_err=slurm_err,
        sbatch_jobname=f"fp_{build_jobname(benchmark, args)}",
    )


def normalize_nodelist(s: str) -> List[str]:
    cleaned = re.sub(r'\s+', '', s or '')
    if not cleaned:
        return []
    return [n for n in cleaned.split(',') if n]


def submit_entry(entry: BenchmarkEntry, node: str) -> bool:
    nodelist_directive = f"#SBATCH --nodelist={node}\n" if node else ""
    post_step = f"\n# 체크포인트 CSV 재배치\n{entry.post_cmd}\n" if entry.post_cmd else ""
    slurm_script = f"""sbatch <<EOT
#!/bin/bash
#SBATCH --job-name={entry.sbatch_jobname}
#SBATCH --output={entry.slurm_out}
#SBATCH --error={entry.slurm_err}
#SBATCH --nodes=1
#SBATCH --ntasks=1
{nodelist_directive}#SBATCH --cpus-per-task=1

echo "Job started at: \\$(date)"
echo "Running on node: \\$(hostname)"
echo "Running benchmark: {entry.name}"
echo "Command: {entry.cmd}"

# 명령어 실행
{entry.cmd}
{post_step}
echo "Job finished at: \\$(date)"
EOT
"""
    print(f"[{time.strftime('%Y-%m-%d %H:%M:%S')}] Submitting Job: {entry.name}")
    result = subprocess.run(
        slurm_script, shell=True, capture_output=True, text=True,
    )
    out = result.stdout.strip()
    err = result.stderr.strip()
    if result.returncode != 0:
        print(
            f"[ERROR] sbatch failed for {entry.name} (rc={result.returncode})",
            file=sys.stderr,
        )
        if out:
            print(f"  stdout: {out}", file=sys.stderr)
        if err:
            print(f"  stderr: {err}", file=sys.stderr)
        return False
    if out:
        print(f"  {out}")
    if err:
        print(f"  [sbatch stderr] {err}", file=sys.stderr)
    return True


def build_run_id(args, current_time: str) -> str:
    return f"{build_config_id(args)}-{current_time}"


def build_manifest(
    args, current_time: str, paths: RunPaths, entries: List[BenchmarkEntry],
) -> Manifest:
    run_id = build_run_id(args, current_time)
    slurm_dir_abs = f"{tree_root}/slurm/{args.slurm_dir}"
    return Manifest(
        submitted_at=time.strftime("%Y-%m-%dT%H:%M:%S"),
        submitter=os.environ.get("USER") or os.getlogin(),
        run_id=run_id,
        result_dir=paths.run_root,
        slurm_dir=slurm_dir_abs,
        comment=args.comment,
        args={k: v for k, v in vars(args).items()},
        result_dirs={
            "report": paths.report_dir,
            "core_report": paths.core_report_dir,
            "checkpoint_report": paths.checkpoint_report_dirs or None,
            "checkpoint_core_report": paths.checkpoint_core_report_dirs or None,
            "memory_trace": paths.memory_trace_dir if args.memory_trace else None,
            "pte_trace": paths.pte_trace_dir if args.pte_trace else None,
            "access_count": paths.access_count_dir if args.access_count else None,
            "dist_trace": (
                paths.dist_trace_dir
                if args.platform in DIST_TRACE_PLATFORMS else None
            ),
            "pc_state_trace": (
                paths.pc_state_trace_dir if args.pc_state_trace else None
            ),
            "data_allocation_log": paths.data_allocation_log_dir,
            "footprint": paths.footprint_dir if args.footprint else None,
        },
        base_options=base_options.strip(),
        benchmarks=entries,
    )


def write_manifest_files(args, manifest: Manifest) -> None:
    primary = manifest_path_in(manifest.result_dir)
    manifest.write(primary)
    print(f"Manifest: {primary}")

    backlink = slurm_backlink_path(manifest.slurm_dir, manifest.run_id)
    try:
        manifest.write(backlink)
        print(f"Manifest backlink: {backlink}")
    except OSError as e:
        print(f"[WARN] Could not write slurm backlink at {backlink}: {e}")


def run_sweep(args, current_time: str, platform: str, dry_run: bool) -> int:
    args.platform = platform
    print(f"\n--- {'[DRY-RUN] ' if dry_run else ''}Preparing platform: {platform} ---")

    paths = compute_run_paths(args, current_time)
    ensure_run_dirs(args, paths)

    entry_builder = build_footprint_entry if args.footprint else build_benchmark_entry
    entries: List[BenchmarkEntry] = []
    for bench in benchmarks:
        entry = entry_builder(bench, args, paths)
        if entry:
            entries.append(entry)

    if not entries:
        print("[WARN] No benchmark entries built for this platform; skipping.")
        return 0

    manifest = build_manifest(args, current_time, paths, entries)
    write_manifest_files(args, manifest)

    pool = args.nodelist  # List[str], may be empty
    pool_display = ",".join(pool) if pool else "(unset)"

    if dry_run:
        print(f"[DRY-RUN] Would submit {len(entries)} jobs for platform '{platform}' "
              f"with nodelist={pool_display}:")
        for i, e in enumerate(entries):
            node = pool[i % len(pool)] if pool else ""
            print(f"  - {e.sbatch_jobname} -> {node or '(slurm-picked)'}")
        return 0

    signal.signal(signal.SIGHUP, signal.SIG_IGN)
    submitted = 0
    failed = 0
    for i, entry in enumerate(entries):
        node = pool[i % len(pool)] if pool else ""
        if submit_entry(entry, node):
            submitted += 1
        else:
            failed += 1
    if failed:
        print(f"[WARN] {failed}/{len(entries)} sbatch submissions failed for "
              f"platform '{platform}'.", file=sys.stderr)
    return submitted


# Backward-compat shim: callers that imported run_benchmark_slurm still work.
def run_benchmark_slurm(benchmark, args, current_time):
    signal.signal(signal.SIGHUP, signal.SIG_IGN)
    paths = compute_run_paths(args, current_time)
    ensure_run_dirs(args, paths)
    entry = build_benchmark_entry(benchmark, args, paths)
    if entry:
        pool = getattr(args, 'nodelist', []) or []
        node = pool[0] if pool else ""
        submit_entry(entry, node)


if __name__ == "__main__":
    parser = argparse.ArgumentParser()
    parser.add_argument('--platform', type=str, default='mesh',
                        help='Platform type to use (default: mesh)')
    parser.add_argument('--time-limit', type=int, default=0,
                        help='Time limit for the simulation in hours (default: 0 for no limit)')
    parser.add_argument('--small', type=bool, default=False,
                        help='Run small benchmarks only')
    parser.add_argument('--new-params', type=bool, default=False,
                        help='Use params_new.py (cross-chiplet data-sharing delta '
                             'configs) instead of params.py')
    parser.add_argument('--slurm-dir', type=str, default=os.path.basename(tree_root),
                        help='Subdirectory under ~/mgvm/slurm/ for Slurm scripts (default: worktree name)')
    parser.add_argument('--profile', type=bool, default=True,
                        help='Write pprof CPU and heap profiles per benchmark into '
                             'slurm/<slurm-dir>/cpu-prof-<size>/ and mem-prof-<size>/')
    parser.add_argument('--log-data-allocation', type=bool, default=False,
                        help='Log data allocation or not')
    parser.add_argument('--access-count', type=bool, default=False,
                        help='Measure memory access count per address or not')
    parser.add_argument('--memory-trace', type=bool, default=False,
                        help='Measure memory access per time')
    parser.add_argument('--pte-trace', type=bool, default=False,
                        help='Measure PTE access per time')
    parser.add_argument('--pte-caching', type=bool, default=True,
                        help='Enable PTE caching or not')
    parser.add_argument('--fast-h2d', type=bool, default=True,
                        help='Skip CP/DMA/DRAM simulation for Host-to-Device '
                             'copies (page table, code object, kernel args, '
                             'input data). Bytes are written directly into '
                             'each chiplet DRAM Storage. H2D traffic is then '
                             'invisible to -trace-mem and related metrics; '
                             'kernel-execution metrics are unaffected.')
    parser.add_argument('--pc-state-trace', type=bool, default=False,
                        help='Measure pc state per time')
    parser.add_argument('--xDim', type=int, default=4, help='X-axis Dimension')
    parser.add_argument('--yDim', type=int, default=4, help='Y-axis Dimension')
    parser.add_argument('--switch-latency', type=int, default=32,
                        help='Switch latency')
    parser.add_argument('--num-req-per-cycle', type=int, default=16,
                        help='Interconnect bandwidth')
    parser.add_argument('--cluster-size', type=int, default=9,
                        help='Cluster size')
    parser.add_argument('--verbose', type=bool, default=False,
                        help='Verbose output')
    parser.add_argument('--round-robin', type=bool, default=False,
                        help='Use round-robin scheduling or not')
    parser.add_argument('--mix', type=bool, default=False,
                        help='Small amounts of benchmarks but all platforms')
    parser.add_argument('--oracle-pc-table', type=bool, default=False,
                        help='Use oracle PC table or not')
    parser.add_argument('--shared-pc-table', type=bool, default=False,
                        help='Use shared PC table (online classifier with '
                             'zero-overhead cross-chiplet hotness sync)')
    parser.add_argument('--pc-classifier', type=str, default='threshold',
                        choices=['threshold', 'score'],
                        help='Runtime PCTable classifier for dynamic-clustering '
                             '(non-oracle). "threshold" = decision tree on '
                             'reuse+sharing; "score" = weighted reuse+sharing+hop '
                             'with hop-gated warm-up shrink.')
    parser.add_argument('--pc-score-w-reuse', type=float, default=0.40,
                        help='score mode: reuse weight')
    parser.add_argument('--pc-score-w-share', type=float, default=0.30,
                        help='score mode: sharing weight')
    parser.add_argument('--pc-score-w-hop', type=float, default=0.30,
                        help='score mode: hop weight')
    parser.add_argument('--pc-score-hop-ref', type=float, default=0,
                        help='score mode: hop normalization ref (0 = auto: mesh diameter)')
    parser.add_argument('--pc-score-warmup-shrink', type=float, default=0.7,
                        help='score mode: hop-gated warm-up shrink (0 disables shrink)')
    parser.add_argument('--nodelist', type=str, default='',
                        help='Slurm nodelist. Empty (default) emits no --nodelist '
                             'directive, so slurm picks (in practice it packs all jobs '
                             'onto one node since each job only asks for 1 CPU). '
                             'Single name (e.g. compasslab3) pins every job to that '
                             'node. Comma-separated list (e.g. '
                             'compasslab1,compasslab2,compasslab3) round-robin assigns '
                             'each submitted job to one of these nodes; each sbatch '
                             'still uses --nodes=1 so there is no slurm conflict.')
    parser.add_argument('--checkpoint', type=str, default='25,50,75',
                        help='Comma-separated integer percentages (1-99) of -max-inst '
                             'at which to dump extra metrics snapshots. e.g. '
                             '"--checkpoint=25,50,75" dumps three snapshots, each as '
                             '<-metric-file-name>_<n>%%.csv. Single value "25" works '
                             'too. Requires per-benchmark max_inst > 0. '
                             'Values are sorted ascending and deduped.')
    parser.add_argument('--comment', type=str, default='',
                        help='Free-form note recorded in manifest.json (e.g. why this '
                             'sweep was run / what changed). Empty by default.')
    parser.add_argument('--footprint', action='store_true',
                        help='Submit mgpusim/cmd/footprint replay jobs instead of the '
                             'timing-sim binary: a host-side address-stream replay that '
                             'gets an untruncated inter-chiplet shared-footprint without '
                             'running a timing simulation. Ignores all timing/tracer '
                             'flags below (-timing, -trace-*, -mem-allocator-type, '
                             'etc. do not apply); only -xDim/-yDim/-sched-partition and '
                             'the benchmark\'s own params are used. Only benchmarks with '
                             'a registered footprint.Replay succeed — see mgpusim/cmd/'
                             'footprint -list. Requires the footprint binary to be '
                             'built first (compile.py now does this automatically). '
                             'Output: <result_dir>/footprint/<bench>_<xDim>x<yDim>/'
                             '{shared_lines.csv,summary.json}, consumed by analyze/'
                             'cacheline/shared_footprint_capacity_cdf.py --footprint-dir=.')
    parser.add_argument('--footprint-window-sizes', type=str,
                        default='1000000,2000000,3000000,4000000,5000000,10000000,100000000',
                        help='Only used with --footprint. Comma-separated tick counts '
                             'for windowed-WSS bucketing, forwarded verbatim to cmd/'
                             'footprint\'s -window-sizes (one tick = one line touch, '
                             'not weighted by TouchN\'s access count — see footprint/'
                             'window.go). Empty string disables windowed-WSS tracking '
                             '(only the lifetime footprint in shared_lines.csv is '
                             'written, which is faster).')
    # Power-of-2 sweep 1 MB..4096 MB — wide enough to locate where hit rate
    # saturates without a separate run to widen the range afterward. Extra
    # thresholds are nearly free once the Fenwick-tree walk is already
    # happening per touch, so the width itself isn't the expensive part.
    FOOTPRINT_DEFAULT_THRESHOLDS_MB = '1,2,4,8,16,32,64,128,256,512,1024,2048,4096'
    parser.add_argument('--footprint-stack-distance-mb', type=str,
                        default=FOOTPRINT_DEFAULT_THRESHOLDS_MB,
                        help='Only used with --footprint. Comma-separated per-chiplet '
                             'cache sizes in MB for EXACT stack-distance hit-rate '
                             'tracking, forwarded verbatim to cmd/footprint\'s '
                             '-stack-distance-thresholds-mb (see footprint/stackdist.go). '
                             'Chiplet-local: each chiplet\'s own full reference stream, '
                             'shared and private lines mixed together as it actually '
                             'sees them. Default is a 1-4096 MB power-of-2 sweep; empty '
                             'disables it — meaningfully more expensive than '
                             '--footprint-window-sizes (one Fenwick tree per chiplet, '
                             'O(log ticks) per touch). For the lifetime shared/private '
                             'capacity split (no stack-distance gating), see '
                             'summary.json\'s shared_mb_per_chiplet / '
                             'private_mb_per_chiplet / chiplet_capacity — always written, '
                             'no extra flag needed.')
    parser.add_argument('--dry-run', action='store_true',
                        help='Build entries and write manifest, but do not submit to slurm')
    args = parser.parse_args()

    # Swap the params source before any use of benchmarks/benchmark_configs.
    # Module-level rebind here is seen by all functions (they resolve these
    # globals at call time).
    if args.new_params:
        from params_new import benchmarks, benchmark_configs
        print("Params source: params_new.py (cross-chiplet data-sharing delta configs)")
    else:
        print("Params source: params.py")

    try:
        args.nodelist = normalize_nodelist(args.nodelist)
    except ValueError as e:
        parser.error(str(e))

    try:
        args.checkpoint = parse_checkpoint_percents(args.checkpoint)
    except ValueError as e:
        parser.error(str(e))

    current_time = time.strftime("%Y%m%d-%H%M%S")

    print("=" * 80)
    print("Slurm Benchmark Submission Tool")
    print(f"Total benchmarks: {len(benchmarks)}")
    print(f"Platform: {args.platform}")
    print(f"Nodelist: {','.join(args.nodelist) if args.nodelist else '(unset — slurm decides, typically packs onto one node)'}")
    if args.footprint:
        print("Mode: FOOTPRINT (host-side replay, no timing sim)")
    if args.dry_run:
        print("Mode: DRY-RUN (manifest only, no sbatch)")
    print("=" * 80)

    if args.small:
        for bench in benchmarks:
            if 'max_inst' in benchmark_configs[bench]:
                benchmark_configs[bench]['max_inst'] *= 0.25
                benchmark_configs[bench]['max_inst'] = int(
                    benchmark_configs[bench]['max_inst']
                )

    if args.round_robin:
        print("Using round-robin scheduling for benchmarks.")
        for bench in benchmarks:
            benchmark_configs[bench]['data-partitioning'] = 'kernel-wide'

    platforms = ['mesh', 'ideal', 'pt-ideal', 'direct', 'mono'] if args.mix else [args.platform]
    submitted = 0
    for platform in platforms:
        submitted += run_sweep(args, current_time, platform, args.dry_run)

    if args.dry_run:
        print(f"\n[DRY-RUN] Manifests written; 0 jobs submitted "
              f"({len(platforms)} platform(s)).")
    else:
        print(f"\nAll {submitted} jobs have been submitted to the Slurm queue "
              f"({len(platforms)} platform(s)).")
        print(f"Check status with: squeue -u {os.getlogin()}")
