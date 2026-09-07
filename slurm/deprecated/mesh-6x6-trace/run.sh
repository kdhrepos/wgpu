#!/bin/sh

mkdir -p ./outputs
mkdir -p ./errors

python3 $HOME/mgvm/scripts/compile.py
python3 $HOME/mgvm/scripts/run.py \
    --platform=mesh \
    --slurm-dir=mesh-6x6-trace \
    --xDim=6 \
    --yDim=6 \
    --log=True \
    --memory-trace=True \
    "$@"
