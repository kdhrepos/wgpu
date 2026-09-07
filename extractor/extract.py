"""Pull a configurable set of `what` metrics out of every benchmark CSV in a run dir.

Each MGPUSim result CSV has 3 columns (where, what, value) plus a leading
"Parameters" block where `what` is empty. This tool filters rows where `what`
matches one of the metrics in metrics.txt (default mode) or the metrics
passed via --metrics (custom mode), and emits a combined xlsx workbook
across all benchmarks.

Modes:
  default  no --metrics flag → metrics come from metrics.txt
           output: extractor/<run-name>/default.xlsx
  custom   --metrics=<name>[,<name>...] → those metrics only
           output: extractor/<run-name>/<metrics-joined>.xlsx

The workbook has two sheets: "long" (one row per match) and "wide"
(benchmark × metric pivot, collapsed by --aggregate).

Typical usage:

    # default metrics.txt
    python3 extract.py 20260516-181337-dynamic-clustering-4x4-oracle-small

    # custom: a single metric
    python3 extract.py <run> --metrics simulation_time

    # most-recent run
    python3 extract.py --latest

    # compare across multiple runs (adds run_id column)
    python3 extract.py <run-A> <run-B>
"""

import argparse
import csv
import sys
from pathlib import Path
from typing import Iterable, List, Optional

from openpyxl import Workbook

EXTRACTOR_DIR = Path(__file__).resolve().parent
DEFAULT_METRICS_FILE = EXTRACTOR_DIR / "metrics.txt"
RESULTS_BASE = Path.home() / "mgvm" / "results"

OUTPUT_FIELDS = ["run_id", "what", "benchmark", "where", "value"]
AGGREGATORS = {
    "first": lambda vs: vs[0],
    "last": lambda vs: vs[-1],
    "sum": lambda vs: sum(_floats(vs)),
    "mean": lambda vs: sum(_floats(vs)) / len(vs) if vs else "",
    "min": lambda vs: min(_floats(vs)),
    "max": lambda vs: max(_floats(vs)),
    "list": lambda vs: ";".join(str(v) for v in vs),
    "count": lambda vs: len(vs),
}


def _floats(values):
    out = []
    for v in values:
        try:
            out.append(float(v))
        except (TypeError, ValueError):
            pass
    return out


def _coerce(v):
    if isinstance(v, (int, float)):
        return v
    try:
        f = float(v)
    except (TypeError, ValueError):
        return v
    return int(f) if f.is_integer() and "." not in str(v) else f


def load_metrics(path: Path) -> List[str]:
    metrics: List[str] = []
    with open(path) as f:
        for line in f:
            line = line.split("#", 1)[0].strip()
            if line:
                metrics.append(line)
    return metrics


def _is_run_dir(p: Path) -> bool:
    return p.is_dir() and (p / "reports").is_dir()


def _run_label(p: Path) -> str:
    """Identifier shown in CLI output and the run_id column.
    New layout -> '<config>/<date>/<time>', old -> '<config>/<timestamp>'."""
    try:
        rel = p.resolve().relative_to(RESULTS_BASE.resolve())
        return str(rel)
    except ValueError:
        return p.name


def _run_ids(r: Path) -> set:
    """All string ids that should resolve exactly to a run dir."""
    parts = _run_label(r).split("/")
    ids = {"/".join(parts), parts[-1]}
    if len(parts) >= 3:                       # <config>/<date>/<time>
        cfg, date, time = parts[0], parts[-2], parts[-1]
        ids |= {f"{date}/{time}", f"{date}-{time}", f"{cfg}/{date}-{time}"}
    return ids


def _iter_run_dirs() -> Iterable[Path]:
    """New layout nests <config>/<date>/<time>; the run dir is the <time> leaf.
    Older <config>/<timestamp> runs are still picked up."""
    for cfg in RESULTS_BASE.iterdir():
        if not cfg.is_dir() or cfg.name.startswith("."):
            continue
        for child in cfg.iterdir():
            if not child.is_dir() or child.name.startswith("."):
                continue
            if _is_run_dir(child):            # old <config>/<timestamp>
                yield child
            else:                              # new <config>/<date>/<time>
                for leaf in child.iterdir():
                    if _is_run_dir(leaf):
                        yield leaf


