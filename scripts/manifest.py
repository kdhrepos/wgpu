"""Sweep manifest: a JSON snapshot of what a sweep submission intended to run.

Written by run.py before any sbatch call so the manifest exists even when
submission fails. Captures the full simulator command line per benchmark plus
all sweep-level options, so a result directory can be matched back to its
slurm sweep configuration after the fact.

v1: submit-time only. Per-benchmark success/failure tracking is out of scope.
v2: result_dir points to the run-root directory `<config>/<timestamp>`;
    subdirs (reports/, memory-traces/, ...) live under it. Manifest itself
    is written at `<result_dir>/manifest.json`.
"""

import json
import os
from dataclasses import dataclass, field, asdict
from typing import List, Optional

SCHEMA_VERSION = 2
MANIFEST_FILENAME = "manifest.json"
SLURM_MANIFEST_SUBDIR = "manifests"


@dataclass
class BenchmarkEntry:
    name: str
    binary_name: str
    alias: str
    cmd: str
    expected_csv: str
    slurm_out: str
    slurm_err: str
    sbatch_jobname: str
    # Optional shell snippet run after `cmd` in the same sbatch job. Reserved
    # for post-run housekeeping; unused now that checkpoint CSVs are written
    # directly to their final destinations. Default empty.
    post_cmd: str = ""


@dataclass
class Manifest:
    schema_version: int = SCHEMA_VERSION
    submitted_at: str = ""
    submitter: str = ""
    run_id: str = ""
    result_dir: str = ""
    slurm_dir: str = ""
    comment: str = ""
    args: dict = field(default_factory=dict)
    result_dirs: dict = field(default_factory=dict)
    base_options: str = ""
    benchmarks: List[BenchmarkEntry] = field(default_factory=list)

    def write(self, path: str) -> None:
        os.makedirs(os.path.dirname(path), exist_ok=True)
        with open(path, "w") as f:
            json.dump(asdict(self), f, indent=2)

    @classmethod
    def load(cls, path: str) -> "Manifest":
        with open(path) as f:
            data = json.load(f)
        benches = [BenchmarkEntry(**b) for b in data.pop("benchmarks", [])]
        m = cls(**data)
        m.benchmarks = benches
        return m


def manifest_path_in(result_dir: str) -> str:
    return os.path.join(result_dir, MANIFEST_FILENAME)


def slurm_backlink_path(slurm_dir: str, run_id: str) -> str:
    return os.path.join(slurm_dir, SLURM_MANIFEST_SUBDIR, f"manifest_{run_id}.json")
