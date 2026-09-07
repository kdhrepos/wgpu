#!/bin/sh

# Resolve the worktree this script lives in (e.g. /…/bypass-hash-inter-cluster-v3)
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

# Score-classifier weight defaults — tuned against oracle labels on
# t_pc_stats_l2miss_pte_per_owner_1.xlsx.  Override via env vars
# (e.g. W_HOP=0.5 ./run.sh 6) without editing this script.
W_REUSE=${W_REUSE:-0.40}
W_SHARE=${W_SHARE:-0.30}
W_HOP=${W_HOP:-0.30}
WARMUP_SHRINK=${WARMUP_SHRINK:-0.7}
HOP_REF=${HOP_REF:-0}     # 0 = auto (mesh diameter)

python3 "$WORKTREE_ROOT/scripts/run.py" \
    --platform=dynamic-clustering \
    --slurm-dir=dynamic-clustering-score \
    --pc-classifier=score \
    --pc-score-w-reuse="$W_REUSE" \
    --pc-score-w-share="$W_SHARE" \
    --pc-score-w-hop="$W_HOP" \
    --pc-score-hop-ref="$HOP_REF" \
    --pc-score-warmup-shrink="$WARMUP_SHRINK" \
    --xDim="$SIZE" \
    --yDim="$SIZE" \
    "$@"
