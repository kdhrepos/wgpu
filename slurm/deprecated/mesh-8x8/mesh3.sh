#!/bin/sh

#SBATCH -J mesh3
#SBATCH -o mesh3.out

#SBATCH -p batch

#SBATCH -N 1

#SBATCH --nodelist=compasslab4

python3 $HOME/mgvm/scripts/compile.py
python3 $HOME/mgvm/scripts/run3.py --parallel=12 --platform=mesh --log=True --xDim=8 --yDim=8 "$@"
