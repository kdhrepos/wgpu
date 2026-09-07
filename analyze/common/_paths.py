"""Path helpers for the per-run results layout used by run.py.

Layout (run.py nests the timestamp as <date>/<time>):
    ~/mgvm/results/<config>/<date>/<time>/{reports,memory-traces,pte-traces,
                                           access-counts,pc-state-traces,
                                           dist-traces,data-allocation-logs,
                                           footprint}/

Older 2-level runs (<config>/<timestamp>) still resolve.

`run_id` (the value every analyze script passes as --date/--trace-id) is
resolved by `run_root()`, which accepts any of these forms so you rarely have
to hunt for a timestamp by hand:

    ""  / "latest"             newest run across *all* configs (by mtime)
    "<config>"                 newest run under that config   (e.g. "mesh-6x6")
    "<config>/<date>"          newest run that day under the config
    "<config>/latest"          newest run under that config
    "latest:<config>"          same as above
    "<config>/<date>/<time>"   exact run
    "<date>-<time>" / "<time>" auto-resolved when unambiguous
    "<substring>"              fuzzy match against "<config>/<date>/<time>"

Run `python3 _paths.py` (or with a config name) to list available runs.
"""

import sys
from pathlib import Path

RESULTS_ROOT = Path.home() / "mgvm" / "results"


def _is_run_dir(p: Path) -> bool:
    """A run dir is a leaf dir that actually produced output — reports/ for a
    timing-sim run, footprint/ for a --footprint run (which never creates
    reports/, see run.py's ensure_run_dirs)."""
    return p.is_dir() and ((p / "reports").is_dir() or (p / "footprint").is_dir())


def _iter_configs():
    for c in sorted(RESULTS_ROOT.iterdir()):
        if c.is_dir() and not c.name.startswith("."):
            yield c


def _iter_runs(config: str | None = None):
    """Yield every run dir. New layout nests <config>/<date>/<time>; the run dir
    is the <time> leaf. Older <config>/<timestamp> runs are still picked up."""
    configs = [RESULTS_ROOT / config] if config else _iter_configs()
    for c in configs:
        if not c.is_dir():
            continue
        for child in sorted(c.iterdir()):
            if not child.is_dir() or child.name.startswith("."):
                continue
            if _is_run_dir(child):                 # old <config>/<timestamp>
                yield child
            else:                                   # new <config>/<date>/<time>
                for leaf in sorted(child.iterdir()):
                    if _is_run_dir(leaf):
                        yield leaf


def _config_of(ts: Path) -> Path:
    """The <config> dir on a run dir's path (direct child of RESULTS_ROOT)."""
    p = ts
    while p.parent != RESULTS_ROOT and p.parent != p:
        p = p.parent
    return p


def _run_key(ts: Path) -> str:
    """Run id relative to its config: '<date>/<time>' (new) or '<timestamp>' (old)."""
    return str(ts.relative_to(_config_of(ts)))


def _full_label(ts: Path) -> str:
    """'<config>/<date>/<time>' (new) or '<config>/<timestamp>' (old)."""
    return f"{_config_of(ts).name}/{_run_key(ts)}"


def _run_ids(ts: Path) -> set:
    """All string ids that should resolve exactly to this run dir."""
    config = _config_of(ts).name
    key = _run_key(ts)                              # 'date/time' or 'timestamp'
    rels = {key}
    if "/" in key:
        date, time = key.split("/", 1)
        rels |= {f"{date}-{time}", time}            # combined + bare time
    ids = set()
    for r in rels:
        ids.add(r)
        ids.add(f"{config}/{r}")
    return ids


def run_tag(run_id) -> str:
    """Filesystem-safe unique run label: '<date>-<time>' (new) or '<timestamp>' (old)."""
    return _run_key(run_root(run_id)).replace("/", "-")


def run_config(run_id) -> str:
    """Config name ('<config>') for a run id — the RESULTS_ROOT child dir.
    Layout-agnostic: works for both '<config>/<date>/<time>' and '<config>/<timestamp>'."""
    return _config_of(run_root(run_id)).name


def run_dir_tag(run_dir) -> str:
    """Config-qualified tag for a resolved run dir Path:
    '<config>-<date>-<time>' (new) / '<config>-<timestamp>' (old).
    Layout-agnostic — flattens the run dir's path relative to the results root."""
    run_dir = Path(run_dir)
    try:
        return "-".join(run_dir.resolve().relative_to(RESULTS_ROOT.resolve()).parts)
    except ValueError:
        return run_dir.name


def latest_run(config: str | None = None) -> Path:
    runs = list(_iter_runs(config))
    if not runs:
        where = f"config {config!r}" if config else "any config"
        raise FileNotFoundError(f"No completed run dirs under {where} in {RESULTS_ROOT}")
    return max(runs, key=lambda p: p.stat().st_mtime)


