#!/bin/sh

mkdir -p ./outputs
mkdir -p ./errors

# First positional arg = wafer dim N (square NxN), default 6.
# If first arg starts with '-', treat it as a flag and use default size.
if [ $# -gt 0 ] && [ "${1#-}" = "$1" ]; then
    SIZE="$1"
    shift
else
    SIZE=6
fi

python3 $HOME/mgvm/scripts/compile.py
python3 $HOME/mgvm/scripts/run.py \
    --platform=direct \
    --slurm-dir=direct \
    --pte-caching=True \
    --xDim="$SIZE" \
    --yDim="$SIZE" \
    "$@"
