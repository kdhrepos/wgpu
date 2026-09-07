#!/bin/sh

mkdir -p ./outputs
mkdir -p ./errors

python3 $HOME/mgvm/scripts/compile.py

if [ $# -gt 0 ] && [ "${1#-}" = "$1" ]; then
    SIZE="$1"
    shift
else
    SIZE=6
fi

python3 $HOME/mgvm/scripts/run.py \
    --platform=mesh \
    --slurm-dir=data-alloc \
    --log-data-allocation=True \
    --xDim="$SIZE" \
    --yDim="$SIZE" \
    "$@"
