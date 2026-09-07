# Parameters for benchmarks.

inst_10M = 10000000
inst_30M = 30000000
inst_100M = 100000000
inst_1B = 1000000000

benchmarks = [
    'gups',
    'syr2k',
    "xsbench",
    "cc",
    'spmv',
    'mis',
    "pagerank",
    'mst',
    'mixtral_ffn',
    'llama70b_ffn',

    'spmm',
    # "bptree",

    # 'lud',
    # 'fr',

    # 'gpt3_175B_gemm',
    # 'llama3_70B_gemm',
    # 'gpt3_175B_mm',
    # 'llama3_70B_mm',
    # 'kmeans',
    # 'atax',
    # 'mvt',
    # 'bicg',
    # 'matrixtranspose',

    # "tc",
    # "fr",
    # "mst",
    # "color",
    # "sssp",
    # "bc",
    # "kcore",

    # "particlefilter",
    # "radixsort",
    # "scan",
    # "histogram",

    # 'particlefilter',
    # "embeddingbag_zipf",
    # "streamcluster",

    # "relu",
    # "fir",
    # "fft",
    # "jacobi1d",
    # "simpleconvolution",
    # "fastwalshtransform",

    # 'lenet',
    # 'bfs',
    # 'nn',

    # 'gaussian',
    # "fdtd2d",
    # "convolution2d",
    # "bitonicsort",
    # 'stencil2d',
]
    

