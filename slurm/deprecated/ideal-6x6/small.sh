#!/bin/sh

mkdir -p ./outputs
mkdir -p ./errors

python3 $HOME/mgvm/scripts/compile.py
python3 $HOME/mgvm/scripts/run.py \
    --platform=ideal \
    --slurm-dir=ideal-6x6 \
    --small=True \
    --xDim=6 \
    --yDim=6 \
    "$@"
