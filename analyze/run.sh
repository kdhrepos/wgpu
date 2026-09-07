#!/usr/bin/env bash
# Run any analyze script with the whole analyze tree (+ ../scripts) on PYTHONPATH,
# so cross-domain imports (formatter, _paths, *_plotter, params) resolve regardless
# of which domain folder the script lives in. Module names are globally unique, so
# flattening every domain dir onto sys.path at runtime is collision-free.
#
#   Usage: analyze/run.sh <domain>/<script>.py [args...]
#   e.g.:  analyze/run.sh pc/pc_rdma_metrics_fig.py mesh-6x6/latest
set -euo pipefail
HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PP="$(find "$HERE" -type d \( -name deprecated -o -name _out -o -name __pycache__ -o -name .claude -o -name .git -o -name figure-generator \) -prune -o -type d -print | tr '\n' ':')"
export PYTHONPATH="${PP}${HERE}/../scripts${PYTHONPATH:+:$PYTHONPATH}"
script="$1"; shift
exec python3 "$HERE/$script" "$@"