def resolve_latest() -> Path:
    runs = list(_iter_run_dirs())
    if not runs:
        raise FileNotFoundError(f"No run dirs under {RESULTS_BASE}")
    runs.sort(key=lambda p: p.stat().st_mtime)
    return runs[-1]


def resolve_run_dir(arg: str) -> Path:
    p = Path(arg)
    if p.is_absolute() and p.is_dir():
        return p
    candidate = RESULTS_BASE / arg
    if candidate.is_dir() and _is_run_dir(candidate):
        return candidate
    if candidate.is_dir():  # arg is a config (or config/date) dir -> newest run
        runs = sorted((r for r in _iter_run_dirs() if candidate in r.parents),
                      key=lambda r: r.stat().st_mtime)
        if runs:
            return runs[-1]
    matches = [r for r in _iter_run_dirs()
               if arg in _run_ids(r) or arg in _run_label(r)]
    if len(matches) == 1:
        return matches[0]
    if len(matches) > 1:
        names = "\n  ".join(sorted(_run_label(m) for m in matches))
        raise ValueError(
            f"Ambiguous run id {arg!r}; multiple matches:\n  {names}\n"
            f"Pass it as '<config>/<date>/<time>' or the full directory name."
        )
    raise FileNotFoundError(
        f"No run dir for {arg!r}; tried absolute path, {RESULTS_BASE}/{arg}, "
        f"and substring match under {RESULTS_BASE}/<config>/<date>/<time>."
    )


def parse_benchmark_name(csv_filename: str) -> str:
    # File name shape: <bench>_<platform>_<xDim>x<yDim>.csv
    # Strip the trailing _<platform>_<x>x<y>.csv to recover bench (handles
    # benches with underscores like gpt3_175B_gemm).
    stem = csv_filename[:-4] if csv_filename.endswith(".csv") else csv_filename
    parts = stem.rsplit("_", 2)
    if len(parts) == 3 and "x" in parts[2]:
        return parts[0]
    return stem


def iter_metric_rows(csv_path: Path, metrics_set: set) -> Iterable[dict]:
    with open(csv_path, newline="") as f:
        reader = csv.reader(f)
        header = next(reader, None)
        if header is None:
            return
        header = [h.strip() for h in header]
        try:
            i_where = header.index("where")
            i_what = header.index("what")
            i_value = header.index("value")
        except ValueError:
            return
        for row in reader:
            if len(row) <= max(i_where, i_what, i_value):
                continue
            what = row[i_what].strip()
            if not what or what not in metrics_set:
                continue
            yield {
                "where": row[i_where].strip(),
                "what": what,
                "value": row[i_value].strip(),
            }


def extract_run(run_dir: Path, metrics_set: set, run_id: Optional[str] = None):
    csv_files = sorted((run_dir / "reports").glob("*.csv"))
    out_rows = []
    for csv_path in csv_files:
        bench = parse_benchmark_name(csv_path.name)
        for r in iter_metric_rows(csv_path, metrics_set):
            out_rows.append({
                "run_id": run_id or _run_label(run_dir),
                "benchmark": bench,
                "where": r["where"],
                "what": r["what"],
                "value": r["value"],
            })
    return out_rows, len(csv_files)


def fill_long(ws, rows, metrics_order, include_run_id: bool):
    fields = OUTPUT_FIELDS if include_run_id else [f for f in OUTPUT_FIELDS if f != "run_id"]
    ws.append(fields)
    metric_rank = {m: i for i, m in enumerate(metrics_order)}
    sorted_rows = sorted(
        rows,
        key=lambda r: (
            metric_rank.get(r["what"], len(metric_rank)),
            r["what"],
            r.get("run_id", ""),
            r["benchmark"],
            r["where"],
        ),
    )
    for r in sorted_rows:
        ws.append([_coerce(r[k]) for k in fields])


def fill_wide(ws, rows, metrics_order, aggregator, include_run_id: bool):
    agg = AGGREGATORS[aggregator]
    bucket: dict = {}
    for r in rows:
        key = (r["run_id"], r["benchmark"]) if include_run_id else (r["benchmark"],)
        bucket.setdefault(key, {}).setdefault(r["what"], []).append(r["value"])

    head = (["run_id"] if include_run_id else []) + ["benchmark"] + metrics_order
    ws.append(head)
    for key in sorted(bucket.keys()):
        row = list(key)
        for m in metrics_order:
            values = bucket[key].get(m, [])
            row.append(_coerce(agg(values)) if values else "")
        ws.append(row)


