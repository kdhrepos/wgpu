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

    ratio = 0.1
    total_addrs = len(df)
    top_n_addrs = round(ratio*total_addrs) 
    top_n_access_count = df.sort_values(by=' count', ascending=False)[:top_n_addrs][' count'].sum()/df[' count'].sum()

    print(f"Benchmark: {benchmark}")
    print(f'Top {ratio*100}% access count ratio: {top_n_access_count:.4f}')
    print("\n")