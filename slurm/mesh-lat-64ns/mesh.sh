#!/bin/sh

#SBATCH -J mesh
#SBATCH -o mesh.out

#SBATCH -p batch

#SBATCH -N 1

#SBATCH --nodelist=compasslab1

python3 $HOME/mgvm/scripts/compile.py

if [ $# -gt 0 ] && [ "${1#-}" = "$1" ]; then
    SIZE="$1"
    shift
else
    SIZE=6
fi

python3 $HOME/mgvm/scripts/run.py --parallel=8 --platform=mesh --log=True --switch-latency=64 --xDim="$SIZE" --yDim="$SIZE" "$@"
