#!/bin/sh

# Resolve the worktree this script lives in (e.g. /…/bypass-…-v2)
# so compile.py / run.py invocations target THIS worktree's tree.
# Result files still land in $HOME/mgvm/results (the main worktree)
# per run.py.
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
    --slurm-dir=oracle-dynamic-clustering \
    --oracle-pc-table=True \
    --xDim="$SIZE" \
    --yDim="$SIZE" \
    "$@"
