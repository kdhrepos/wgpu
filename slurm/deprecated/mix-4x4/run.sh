#!/bin/sh

mkdir -p ./outputs
mkdir -p ./errors

python3 $HOME/mgvm/scripts/compile.py
python3 $HOME/mgvm/scripts/run.py \
    --slurm-dir=mix-4x4 \
    --mix=True \
    --xDim=4 \
    --yDim=4 \
    --memory-trace=True \
    "$@"
