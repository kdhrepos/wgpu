# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Purpose

**Simple value extractor**, separate from `analyze/`. Pulls a configurable set of `what` column values out of every benchmark CSV in a run directory and emits one combined table. No plotting, no statistics — just "for these metric names, give me the rows from every CSV in this run."

Use `analyze/` for plots, derived metrics, and figure generation. Use this when you just need the numbers.

## Input format

Each result CSV under `~/mgvm/results/reports/<run-id>/<bench>_<platform>_<x>x<y>.csv` has 3 columns: `where`, `what`, `value`. The leading rows are a "Parameters" metadata block where `what` is empty — those are skipped automatically. Real metric rows look like:

```
driver, simulation_time, 1.833804982006
GPU1.L2[3].Top, hit-rate, 0.7421
GPU1.CU[42], inst_count, 12345
```

## Files

- `extract.py` — the tool.
- `metrics.txt` — user-editable list of `what` values to extract (one per line, `#` comments OK, whitespace tolerated).

## Configuring metrics (the "easy" knob)

The intended workflow is: **open `metrics.txt` in any text editor, add or remove metric names, save, run `extract.py`**. Nothing more. The first line of the file shows the format; the default has a starter set with more commented-out examples.

For one-off queries without editing the file, pass `--metrics` on the CLI:
```bash
python3 extract.py <run-id> --metrics=simulation_time,inst_count,hit-rate
```

## Typical invocations

```bash
# Long format (one row per matching (benchmark, where, what)), default metrics.txt
python3 extract.py 20260516-181337-dynamic-clustering-4x4-oracle-small

# Wide pivot: one row per benchmark, one column per metric
python3 extract.py <run-id> --wide

# Most-recent run, just print to stdout
python3 extract.py --latest --wide

# Multi-run comparison — output gains a `run_id` column automatically
python3 extract.py <run-A> <run-B> --wide --output compare.csv

# Aggregate when a `what` appears many times (e.g. per-CU inst_count)
python3 extract.py <run-id> --wide --aggregate=sum
```

Aggregators (only used in `--wide` mode): `first` (default), `last`, `sum`, `mean`, `min`, `max`, `list` (join with `;`), `count`. `sum`/`mean`/`min`/`max` coerce values to float and silently drop non-numeric rows.

## Run ID resolution

Positional args accept:
- An exact `<config>/<timestamp>` under `~/mgvm/results/` (e.g. `mesh-6x6/20260601-235841`)
- A config name (e.g. `mesh-6x6`) — resolves to that config's newest run
- A bare `<timestamp>` — auto-resolved under any config when unambiguous
- An absolute path
- A substring — fuzzy-matched against `<config>/<timestamp>`. If multiple match, the error lists all candidates so you can pick.

`--latest` picks the most-recently-modified run dir across all configs. Mutually exclusive with positional args.

## What this does NOT do

- No regex / glob on `what` names. Add the exact name to `metrics.txt`. (Add globbing later if needed.)
- No filtering by `where`. Both rows like `driver, kernel_time` and `GPU1.CommandProcessor, kernel_time` come through — they differ on `where`. In `--wide` mode the aggregator collapses them.
- No statistics, no plotting, no derived metrics. Use `analyze/` for those.

## Conventions

- Stdlib-only (`csv`, `argparse`, `pathlib`). No pandas dependency — runs anywhere Python 3 runs.
- Output is always RFC-4180 CSV via `csv.writer`. Pipe to `column -ts,` or load with pandas if you want a prettier view.
- Stderr carries progress / debug info (`[extractor] ...`); stdout is the data. Safe to redirect with `> file.csv`.
