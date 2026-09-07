#!/bin/sh

mkdir -p ./outputs
mkdir -p ./errors

# python3 $HOME/mgvm/scripts/compile.py
python3 $HOME/mgvm/scripts/run.py \
    --platform=dynamic-clustering \
    --slurm-dir=oracle-dynamic-clustering \
    --oracle-pc-table=True \
    --xDim=6 \
    --yDim=6 \
    --small=True \
    "$@"