def _sanitize_for_filename(name: str) -> str:
    return "".join(c if c.isalnum() or c in "-_." else "_" for c in name)


def main():
    parser = argparse.ArgumentParser(
        description=__doc__,
        formatter_class=argparse.RawDescriptionHelpFormatter,
    )
    parser.add_argument(
        "runs", nargs="*",
        help="Run ids (relative paths like '<config>/<date>/<time>' or just "
             "'<date>-<time>' if unambiguous), or absolute paths. Multiple runs "
             "are concatenated with a run_id column.",
    )
    parser.add_argument(
        "--latest", action="store_true",
        help="Process the most-recently-modified run dir under "
             "~/mgvm/results/<config>/<date>/<time>/. Mutually exclusive with "
             "positional runs.",
    )
    parser.add_argument(
        "--metrics-file", default=str(DEFAULT_METRICS_FILE),
        help=f"Path to metrics list file used in default mode "
             f"(default: {DEFAULT_METRICS_FILE}).",
    )
    parser.add_argument(
        "--metrics", default=None,
        help="Switch to custom mode. Comma-separated metric names; output file "
             "is named after them. e.g. --metrics=simulation_time",
    )
    parser.add_argument(
        "--output", "-o", default=None,
        help="Output xlsx path. Default: "
             "extractor/<run-name>/{default | <metrics-joined>}.xlsx.",
    )
    parser.add_argument(
        "--aggregate", default="first", choices=sorted(AGGREGATORS.keys()),
        help="How to collapse multi-row metrics in the wide sheet "
             "(default: first). 'sum'/'mean'/'min'/'max' coerce values to "
             "float and skip non-numeric; 'list' joins with ';'.",
    )
    args = parser.parse_args()

    if args.latest and args.runs:
        parser.error("--latest cannot be combined with explicit run ids")
    if not args.latest and not args.runs:
        parser.error("specify at least one run id, or pass --latest")

    if args.metrics:
        mode = "custom"
        metrics = [m.strip() for m in args.metrics.split(",") if m.strip()]
    else:
        mode = "default"
        metrics_path = Path(args.metrics_file)
        if not metrics_path.is_file():
            parser.error(f"metrics file not found: {metrics_path}")
        metrics = load_metrics(metrics_path)
    if not metrics:
        parser.error("no metrics specified")
    metrics_set = set(metrics)

    print(f"[extractor] mode={mode}, metrics ({len(metrics)}): {metrics}",
          file=sys.stderr)

    run_dirs = [resolve_latest()] if args.latest else [resolve_run_dir(r) for r in args.runs]

    all_rows = []
    total_csvs = 0
    for run_dir in run_dirs:
        label = _run_label(run_dir)
        rows, n = extract_run(run_dir, metrics_set, run_id=label)
        all_rows.extend(rows)
        total_csvs += n
        print(f"[extractor] {label}: {n} csv files, "
              f"{len(rows)} matching rows", file=sys.stderr)

    print(f"[extractor] total: {total_csvs} csv files, {len(all_rows)} "
          f"matching rows across {len(run_dirs)} run(s)", file=sys.stderr)

    if not all_rows:
        print(f"[extractor] no matching metrics found for {metrics}; "
              f"skipping output file.", file=sys.stderr)
        return

    include_run_id = len(run_dirs) > 1

    if args.output:
        output_path = Path(args.output)
        output_path.parent.mkdir(parents=True, exist_ok=True)
    else:
        subdir_name = "+".join(_run_label(rd).replace("/", "-") for rd in run_dirs)
        out_dir = EXTRACTOR_DIR / subdir_name
        out_dir.mkdir(parents=True, exist_ok=True)
        if mode == "default":
            stem = "default"
        else:
            stem = _sanitize_for_filename("_".join(metrics))
        output_path = out_dir / f"{stem}.xlsx"

    wb = Workbook()
    ws_long = wb.active
    ws_long.title = "long"
    fill_long(ws_long, all_rows, metrics, include_run_id)
    ws_wide = wb.create_sheet("wide")
    fill_wide(ws_wide, all_rows, metrics, args.aggregate, include_run_id)
    wb.save(output_path)

    print(f"[extractor] wrote {output_path}", file=sys.stderr)


if __name__ == "__main__":
    main()
