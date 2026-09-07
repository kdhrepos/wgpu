#!/bin/sh

mkdir -p ./outputs
mkdir -p ./errors

python3 $HOME/mgvm/scripts/compile.py
python3 $HOME/mgvm/scripts/run.py \
    --platform=mesh \
    --slurm-dir=mesh-4x4 \
    --small=True \
    "$@"
