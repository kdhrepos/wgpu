#!/bin/sh

#SBATCH -J mesh2
#SBATCH -o mesh2.out

#SBATCH -p batch

#SBATCH -N 1

#SBATCH --nodelist=compasslab2

python3 $HOME/mgvm/scripts/compile.py

if [ $# -gt 0 ] && [ "${1#-}" = "$1" ]; then
    SIZE="$1"
    shift
else
    SIZE=6
fi

python3 $HOME/mgvm/scripts/run2.py --parallel=9 --platform=mesh --log=True --switch-latency=64 --xDim="$SIZE" --yDim="$SIZE" "$@"
