#!/bin/sh

#SBATCH -J mesh
#SBATCH -o mesh.out

#SBATCH -p batch

#SBATCH -N 1

#SBATCH --nodelist=compasslab2

python3 $HOME/mgvm/scripts/compile.py
python3 $HOME/mgvm/scripts/run.py --parallel=8 --platform=mesh --log=True --xDim=8 --yDim=8 "$@"
