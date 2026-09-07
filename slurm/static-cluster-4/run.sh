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
    --platform=static-cluster \
    --cluster-size=4 \
    --slurm-dir=static-cluster-4 \
    --memory-trace=True \
    --log=True \
    --xDim="$SIZE" \
    --yDim="$SIZE" \
    "$@"
