// EmbeddingBag forward (sum pooling) with offsets.
//
// Computes:
//   out[b, d] = sum_{i in [offsets[b], offsets[b+1])} table[indices[i], d]
//
// Grid layout:
//   get_global_id(0) -> batch index  b in [0, B)
//   get_global_id(1) -> embedding dim d in [0, D)
//
// Buffers:
//   indices : int   [NNZ]      flat list of row IDs
//   offsets : int   [B + 1]    CSR-style offsets into `indices`
//   table   : float [V * D]    embedding table (row-major)
//   out     : float [B * D]    pooled output (row-major)
//
// One work-item produces one output element out[b, d]; the bag walk over
// indices[offsets[b]..offsets[b+1]) is a per-WI scalar loop. This keeps
// vector loads coalesced along the d axis while the indices[] read is a
// scalar broadcast inside the wavefront.
__kernel void EmbeddingBagSum(
    const int B,
    const int D,
    __global const int*   indices,
    __global const int*   offsets,
    __global const float* table,
    __global float*       out)
{
    const int b = get_global_id(0);
    const int d = get_global_id(1);
    if (b >= B || d >= D) return;

    const int start = offsets[b];
    const int end   = offsets[b + 1];

    float acc = 0.0f;
    for (int i = start; i < end; ++i) {
        const int row = indices[i];
        acc += table[row * D + d];
    }
    out[b * D + d] = acc;
}
