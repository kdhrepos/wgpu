#!/bin/sh

mkdir -p ./outputs
mkdir -p ./errors

python3 $HOME/mgvm/scripts/compile.py
python3 $HOME/mgvm/scripts/run.py \
    --platform=nearfetch \
    --slurm-dir=nearfetch-6x6 \
    --xDim=6 \
    --yDim=6 \
    --small=True \
    --log=True \
    "$@"