def run_root(run_id) -> Path:
    # 1. empty / "latest" -> newest run anywhere
    if run_id in (None, "", "latest"):
        return latest_run()
    run_id = str(run_id).strip()

    # 2. explicit "latest" of a config
    if run_id.startswith("latest:"):
        return latest_run(run_id.split(":", 1)[1])
    if run_id.endswith("/latest"):
        return latest_run(run_id[: -len("/latest")])

    # 3. exact run dir (absolute path or <config>/<date>/<time> under root)
    p = Path(run_id)
    if p.is_absolute() and _is_run_dir(p):
        return p
    cand = RESULTS_ROOT / run_id
    if _is_run_dir(cand):
        return cand

    runs = list(_iter_runs())

    # 4. a config (or <config>/<date>) directory -> its newest run underneath
    if cand.is_dir():
        under = [ts for ts in runs if cand in ts.parents]
        if under:
            return max(under, key=lambda t: t.stat().st_mtime)

    # 5. bare <date>/<time>, <date>-<time>, <time>, or old <timestamp>
    exact = [ts for ts in runs if run_id in _run_ids(ts)]
    if len(exact) == 1:
        return exact[0]
    if len(exact) > 1:
        names = "\n  ".join(sorted(_full_label(m) for m in exact))
        raise ValueError(
            f"ambiguous run id {run_id!r}; matches:\n  {names}\n"
            f"Specify as '<config>/<date>/<time>'."
        )

    # 6. fuzzy substring against "<config>/<date>/<time>"
    fuzzy = [ts for ts in runs if run_id in _full_label(ts)]
    if len(fuzzy) == 1:
        return fuzzy[0]
    if len(fuzzy) > 1:
        names = "\n  ".join(sorted(_full_label(m) for m in fuzzy))
        raise ValueError(
            f"ambiguous run id {run_id!r}; matches:\n  {names}\n"
            f"Pass it as '<config>/<date>/<time>' or use '<config>' for the newest."
        )

    raise FileNotFoundError(
        f"no run for {run_id!r}; tried absolute path, {RESULTS_ROOT}/{run_id}, "
        f"a config name, an exact timestamp, and a substring match."
    )


def report_dir(run_id) -> Path:
    return run_root(run_id) / "reports"


def memory_trace_dir(run_id) -> Path:
    return run_root(run_id) / "memory-traces"


def pte_trace_dir(run_id) -> Path:
    return run_root(run_id) / "pte-traces"


def access_count_dir(run_id) -> Path:
    return run_root(run_id) / "access-counts"


def pc_state_trace_dir(run_id) -> Path:
    return run_root(run_id) / "pc-state-traces"


def dist_trace_dir(run_id) -> Path:
    return run_root(run_id) / "dist-traces"


def data_allocation_log_dir(run_id) -> Path:
    return run_root(run_id) / "data-allocation-logs"


def footprint_dir(run_id) -> Path:
    """mgpusim/cmd/footprint output (run.py --footprint), not a timing trace:
    per-benchmark <bench>_<W>x<H>/{shared_lines.csv,summary.json}."""
    return run_root(run_id) / "footprint"


def pc_footprint_dir(run_id) -> Path:
    """Per-chiplet PC-table HLL footprint dumps (pctable-windowed-classifier):
    <bench>_<config>_pc_footprint.csv,
    columns: cycle,sim_time,chiplet_id,pc,hll_footprint,hotness,req_count."""
    return run_root(run_id) / "pc-footprint"


def manifest_path(run_id) -> Path:
    return run_root(run_id) / "manifest.json"


# ---------------------------------------------------------------------------
# 분석 출력 트리. motivation 그림들이 공통으로 쓰는 곳.
# ---------------------------------------------------------------------------

ANALYZE_ROOT = Path(__file__).resolve().parent.parent
OUT_ROOT = ANALYZE_ROOT / "_out"


def motivation_out_dir(domain: str, fig_name: str) -> Path:
    """`_out/<domain>/<fig_name>/` 디렉토리 생성 후 반환."""
    p = OUT_ROOT / domain / fig_name
    p.mkdir(parents=True, exist_ok=True)
    return p


def domain_out_dir(domain: str, name: str, run_id) -> Path:
    """표준 출력 위치 `_out/<domain>/<name>/<run_tag>/` 생성 후 반환.
    도메인 폴더 / 스크립트 이름 / 실행 날짜로 결과를 분리하는 공통 규약."""
    p = OUT_ROOT / domain / name / run_tag(run_id)
    p.mkdir(parents=True, exist_ok=True)
    return p


def address_stats_dir(run_id) -> Path:
    """`address_access_stats.py` 출력 위치 (없으면 빈 경로 — 호출자가 graceful skip)."""
    return OUT_ROOT / "address" / "address_access_stats" / run_tag(run_id)


def local_remote_ratio_xlsx(run_id) -> Path:
    """`local_remote_ratio.py` 가 저장한 엑셀 (없을 수 있음)."""
    return OUT_ROOT / "cacheline" / run_tag(run_id) / "local_remote_access_ratio.xlsx"


def _print_runs(config_filter: str | None) -> None:
    """List runs grouped by config, newest first, marking the latest."""
    from datetime import datetime

    configs = ([RESULTS_ROOT / config_filter] if config_filter
               else list(_iter_configs()))
    overall_latest = None
    try:
        overall_latest = latest_run()
    except FileNotFoundError:
        pass

    any_shown = False
    for c in configs:
        runs = sorted(_iter_runs(c.name), key=lambda p: p.stat().st_mtime,
                      reverse=True)
        if not runs:
            continue
        any_shown = True
        print(f"\n{c.name}/")
        for i, r in enumerate(runs):
            mtime = datetime.fromtimestamp(r.stat().st_mtime).strftime("%Y-%m-%d %H:%M")
            tags = []
            if i == 0:
                tags.append("config-latest")
            if overall_latest is not None and r == overall_latest:
                tags.append("LATEST")
            suffix = f"  <- {', '.join(tags)}" if tags else ""
            print(f"  {_run_key(r)}   ({mtime}){suffix}")
    if not any_shown:
        print(f"No completed runs under {RESULTS_ROOT}"
              + (f"/{config_filter}" if config_filter else ""))


if __name__ == "__main__":
    _print_runs(sys.argv[1] if len(sys.argv) > 1 else None)
