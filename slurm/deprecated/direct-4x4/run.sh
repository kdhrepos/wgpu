#!/bin/sh

mkdir -p ./outputs
mkdir -p ./errors

python3 $HOME/mgvm/scripts/compile.py
python3 $HOME/mgvm/scripts/run.py \
    --platform=direct \
    --slurm-dir=direct-4x4 \
    --pte-caching=True \
    --xDim=4 \
    --yDim=4 \
    "$@"