benchmark_configs = {
    # square matrix
    'gemm': {
        'sched-partition': 'Ydiv',
        'data-partitioning': 'row-based,col-based,stride-aware',
        'data-strides': "0,65536,0", # a, b, c
        'params': '-i=16384 -j=16384 -k=16384', # ~= 3GB
        'max_inst': inst_100M*5,
        # 'max_inst': inst_100M,
    },
    'matrixmultiplication': {
        'sched-partition': 'Ydiv',
        'data-partitioning': 'row-based,col-based,stride-aware',
        'data-strides': "0,49152,0", # a, b, c
        'params': '-x=12288 -y=12288 -z=12288', # ~= 2.1GB
        'max_inst': inst_100M*5,
        # 'max_inst': inst_100M*3,
    },

    # High-sharing Apps
    'gups': {
        'sched-partition': 'Xdiv',
        'data-partitioning': 'kernel-wide', # ITL
        'params': '-thread-block-size=64 -nthread-blocks=8192 -table-param=512', # ~= 1GB
        'max_inst': inst_10M,
    },
    'syr2k': {
        # FOLLOW LASP: keep the original row,row,col partition (row-based a,b is
        # the locality-optimal LASP choice; kernel-wide would be anti-LASP). The
        # only knob is SIZE: raise NJ to grow the reused A/B rows past L2 while
        # NI (=output C=NI^2) stays bounded so streamed C can't dilute. Sharing
        # stays at LASP's reqC (~18, X-dim), just larger hot volume.
        'sched-partition': 'Ydiv',
        'data-partitioning': 'row-based,row-based,row-based', # a, b, c  (LASP, unchanged)
        'data-strides': "0,0,0", # a, b, c
        'params': '-ni=8192 -nj=8192', # A,B=512MB each (spill L2), C=64MB  (was 8192/8192)
        'max_inst': inst_100M,
    },
    'llama70b_ffn': {  # K=8192 (Llama-70B hidden), N=27648 (~=28672 FFN, -3.6%), M=256
        'name': 'gemm', 
        'alias': 'llama70b_ffn',
        'sched-partition': 'Xdiv',
        'data-partitioning': 'row-based,col-based,col-based',
        # 'data-strides': "0,110592,110592", # b=c=N*4=27648*4
        # 'params': '-i=256 -j=27648 -k=8192', # A=8MB, B=906MB, C=28MB
        'data-strides': "0,114688,114688", # b=c=N*4=28672*4
        'params': '-i=256 -j=28672 -k=8192', # A=8MB, B=906MB, C=28MB
        'max_inst': inst_1B,
    },
    'mixtral_ffn': {  # Mixtral 8x22B expert FFN: K=6144 (hidden), N=16384 (real), M=336
        'name': 'gemm', 
        'alias': 'mixtral_ffn',
        'sched-partition': 'Xdiv',
        'data-partitioning': 'row-based,col-based,col-based',
        'data-strides': "0,65536,65536", # b=c=N*4=16384*4
        'params': '-i=336 -j=16384 -k=6144', # A(shared)=8.3MB, B=396MB, C=22MB
        'max_inst': inst_1B,
    },
    'gpt3_175B_gemm': {
        'name': 'gemm',
        'alias': 'gpt3_175B_gemm',
        'sched-partition': 'Xdiv',
        'data-partitioning': 'row-based,col-based,col-based', # a, b, c
        'data-strides': "0,196608,196608", # a, b, c
        'params': '-i=2048 -j=49152 -k=12288', # ~= 2.7GB
        'max_inst': inst_100M*3,
        # 'max_inst': inst_100M,
    },
    'gpt3_175B_mm': {
        'name': 'matrixmultiplication',
        'alias': 'gpt3_175B_mm',
        'sched-partition': 'Xdiv',
        'data-partitioning': 'row-based,col-based,col-based', # a, b, c
        'data-strides': "0,196608,196608",  # a, b, c
        'params': '-x=12288 -y=2048 -z=49152', # ~= 2.7GB
        # 'max_inst': inst_100M*5,
        'max_inst': inst_1B,
    },
    'llama3_70B_gemm': {
        'name': 'gemm',
        'alias': 'llama3_70B_gemm',
        'sched-partition': 'Xdiv',
        'data-partitioning': 'row-based,col-based,col-based',
        'data-strides': "0,114688,114688", # a, b, c
        'params': '-i=2048 -j=28672 -k=8192', # ~= 1.1GB
        # 'max_inst': inst_100M,
        'max_inst': inst_100M*3,
    },
    'llama3_70B_mm': {
        'name': 'matrixmultiplication',
        'alias': 'llama3_70B_mm',
        'sched-partition': 'Xdiv',
        'data-partitioning': 'row-based,col-based,col-based',
        'data-strides': "0,114688,114688", # a, b, c
        'params': '-x=8192 -y=2048 -z=28672', # ~= 1.1GB
        'max_inst': inst_1B,
        # 'max_inst': inst_100M*5,
    },
    'gaussian': { # Rodinia Gaussian elimination (Fan1, Fan2).
        # Algorithm: per outer iteration t = 0..size-2,
        #   Fan1: write column t of M (size-1-t threads, 1D).
        #   Fan2: update active submatrix; every WG reads the SAME
        #         pivot row A[t, :], so that row is cross-chiplet shared.
        # Per-iteration the pivot row is fresh (size-t bytes / iter),
        # cumulative pivot-row traffic ~= size^2 * 2 bytes.
        #
        # Sizing for 6x6 = 36 chiplets, ~1.7 GB total HBM footprint:
        #   - size=12288 -> each of M, A, B = size^2 * 4 = 576 MB,
        #     3 buffers -> ~1.7 GB total.
        #   - Cumulative pivot-row broadcast traffic ~= 288 MB across
        #     12287 outer iterations (each iteration adds (size-t)*4 B
        #     of fresh data fetched cross-chiplet on mesh).
        #   - Xdiv on Fan2 + row-based M/A/B aligns local-write rows
        #     with each chiplet's globalIdx range; only the pivot row
        #     crosses chiplets (one per iteration).
        # max_inst budget: t=0 Fan2 alone is ~6e7 WIs * ~30 inst ~= 2 GI;
        # inst_1B*3 -> covers iter 0 plus a few more before the matrix
        # shrinks (later iters are cheaper).
        'sched-partition': 'Xdiv, Xdiv',  # Fan1, Fan2
        'data-partitioning': 'row-based,row-based,row-based',  # M, A, B (alloc order in initMem)
        'params': '-size=12288', # 576MB x 3 = 1.7GB
        'max_inst': inst_100M,
    },
    'vgg16': {
        'sched-partition': "Xdiv",
        'params': ' -epoch=10 -batch-size=32 ',
        'max_inst': inst_100M,
    },
    'lenet': {
        'sched-partition': "Xdiv",
        'params': ' -epoch=10 -batch-size=256 ',
        'max_inst': 657032366,
    },
    'spmv': {
        'sched-partition': 'Xdiv',
        'data-partitioning': 'kernel-wide', # ITL
        'params': '-dim=4194304 -sparsity=0.00001 -zipf-s=0.8', # FP:1.46G nnz=176M deg42; x-vec=16.8MB (4MB<x<36MB); zipf spread
        'max_inst': inst_10M*3,
    },
    'mis': {
        'sched-partition': 'Xdiv',
        'data-partitioning': 'kernel-wide', # ITL
        'params': '-numNodes=4194304 -numItems=134217728 -zipf-s=1.0', # d3+d6=16.8MB×2=33.6MB (<36MB cluster); 축소로 cluster-fit
        'max_inst': inst_10M*6,
    },
    'matrixtranspose': { # No Locality
        'sched-partition': 'Ydiv',
        'data-partitioning': 'row-based,col-based',
        'data-strides': "0,32768", # input, output
        'col-size-for-lasp': 8192,
        'params': '-width=8192', # 512MB
        'max_inst': 38273024,
    },
    'bfs': { # ITL
        'sched-partition': 'Xdiv',
        'params': '-node=4194304 -degree=64 -depth=3', # ~= 1.1GB, 65536 WGs
        'max_inst': inst_100M*5,
        # 'max_inst': 116624035,
    },
    'kmeans': { # ITL
        'sched-partition': 'Xdiv',
        'params': '-points=1048576 -features=64 -clusters=20 -max-iter=1', # 512MB
        'max_inst': inst_100M, 
    },
    'lud': { # No Locality
        'sched-partition': 'Xdiv, Xdiv, Ydiv',
        'params': '-matrix-dim=12288', # 576MB
        # 'params': '-matrix-dim=24576', # ~2.3GB
        'max_inst': inst_100M*5,
    },
    'atax': { # RCL
        'sched-partition': 'Xdiv, Xdiv',
        'data-partitioning': 'row-based',
        'params': ' -x=16384 -y=16384 ', # 1GB
        'max_inst': inst_100M,
    },
    'mvt': { # RCL
        'sched-partition': 'Xdiv, Xdiv',
        'data-partitioning': 'row-based',
        'params': ' -n=16384 ', # 1GB
        'max_inst': inst_100M,
    },
    'bicg': {# RCL
        'sched-partition': 'Xdiv, Xdiv',
        'data-partitioning': 'row-based',
        'params': ' -x=16384 -y=16384', # 1GB
        'max_inst': inst_100M,
    },
    'pagerank': { # ITL
        'sched-partition': 'Xdiv',
        'params': '-node=6291456 -sparsity=0.00001 -iterations=1 -zipf-s=0.8', # rank 25.2MB deg63; col~1.58GB
        'max_inst': inst_100M,
    },
    'spmm': {
        'sched-partition': 'Xdiv',
        'data-partitioning': 'kernel-wide',
        # csrmv_batched reads B column-wise: denseB[k*ldB + curr_col], stride n*4 > 64B.
        # So ONE column pass touches exactly 1 line per row => touched B = Dim*64B,
        # independent of N. curr_col is the OUTER loop and one pass costs nnz(=Dim^2*sparsity)
        # work, so raising max_inst cannot reach the next line (needs 16 full passes).
        # Dim is therefore the only footprint lever: Dim=262144 -> touched B = 16.8MB
        # (4MB < 16.8 < 36MB cluster). sparsity set to keep deg=Dim*sparsity~786 (nnz~206M).
        # zipf-s LOW(0.2): columns near-uniform -> B uniformly hot, not collapsed to hub rows.
        'params': '-dim=262144 -sparsity=0.003 -n=96 -zipf-s=0.2',
        'max_inst': inst_10M*5,
    },

    # Low-sharing Apps
    'fdtd2d': {
        'sched-partition': 'Ydiv',
        'params': '-nx=4096 -ny=4096', # 192MB
    },
    'fastwalshtransform': {
        'sched-partition': 'Xdiv',
        'params': '-length=134217728', # 512MB
        'max_inst': inst_100M,
    },
    'jacobi1d': {
        'sched-partition': 'Xdiv',
        'params': '-n=33554432 -steps=1', # 256MB
        'max_inst': 40894465,
    },
    'relu': {
        'sched-partition': 'Xdiv',
        'params': '-n=67108864', # = 512MB
        'max_inst': inst_100M,
    },
    'color': {
        'sched-partition': 'Xdiv',
        'params': '-num-nodes=8388608 -num-edges=134217728 -zipf-s=1.0', # deg16; FP~=1.09GB
        'max_inst': inst_100M,
    },
    'convolution2d': {
        'sched-partition': 'Ydiv',
        'params': '-ni=8192 -nj=8192', # 512MB
    },
    'sssp': {
        'sched-partition': 'Xdiv',
        'params': '-num-nodes=8388608 -num-items=134217728 -zipf-s=1.0', # deg16; FP~=1.09GB
        'max_inst': inst_100M*2,
    },
    'fft': {
        'sched-partition': 'Xdiv',
        'params': ' -MB=512 -passes=1 ',
        'max_inst': inst_100M*2,
    },
    'fir': {
        'sched-partition': 'Xdiv',
        'params': ' -length=33554432 ', # 256MB
        'max_inst': inst_100M*2,
    },
    'bitonicsort': {
        'sched-partition': 'Xdiv',
        'params': ' -length=33554432 ', # 256MB
    },
    'simpleconvolution': {
        'sched-partition': 'Xdiv',
        'params': ' -height=8190 -width=8190 ', # 512MB
        'max_inst': inst_100M,
    },
    'stencil2d': { # No Locality
        'sched-partition': 'Xdiv',
        'params': '-row=8192 -col=8192', # 512MB
        'max_inst': 320143360,
    },

    # ------------------------------------------------------------------------
    # Newly ported (commit 6db6b1b) — irregular / high-inter-WG-sharing apps.
    # Each benchmark uses a single LASP allocation category ("div4" in
    # benchmark.go), so 'data-partitioning' takes ONE token; 'kernel-wide'
    # interleaves every device buffer across all chiplets at page granularity,
    # which is what drives the cross-chiplet sharing we want here.
    # Working-set is held to ~150-512MB so it fits comfortably even at
    # 16-chiplet (64GB) while still saturating bandwidth at 64-chiplet (256GB).
    # max_inst is bounded so each job completes within a few hours of sim time.
    # ------------------------------------------------------------------------
    'bc': { # Betweenness Centrality (Brandes 2-phase BFS).
        # Sharing: every chiplet atomic-adds into shared rho/sigma/bc float
        # arrays during forward BFS and backward dependency sweep.
        # LASP per-kernel (launch order): cleanInt(d), cleanFloat(rho),
        # cleanFloat(sigma), bfs_kernel, backtrack_kernel.
        # LASP per-buffer (alloc order):  row, col, d, rho, sigma, bc, cont.
        'sched-partition': 'Xdiv,Xdiv,Xdiv,Xdiv,Xdiv',
        'data-partitioning': 'kernel-wide,kernel-wide,kernel-wide,kernel-wide,kernel-wide,kernel-wide,kernel-wide',
        # Scaled 8x (avg degree 8) for wafer-scale: col=256M*4=1GB + 5 per-node
        # float arrays (32M*4 each) ≈ 640MB  -> ~1.65GB footprint.
        'params': '-num-nodes=8388608 -num-edges=268435456 -num-src=4 -zipf-s=1.0', # deg32; FP~=1.16GB
        'max_inst': inst_100M,
    },
    'cc': { # Connected Components (Shiloach-Vishkin union-find).
        # Sharing: all WGs atomic-CAS into the shared comp[] label array.
        # LASP per-kernel (launch order): hook_kernel, compress_kernel.
        # LASP per-buffer (alloc order):  row, col, comp, changed.
        'sched-partition': 'Xdiv,Xdiv',
        'data-partitioning': 'kernel-wide,kernel-wide,kernel-wide,kernel-wide',
        # Scaled 8x (avg degree 8) for wafer-scale: col=256M*4=1GB + row/comp
        # (32M*4 each) ≈ 256MB -> ~1.3GB footprint.
        # 'params': '-num-nodes=33554432 -num-edges=268435456', # ~1.3GB
        'params': '-num-nodes=4194304 -num-edges=268435456 -zipf-s=1.0', # deg64; FP~=1.03GB
        'max_inst': inst_100M*5,
    },
    'kcore': { # K-Core Decomposition (iterative peeling, K=16).
        # Sharing: per-iteration atomic decrements on a shared degree[] array
        # across all chiplets; many short rounds.
        # LASP per-kernel (launch order): init_degree_kernel, peel_kernel,
        # update_degree_kernel.
        # LASP per-buffer (alloc order):  row, col, active, degree,
        # to_remove, changed.
        'sched-partition': 'Xdiv,Xdiv,Xdiv',
        'data-partitioning': 'kernel-wide,kernel-wide,kernel-wide,kernel-wide,kernel-wide,kernel-wide',
        # Scaled 8x (avg degree 8) for wafer-scale: col=256M*4=1GB + 4 per-node
        # int arrays (32M*4 each) ≈ 512MB -> ~1.5GB footprint.
        'params': '-num-nodes=8388608 -num-edges=268435456 -k=16 -zipf-s=1.0', # deg32; FP~=1.12GB
        'max_inst': inst_100M,
    },
    'bptree': {
        'sched-partition': 'Xdiv',
        'data-partitioning': 'kernel-wide,kernel-wide,kernel-wide,kernel-wide,kernel-wide,kernel-wide,kernel-wide,kernel-wide',
        'params': '-size=64000000 -count=3000000 -bptree-zipf-s=0',
        'max_inst': inst_100M * 5,
    },
    'xsbench': { # ITL — irregular pointer-chase (NUMA stress; no exploitable locality)
        # Mirrors samples/xsbench/mesh. 4 buffers, all interleaved — LASP cannot
        # localize a random energy/material lookup, so kernel-wide is the only
        # sensible token (xsbench is the NUMA control case, not a LASP win):
        #   unionizedEnergyArray = nIso*nGrid*4 B       (HOT: binary-searched by all)
        #   indexGrid            = nIso*nGrid*nIso*4 B  (COLD: random row/lookup — the big one)
        #   nuclideGrid          = nIso*nGrid*32 B      (COLD: random)
        #   verification         = lookups*4 B          (per-lookup write)
        # gridpoints=100000 -> indexGrid ~= 1.85 GB (~51 MB/chiplet on 6x6),
        # well past aggregate L2 -> surfaces capacity misses + cross-chiplet fan-in.
        'sched-partition': 'Xdiv',
        'data-partitioning': 'kernel-wide',
        'params': '-isotopes=68 -gridpoints=100000 -lookups=30000000 -grid-type=unionized',
        # NOTE: the ~99% L2 hit + ~0 remote previously seen here was an emu bug,
        # NOT a max-inst/warmup effect. LCG_random_float's (float)(uint64) cast
        # lowered to a v_lshlrev_b64 u64->f32 the emu mis-executes -> p_energy
        # collapsed to ~hundreds of values, so every lookup hit the same ~2 MB
        # hot search tree and never reached the cold grids. Fixed in kernel.cl +
        # benchmark.go (high-31-bit u32->f32); rebuild kernel.hsaco (ROCm: make).
        # With the fix lookups diverge across the cold grids -> moderate L2 hit +
        # remote traffic. 1B kept so enough lookups complete to fill the cold tail.
        'max_inst': inst_100M,
    },
    'nn': { # Low-sharing baseline — per-work-item Euclidean distance.
        # No cross-WG writes; each thread reads its own LatLong and writes
        # its own distance. Useful as a strided-access reference point
        # against SpMV/bptree's indirect patterns.
        # Memory:
        #   locations = num-records * 8 B (LatLong = lat+lng floats)
        #   distances = num-records * 4 B
        # Scaled 8x for wafer-scale: num-records=128M → ~1.5 GB total,
        # grid = num-records threads (256/WG) → 524288 workgroups.
        # LASP per-kernel: NearestNeighbor (single launch).
        # LASP per-buffer (alloc order): locations, distances.
        'sched-partition': 'Xdiv',
        'data-partitioning': 'kernel-wide,kernel-wide',
        'params': '-num-records=134217728 -lat=30 -lng=90', # ~1.5GB
        'max_inst': inst_100M,
    },

    'tc': { # Triangle Counting (merge-based set intersection).
        # Sharing: heavy read traffic on CSR row/col arrays (every chiplet
        # reads any vertex's neighborhood) + atomic_add on a single global
        # totalCount. Smaller graph because per-vertex work is O(d^2).
        # LASP per-kernel (launch order): tc_kernel.
        # LASP per-buffer (alloc order):  row, col, tc_per_vertex,
        # total_count.
        'sched-partition': 'Xdiv',
        'data-partitioning': 'kernel-wide,kernel-wide,kernel-wide,kernel-wide',
        'params': '-num-nodes=8388608 -num-edges=134217728 -zipf-s=1.0', # deg16; FP~=576MB
        'max_inst': inst_100M*3,
    },
    'mst': { # Minimum Spanning Tree (Borůvka's algorithm).
        # Sharing: find_min phase is a per-component reduction — all vertices
        # of component C contend on min_wt[C] / min_edge[C]. Zipf graph makes
        # a few large components dominate → extreme hot spots on those slots.
        # This is structurally heavier than CC (which has no reduction stage):
        # union-find comp[] contention + reduction contention on min_wt[].
        # NOTE: kernels.hsaco must be built on ROCm node before running:
        #   cd mgpusim/benchmarks/pannotia/mst && make
        # LASP per-kernel (launch order): reset, find_min, union, compress.
        # LASP per-buffer (alloc order):  row, col, weight, comp, min_wt,
        #   min_edge, changed.
        'sched-partition': 'Xdiv,Xdiv,Xdiv,Xdiv',
        'data-partitioning': 'kernel-wide,kernel-wide,kernel-wide,kernel-wide,kernel-wide,kernel-wide,kernel-wide',
        # N=4M, E=100M (deg24), Zipf-s=1.05:
        #   row(16MB) + col(400MB) + weight(400MB) + comp/min_wt/min_edge(16MB×3) = ~864MB
        #   col+weight >> 144MB aggregate L2 → heavy L2 misses on CSR traversal.
        #   find_min writes to min_wt[srccomp]: Zipf hub component accumulates
        #   ~4.76% of 100M edges = ~4.76M cross-chiplet writes to one slot.
        'params': '-num-nodes=6291456 -num-edges=150994944 -zipf-s=0.9', # deg24; comp=25.2MB (4MB<x<36MB); edges↑ cold halo
        'max_inst': inst_10M*6,
    },
    'fr': { # Friend Recommendation (Pannotia; 2-hop set intersection).
        # Sharing: hub vertex adjacency lists become hot cross-chiplet read
        # targets. Zipf s=1.05 skews column selection so ~5% of edges land
        # on the top hub → N(hub) array (~5% of col[]) hammered by all
        # chiplets whose vertices have hub as a 2-hop neighbor.
        # Heavier per-vertex than TC: O(d_u * d_w * log d_u + merge) per u,
        # plus binary-search adjacency check per 2-hop candidate.
        # NOTE: kernels.hsaco must be built on ROCm node before running:
        #   cd mgpusim/benchmarks/pannotia/fr && make
        # LASP per-kernel (launch order): fr_kernel.
        # LASP per-buffer (alloc order):  row, col, fr_best, fr_score.
        'sched-partition': 'Xdiv',
        'data-partitioning': 'kernel-wide,kernel-wide,kernel-wide,kernel-wide',
        'params': '-num-nodes=8388608 -num-edges=218103808 -zipf-s=1.05', # deg26; FP~=928MB
        'max_inst': inst_10M*3,
        # 'max_inst': inst_100M*3,
    },
    'streamcluster': { # Rodinia online k-medians (pgain_kernel, single kernel).
        # Sharing: every WG broadcasts coord_d[i*num + x] for the per-launch
        # candidate x (read-only shared), then per-thread reads at
        # coord_d[i*num + tid] (ITL), and a scattered RMW into
        # work_mem_d[base + center_table_d[assign]] (data-dependent → Row 7).
        # LASP per-kernel (launch order): pgain_kernel (Iter launches).
        # LASP per-buffer (alloc order):  p, coord, work_mem, center_table,
        # switch_membership.
        'sched-partition': 'Xdiv',
        # 5 buffers in allocation order: p, coord, work_mem,
        # center_table, switch_membership.
        'data-partitioning': 'kernel-wide,kernel-wide,kernel-wide,kernel-wide,kernel-wide',
        # Footprint: coord(2M*128*4 = 1 GB) + work_mem(2M*65*4 ≈ 520 MB)
        # + p(2M*24 = 48 MB) + ct/sm(~10 MB) ≈ 1.6 GB.
        'params': '-num=2097152 -dim=128 -k=64 -iter=8',
        'max_inst': inst_100M,
    },
    'histogram': { # Parboil-style global atomic histogram.
        # Strongest inter-chiplet sharing in the new set: ALL WGs from ALL
        # chiplets atomic_add into the same 256-bin output array (~1KB =
        # 16 cache lines of contention).
        'sched-partition': 'Xdiv',
        'data-partitioning': 'kernel-wide',
        'params': '-num-elements=134217728 -num-bins=256', # 512MB input
        'max_inst': inst_100M,
    },
    'particlefilter': { # Rodinia SIR particle filter (single-precision, fp32).
        # Sharing: normalize_weights does cross-WG partial-sum reduction,
        # and find_index does a global CDF binary search where every WG
        # reads the shared CDF array.
        # LASP per-kernel (launch order): likelihood_kernel,
        # normalize_weights_kernel, find_index_kernel.
        # LASP per-buffer (alloc order):  arrayX, arrayY, xj, yj, weights,
        # CDF, u, ind, objxy, likelihood, image(I), seed, partial_sums.
        #
        # Sizing for 6x6 = 36 chiplets, ~2.3 GB total HBM footprint.
        # Footprint = 68*N bytes (fp32): 8 float particle buffers (32N)
        #   + seed (4N) + ind = Nparticles*countOnes ints (countOnes=8 -> 32N)
        #   + tiny partial_sums/objxy + image.
        #   - N = 2^25 = 33.5M particles -> ~2.28 GB + 8 MB image.
        #   - CDF (find_index's cross-chiplet binary-search target) = 134 MB.
        #     Below aggregate L2 (1.15 GB) so partly absorbed, but lookups
        #     scatter across all 36 chiplets and still expose mesh hops.
        #   - ind is the largest buffer (1.07 GB): per-particle x countOnes
        #     image indices, written then read within likelihood_kernel.
        #   - image 2048x2048x2 = 8 MB, sampled via I[ind[...]].
        # N is a multiple of blockSize (256): 33554432/256 = 131072.
        'sched-partition': 'Xdiv,Xdiv,Xdiv',
        'data-partitioning': 'kernel-wide,kernel-wide,kernel-wide,kernel-wide,kernel-wide,kernel-wide,kernel-wide,kernel-wide,kernel-wide,kernel-wide,kernel-wide,kernel-wide,kernel-wide',
        'params': '-nparticles=33554432 -isz-x=2048 -isz-y=2048 -nfr=2', # CDF=134MB, total ~2.3GB
        'max_inst': inst_100M,
    },
    'radixsort': { # SHOC LSB radix sort (32-pass; reduce/top_scan/bottom_scan).
        # Sharing: each pass scatters across the entire array via the shared
        # isums[] scan buffer; 32 passes × 3 kernels = 96 launches, so keep
        # the array modest.
        # LASP per-kernel (launch order): reduce, top_scan, bottom_scan.
        # LASP per-buffer (alloc order):  dA, dB, dIsums.
        # bottom_scan scatters into dA/dB by 4-bit digit key (Row 7); that
        # dominates the per-buffer placement choice, so kernel-wide wins
        # over reduce-only's Row-1 (stride-aware) preference.
        'sched-partition': 'Xdiv,Xdiv,Xdiv',
        'data-partitioning': 'kernel-wide,kernel-wide,kernel-wide',
        'params': '-num-elements=33554432 -block-size=256', # ~128MB ×2, 32 passes
        'max_inst': inst_100M*5,
    },
    # ------------------------------------------------------------------------
    # EmbeddingBag (DLRM sparse path). Single kernel: gather L rows from a
    # [V x D] table per bag, sum-reduce along the bag axis -> [B x D] output.
    # The defining access pattern is a *data-dependent random gather* on
    # `table` driven by `indices`, which is the canonical LASP-worst-case:
    # row-based partitioning of `table` localises only ~1/N of the reads,
    # so the dominant buffer is interleaved (kernel-wide) to spread DRAM
    # bank pressure and avoid Zipf hotspots on any single chiplet.
    #
    # Buffer allocation order in benchmark.go::initMem:
    #   indices, offsets, table, out
    # Xdiv (batch axis) localises indices/offsets/out via row-based; table
    # has no useful spatial partition, so kernel-wide.
    #
    # Sizing — one config across 16/36/64 chiplets:
    #   table   = V*D*4 = 4M * 128 * 4 = 2 GB    (dominant footprint)
    #   indices = B*L*4 = 8192 * 32 * 4 = 1 MB
    #   offsets = (B+1)*4 = 32 KB
    #   out     = B*D*4 = 4 MB
    #   total  ~= 2 GB  -> comfortably fits 16-chiplet (64 GB) HBM budget.
    # Work: grid=(B,D)=(8192,128) with WG=(8,8) -> 16384 WGs.
    #   - 16-chiplet (~512 CUs):  ~32 WGs/CU, oversubscribed.
    #   - 36-chiplet (~1152 CUs): ~14 WGs/CU.
    #   - 64-chiplet (~2048 CUs):  ~8 WGs/CU, still full occupancy.
    # Instruction budget: ~50 inst/WI * B * D ~= 52M -> inst_100M headroom.
    'embeddingbag': { # uniform indices (LASP baseline study)
        'sched-partition': 'Xdiv',
        'data-partitioning': 'row-based,row-based,kernel-wide,row-based',
        'params': '-B=8192 -D=128 -V=4194304 -L=32',
        'max_inst': inst_100M,
    },
    'embeddingbag_zipf': { # Zipf(1.1) indices (realistic DLRM / hotspot study)
        'name': 'embeddingbag',
        'alias': 'embeddingbag_zipf',
        'sched-partition': 'Xdiv',
        'data-partitioning': 'row-based,row-based,kernel-wide,row-based',
        'params': '-B=8192 -D=128 -V=4194304 -L=32 -zipf',
        'max_inst': inst_100M,
    },

    'scan': { # SHOC Blelloch prefix sum (single-pass: reduce/top_scan/bottom_scan).
        # Sharing: top_scan is single-block over the entire isums[] array
        # (every chiplet's reductions feed it); bottom_scan reads back the
        # globally-scanned isums[] from all chiplets.
        # LASP per-kernel (launch order): reduce, top_scan, bottom_scan.
        # LASP per-buffer (alloc order):  dInput, dOutput, dBlockSums.
        # dInput / dOutput are block-strided contiguous (Row 1 NL) in both
        # reduce and bottom_scan -> stride-aware; dBlockSums is small and
        # touched globally by top_scan -> kernel-wide.
        'sched-partition': 'Xdiv,Xdiv,Xdiv',
        'data-partitioning': 'stride-aware,stride-aware,kernel-wide',
        'data-strides': "0,0,0", # dInput, dOutput, dBlockSums
        'params': '-num-elements=67108864 -block-size=256', # 256MB ×2
        'max_inst': inst_100M,
    },
}