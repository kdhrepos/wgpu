import pandas as pd
import os

benchmarks = [
    # High-sharing Apps
    'bfs',
    'gemm',
    'lu',
    'spmv',
    'mm3',
    'mm2',
    'matrixmultiplication',
    'syr2k',
    'gups',    
    'mis',
    'matrixtranspose',
    'stencil2d',
    'gaussian',
    'im2col',
    'vgg16',

    # Low-sharing Apps
    'fdtd2d',
    'fastwalshtransform',
    'jacobi1d',
    'relu',
    'color',
    'convolution2d',
    'sssp',
    'fir',
    'fft',
    'bitonicsort',
    'pagerank',
]

if __name__=="__main__":
    username = os.path.expanduser('~')
    date = '20260221-223953'
    base_dir = f'{username}/mgvm/results/memory-traces'
    expanded_dir = f"{base_dir}/{date}/expanded"

    row = []

    for benchmark in benchmarks:
        trace_file = f"{expanded_dir}/{benchmark}_mesh_6x6_memory_trace.csv"

        if not os.path.exists(trace_file):
            print(f"No {trace_file}, skip")
            continue
        else:
            print(f"Processing {benchmark}")

        df = pd.read_csv(trace_file, skipinitialspace=True)

        unique_pcs = df[['pc']].drop_duplicates()
        unique_pcs['lsb8'] = unique_pcs['pc'] & 0xFF

        counts_per_lsb = unique_pcs.groupby('lsb8')['pc'].count()
        collisions = counts_per_lsb[counts_per_lsb > 1]

        total_collided_pcs = collisions.sum()
        num_collisions = len(collisions)

        row.append({
            'benchmark': benchmark,
            'pc_count': len(unique_pcs),
            'pc_lsb8_count': unique_pcs['lsb8'].nunique(),
            'total_collided_pcs': total_collided_pcs,
        })

    result_df = pd.DataFrame(row)
    result_df.to_csv(f"./lsb8_pc.csv", index=False)