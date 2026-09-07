import pandas as pd
import matplotlib.pyplot as plt
import os

from _paths import access_count_dir

username = os.path.expanduser('~')

benchmarks = [
    # High-sharing Apps
    # 'gups',    
    # 'syrk',
    # 'syr2k',
    # 'spmv',
    # 'mis', # Deadlock
    # 'jacobi2d',
    # 'stencil2d',
    # 'gemm',
    # 'lu',
    # 'matrixmultiplication',
    # 'mm2',
    # 'mm3',
    # 'gaussian',
    # 'matrixtranspose', # Error
    # 'b+tree',

    # # Low-sharing Apps
    # # 'fdtd2d',
    # 'fastwalshtransform',
    # 'jacobi1d',
    # 'relu',
    'pagerank',
    # 'shoc-reduction',
    # 'bfs',
    # # 'nn',
    # # 'hotspot3d',
    # # 'srad',
    # 'color',
    # 'convolution2d',
    # 'sssp',
]

result_dir=f'{username}/mgvm/analyze/_out/access-count/access-counts'
print(result_dir)

for benchmark in benchmarks:
    input = f'{access_count_dir("20260130-040704")}/{benchmark}_mesh_6x6_access_count.csv'

    df = pd.read_csv(input)

    plt.figure(figsize=(10, 6)) 
    plt.plot(df[' address'], df[' count'], 'o', label='Count per Address', markersize=4)

    plt.title('Access count per address', fontsize=14)
    plt.xlabel('Address', fontsize=12)
    plt.ylabel('Count', fontsize=12)
    plt.grid(True, linestyle='--', alpha=0.6)

    plt.ticklabel_format(style='plain', axis='x')
    plt.xticks(rotation=45)

    plt.legend()
    plt.tight_layout()

    plt.savefig(f'{result_dir}/{benchmark}.png', dpi=400)