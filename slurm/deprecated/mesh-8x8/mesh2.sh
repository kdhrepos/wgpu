#!/bin/sh

#SBATCH -J mesh2
#SBATCH -o mesh2.out

#SBATCH -p batch

#SBATCH -N 1

#SBATCH --nodelist=compasslab3

python3 $HOME/mgvm/scripts/compile.py
python3 $HOME/mgvm/scripts/run2.py --parallel=9 --platform=mesh --log=True --xDim=8 --yDim=8 "$@"
