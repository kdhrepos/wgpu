#!/bin/sh

mkdir -p ./outputs
mkdir -p ./errors

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
    --small=True \
    --xDim="$SIZE" \
    --yDim="$SIZE" \
    "$@"
