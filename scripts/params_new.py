# Parameters for benchmarks.

inst_10M = 10000000
inst_30M = 30000000
inst_100M = 100000000
inst_1B = 1000000000

benchmarks = [
    # 'cc',
    # 'mst',
    # 'resnet_3x3',
    # 'resnet_3x3_mm',
    # 'gups'
    # 'mixtral_ffn',
    # 'mixtral_ffn2',
    # 'mst',
    'bptree',
    # 'spmm',
]
    

benchmark_configs = {
    'bptree': {
        'sched-partition': 'Xdiv',
        'data-partitioning': 'kernel-wide,kernel-wide,kernel-wide,kernel-wide,kernel-wide,kernel-wide,kernel-wide,kernel-wide',
        # order=32 (binary-fixed): L1-band = size/2 bytes. size=48M -> band 24MB
        # (4MB<24<36MB cluster). Shrunk from 128M(64MB, over cluster): bigger band
        # = worse (footprint clamp harder + replication pressure).
        # zipf-s flag: <=1 = uniform (whole band hot); >1 = Zipfian (1.05 too hot).
        # 'params': '-size=48000000 -count=4000000 -bptree-zipf-s=1.03',
        # 'max_inst': inst_100M*5,
        # 'params': '-size=24000000 -count=6000000 -bptree-zipf-s=1.15',
        # 'max_inst': inst_100M * 3,
        'params': '-size=64000000 -count=3000000 -bptree-zipf-s=0',
        'max_inst': inst_100M * 5,
    },

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

    # Tall/skinny rectangular (Ydiv): shared operand B = k*j shrunk into the
    # cluster-pool band (4MB < B < 16MB) so dyn-clustering holds it while
    # nearfetch (single 4MB L2) thrashes; large i keeps footprint high.
    # Align for A locality across 4x4/6x6/8x8: k%1024, j%1024, i%576(=lcm16,36,64).
    # B stride = j*4 (B row byte width). B=8MB @ (k=2048,j=1024).
    # NOTE: unrealistic AI shape; realistic path = LLM Xdiv (activations shared).
    # Not in `benchmarks` list -> won't auto-submit.
    'tall_gemm': {
        'name': 'gemm',
        'alias': 'tall_gemm',
        'sched-partition': 'Ydiv',
        'data-partitioning': 'row-based,col-based,stride-aware',
        'data-strides': "0,4096,0", # a, b(=j*4), c
        'params': '-i=147456 -j=1024 -k=2048', # B=8MB, A=1.2GB, ~1.8GB total
        'max_inst': inst_100M*5,
    },
    'tall_mm': {
        'name': 'matrixmultiplication',
        'alias': 'tall_mm',
        'sched-partition': 'Ydiv',
        'data-partitioning': 'row-based,col-based,stride-aware',
        'data-strides': "0,4096,0", # a, b(=z*4), c
        'params': '-x=2048 -y=147456 -z=1024', # x=k,y=i,z=j; B=8MB, ~1.8GB
        'max_inst': inst_100M*5,
    },

    # Realistic LLM-shaped GEMM (Xdiv): partition output features N -> weights B
    # (K*N) localized, ACTIVATIONS A (M*K) shared. Real K (hidden) + short M
    # (tokens) put shared A into the cluster-pool band (4MB < A <= 16MB) so
    # dyn-clustering holds A while nearfetch (single 4MB L2) thrashes it; the
    # large weights B keep footprint high. Align: N % 576 (=lcm 16,36,64) for a
    # clean B split across 4x4/6x6/8x8 -> real N rounded to nearest 576-multiple;
    # K % 1024 (A row = whole pages). strides b=c=N*4 (col-based row byte width).
    # name='gemm'; not in `benchmarks` list -> won't auto-submit.
    'gpt3_qkv': {   # K=12288 (GPT3 hidden), N=36864 (=3x hidden, exact), M=256
        'name': 'gemm', 'alias': 'gpt3_qkv',
        'sched-partition': 'Xdiv',
        'data-partitioning': 'row-based,col-based,col-based',
        'data-strides': "0,147456,147456", # b=c=N*4=36864*4
        'params': '-i=256 -j=36864 -k=12288', # A(shared)=12MB, B=1.8GB, C=38MB
        'max_inst': inst_100M*3,
    },
    'gpt3_ffn': {   # K=12288, N=48960 (~=49152 FFN, -0.4%), M=256
        'name': 'gemm', 'alias': 'gpt3_ffn',
        'sched-partition': 'Xdiv',
        'data-partitioning': 'row-based,col-based,col-based',
        'data-strides': "0,195840,195840", # b=c=N*4=48960*4
        'params': '-i=256 -j=48960 -k=12288', # A=12MB, B=2.4GB, C=50MB
        'max_inst': inst_100M*3,
    },
    'llama70b_ffn': {  # K=8192 (Llama-70B hidden), N=27648 (~=28672 FFN, -3.6%), M=256
        'name': 'gemm', 'alias': 'llama70b_ffn',
        'sched-partition': 'Xdiv',
        'data-partitioning': 'row-based,col-based,col-based',
        'data-strides': "0,110592,110592", # b=c=N*4=27648*4
        'params': '-i=256 -j=27648 -k=8192', # A=8MB, B=906MB, C=28MB
        'max_inst': inst_100M*3,
    },
    'llama70b_ffn_m512': {  # K=8192, N=18432 (=2.25x hidden), M=512 -> A at 16MB edge
        'name': 'gemm', 'alias': 'llama70b_ffn_m512',
        'sched-partition': 'Xdiv',
        'data-partitioning': 'row-based,col-based,col-based',
        'data-strides': "0,73728,73728", # b=c=N*4=18432*4
        'params': '-i=512 -j=18432 -k=8192', # A=16MB, B=604MB, C=38MB
        'max_inst': inst_100M*3,
    },
    # ---- Mixtral 8x22B expert FFN (MoE) ----
    # SOURCE (verified 2026-07-14): HuggingFace mistralai/Mixtral-8x22B-v0.1
    #   config.json: hidden_size=6144, intermediate_size=16384, num_local_experts=8,
    #   num_experts_per_tok=2, num_hidden_layers=56, heads=48, kv_heads=8, head_dim=128.
    #   -> expert FFN gate/up GEMM = hidden x intermediate = 6144 x 16384 = (K, N).
    #   M=336 = tokens routed to one expert (chosen so shared A=M*K ~= 8MB band).
    # NOTE: Mixtral 8x22B has NO dedicated paper. The Mixtral paper
    #   (Jiang et al., "Mixtral of Experts", 2024, arXiv:2401.04088) covers only
    #   8x7B (hidden=4096, intermediate=14336). For a paper-citable variant use
    #   8x7B: -i=512 -j=14336 -k=4096 (A=8MB, B=235MB).
    # mixtral_ffn = real N=16384 (stride 65536).  mixtral_ffn2 = N=16128 (%576
    #   aligned, -1.6%) for clean chiplet split; keep both to compare.
    'mixtral_ffn': {  # Mixtral 8x22B expert FFN: K=6144 (hidden), N=16384 (real), M=336
        'name': 'gemm', 
        'alias': 'mixtral_ffn',
        'sched-partition': 'Xdiv',
        'data-partitioning': 'row-based,col-based,col-based',
        # 'data-strides': "0,64512,64512", # b=c=N*4=16128*4
        'data-strides': "0,65536,65536", # b=c=N*4=16384*4
        # 'params': '-i=336 -j=16128 -k=6144', # A(shared)=8.3MB, B=396MB, C=22MB
        'params': '-i=336 -j=16384 -k=6144', # A(shared)=8.3MB, B=396MB, C=22MB
        'max_inst': inst_1B,
    },
    'mixtral_ffn2': {  # Mixtral 8x22B expert FFN: K=6144 (hidden), N=16384->16128 (%576,-1.6%), M=336
        'name': 'gemm', 
        'alias': 'mixtral_ffn2',
        'sched-partition': 'Xdiv',
        'data-partitioning': 'row-based,col-based,col-based',
        'data-strides': "0,64512,64512", # b=c=N*4=16128*4
        'params': '-i=336 -j=16128 -k=6144', # A(shared)=8.3MB, B=396MB, C=22MB
        'max_inst': inst_1B,
    },

    # Same LLM-shaped GEMMs on the matrixmultiplication kernel (AMDAPPSDK,
    # LDS-tiled float4) instead of polybench gemm (naive). Different reuse
    # structure (blockA tiled in LDS) -> complementary L2 data point.
    # mm flags: x=K(contraction), y=M(rows/tokens), z=N(features). Same Xdiv +
    # row/col/col + strides=N*4 as the gemm twins. name='matrixmultiplication'.
    'gpt3_qkv_mm': {   # K=12288, N=36864, M=256 ; A(shared)=12MB, B=1.8GB
        'name': 'matrixmultiplication', 'alias': 'gpt3_qkv_mm',
        'sched-partition': 'Xdiv',
        'data-partitioning': 'row-based,col-based,col-based',
        'data-strides': "0,147456,147456", # b=c=N*4=36864*4
        'params': '-x=12288 -y=256 -z=36864',
        'max_inst': inst_100M*3,
    },
    'gpt3_ffn_mm': {   # K=12288, N=48960, M=256 ; A=12MB, B=2.4GB
        'name': 'matrixmultiplication', 'alias': 'gpt3_ffn_mm',
        'sched-partition': 'Xdiv',
        'data-partitioning': 'row-based,col-based,col-based',
        'data-strides': "0,195840,195840", # b=c=N*4=48960*4
        'params': '-x=12288 -y=256 -z=48960',
        'max_inst': inst_100M*3,
    },
    'llama70b_ffn_mm': {  # K=8192, N=27648, M=256 ; A=8MB, B=906MB
        'name': 'matrixmultiplication', 'alias': 'llama70b_ffn_mm',
        'sched-partition': 'Xdiv',
        'data-partitioning': 'row-based,col-based,col-based',
        'data-strides': "0,110592,110592", # b=c=N*4=27648*4
        'params': '-x=8192 -y=256 -z=27648',
        'max_inst': inst_100M*3,
    },
    'llama70b_ffn_m512_mm': {  # K=8192, N=18432, M=512 ; A=16MB, B=604MB
        'name': 'matrixmultiplication', 'alias': 'llama70b_ffn_m512_mm',
        'sched-partition': 'Xdiv',
        'data-partitioning': 'row-based,col-based,col-based',
        'data-strides': "0,73728,73728", # b=c=N*4=18432*4
        'params': '-x=8192 -y=512 -z=18432',
        'max_inst': inst_100M*3,
    },

    # CNN 3x3 conv (im2col) as GEMM (non-LLM). ROLE FLIP vs LLM: shared A =
    # WEIGHTS (C_out x K), private B = INPUT patches (K x spatial), K=C_in*9.
    # i=C_out, k=K(=C_in*9), j=spatial(batch*H*W). Xdiv -> weights pooled by DC.
    # WHY 3x3 not 1x1: 1x1 has K=C_in small -> for A=8MB needs N(spatial) huge
    # (110592) -> weights over-reused across N -> cached local -> remote only 5%
    # -> DC ties (verified 2026-07-12, -0.7%). 3x3 K=C_in*9 large -> N small
    # (24192, ~llama regime) -> less reuse -> high remote -> DC wins.
    # A~9MB (in band), B~892MB. N%576, K%1024. gemm favored over mm.
    'resnet_3x3': {   # 3x3 conv C_in1024 -> C_out256, spatial=24192, K=9216
        'name': 'gemm', 'alias': 'resnet_3x3',
        'sched-partition': 'Xdiv',
        'data-partitioning': 'row-based,col-based,col-based',
        'data-strides': "0,96768,96768", # b=c=N*4=24192*4
        'params': '-i=256 -j=24192 -k=9216', # A(weights)=9.4MB, B(input)=892MB
        'max_inst': inst_100M*3,
    },
    'resnet_3x3_mm': {   # same on the matrixmultiplication kernel; x=K,y=M,z=N
        'name': 'matrixmultiplication', 'alias': 'resnet_3x3_mm',
        'sched-partition': 'Xdiv',
        'data-partitioning': 'row-based,col-based,col-based',
        'data-strides': "0,96768,96768",
        'params': '-x=9216 -y=256 -z=24192',
        'max_inst': inst_100M*3,
    },

    # SpMM (sparse A x dense B) = ported clSPARSE csrmv_batched
    # (benchmarks/clsparse/spmm; run `make` on a ROCm node to build spmm.hsaco).
    # Sparse family -> kernel-wide (ITL) + Zipf hub skew (SCORE clustering
    # target), NOT LASP localization. Gathered dense B (Dim x N) sized to the
    # cluster band; footprint from nnz. Dim<=65536 keeps the generator's uint32
    # occupancy key (y*Dim+x) from overflowing. key='spmm' -> samples/spmm.
    'spmm': {
        'sched-partition': 'Xdiv',
        'data-partitioning': 'kernel-wide',
        # B(dense) = Dim*N*4. N=96 -> B=25.2MB (4MB<25<36MB cluster). reuse=nnz/Dim=1678x.
        # zipf-s LOW(0.2): columns near-uniform -> whole B uniformly hot (not collapsed
        # to few hub rows). Raising N (not zipf) grows the hot band; keep zipf low.
        'params': '-dim=65536 -sparsity=0.0256 -n=96 -zipf-s=0.2',
        'max_inst': inst_100M,
    },

    # High-sharing Apps
    'gups': {
        'sched-partition': 'Xdiv',
        'data-partitioning': 'kernel-wide', # ITL
        'params': '-thread-block-size=64 -nthread-blocks=8192 -table-param=64', # ~= 256MB
        'max_inst': inst_10M*3,
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
        # 'max_inst': inst_30M,
        # 'max_inst': inst_10M*5,
        'max_inst': inst_100M,
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
    'lud': { # No Locality
        'sched-partition': 'Xdiv, Xdiv, Ydiv',
        'params': '-matrix-dim=12288', # 576MB
        # 'params': '-matrix-dim=24576', # ~2.3GB
        'max_inst': inst_100M*5,
    },
    'pagerank': { # ITL
        'sched-partition': 'Xdiv',
        # baseline: '-node=4194304 -sparsity=0.00002 -iterations=3 -zipf-s=1.0', # rank 16MB deg84; per-chiplet WSS ~4MB (marginal)
        # dyn-clustering favorable: rank 24MB (4MB<24MB<36MB cluster), globally shared, deg126 col halo 3.2GB, zipf spread
        'params': '-node=6291456 -sparsity=0.00001 -iterations=1 -zipf-s=0.8', # rank 25.2MB deg63; col~1.58GB
        'max_inst': inst_100M,
    },

    'cc': { # Connected Components (Shiloach-Vishkin union-find).
        # Sharing: all WGs atomic-CAS into the shared comp[] label array.
        # LASP per-kernel (launch order): hook_kernel, compress_kernel.
        # LASP per-buffer (alloc order):  row, col, comp, changed.
        'sched-partition': 'Xdiv,Xdiv',
        'data-partitioning': 'kernel-wide,kernel-wide,kernel-wide,kernel-wide',
        # DC-vs-NF MARGIN, not band-fit: the hot comp[] read band (data_id 2)
        # measured reuse ~61, shared by ~29 chiplets, read-only. At 16 MB that
        # data is SO hot that NF's reactive forwarding catches it as well as
        # DC's proactive distribution -> tiny margin. DC only wins big when the
        # working set is large enough that a single L2 (4 MB) THRASHES but the
        # WARM cluster pool (3x3 * 4 MB = 36 MB) still holds it. Push W toward
        # the 36 MB ceiling (with headroom) to lower reuse and break NF:
        #   comp = num-nodes*4 B:  8388608 -> 32 MB (deg32) : W=8x L2, in WARM
        #                                                     pool w/ headroom
        #                          9437184 -> 36 MB : at pool ceiling (risky)
        #                          4194304 -> 16 MB : reuse~61, NF keeps up
        #                                             (no margin, prev)
        # WATCH: comp must stay WARM-classified (36 MB pool). If it drops to
        # HOT (16 MB pool) 32 MB overflows -> DC thrashes too. reuse ~61->~30
        # here still keeps it hot. More inst does NOT help: read band is
        # saturated (access/line=61), inst only repeats it.
        # 'params': '-num-nodes=33554432 -num-edges=268435456', # ~1.3GB
        'params': '-num-nodes=8388608 -num-edges=268435456 -zipf-s=0.8', # deg32; comp=32MB (8x L2, in WARM pool)
        'max_inst': inst_100M*5,
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
}