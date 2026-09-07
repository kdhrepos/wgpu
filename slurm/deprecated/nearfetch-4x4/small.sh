#!/bin/sh

mkdir -p ./outputs
mkdir -p ./errors

python3 $HOME/mgvm/scripts/compile.py
python3 $HOME/mgvm/scripts/run.py \
    --platform=nearfetch \
    --slurm-dir=nearfetch-4x4 \
    --xDim=4 \
    --yDim=4 \
    --small=True \
    "$@"
