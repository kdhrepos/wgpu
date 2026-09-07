#!/bin/sh

# Resolve the worktree this script lives in (e.g. /…/some-worktree)
# so compile.py / run.py invocations target THIS worktree's tree.
# Result files still land in $HOME/mgvm/results (the main worktree)
# per run.py.
SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
WORKTREE_ROOT="$(cd "$SCRIPT_DIR/../.." && pwd)"

mkdir -p ./outputs
mkdir -p ./errors

if [ $# -gt 0 ] && [ "${1#-}" = "$1" ]; then
    SIZE="$1"
    shift
else
    SIZE=6
fi

python3 "$WORKTREE_ROOT/scripts/compile.py"
python3 "$WORKTREE_ROOT/scripts/run.py" \
    --platform=ideal \
    --slurm-dir=ideal \
    --xDim="$SIZE" \
    --yDim="$SIZE" \
    "$@"
