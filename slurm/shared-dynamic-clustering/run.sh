#!/bin/sh

# Dynamic-clustering with the SHARED PC table: an online hotness classifier
# whose per-home classification is synchronized across all chiplets at
# ZERO simulated overhead (a requestor reads the home chiplet's live
# hotness directly, no flit / no latency). Distinct from
# oracle-dynamic-clustering, which loads a static pre-computed JSON.
#
# Resolve the worktree this script lives in so compile.py / run.py target
# THIS worktree's tree. Result files still land in $HOME/mgvm/results.
SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
WORKTREE_ROOT="$(cd "$SCRIPT_DIR/../.." && pwd)"

mkdir -p ./outputs
mkdir -p ./errors

python3 "$WORKTREE_ROOT/scripts/compile.py" --pgo-platform dynamic-clustering-score

if [ $# -gt 0 ] && [ "${1#-}" = "$1" ]; then
    SIZE="$1"
    shift
else
    SIZE=6
fi

python3 "$WORKTREE_ROOT/scripts/run.py" \
    --platform=dynamic-clustering \
    --slurm-dir=shared-dynamic-clustering \
    --shared-pc-table=True \
    --pc-classifier=score \
    --xDim="$SIZE" \
    --yDim="$SIZE" \
    "$@"
