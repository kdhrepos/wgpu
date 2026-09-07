#!/bin/sh

mkdir -p ./outputs
mkdir -p ./errors

python3 $HOME/mgvm/scripts/compile.py
python3 $HOME/mgvm/scripts/run.py \
    --slurm-dir=mix-6x6 \
    --mix=True \
    --xDim=6 \
    --yDim=6 \
    --memory-trace=True \
    "$@"
