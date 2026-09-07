#!/bin/sh

mkdir -p ./outputs
mkdir -p ./errors

python3 $HOME/mgvm/scripts/compile.py
python3 $HOME/mgvm/scripts/run.py \
    --platform=direct \
    --slurm-dir=direct-6x6 \
    --pte-caching=True \
    --xDim=6 \
    --yDim=6 \
    "$@"
