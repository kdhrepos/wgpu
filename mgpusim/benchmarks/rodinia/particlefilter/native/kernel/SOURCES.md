# Kernel sources — rodinia/particlefilter

| File                 | Origin                                                                                                                |
|----------------------|-----------------------------------------------------------------------------------------------------------------------|
| `particle_double.cl` | https://raw.githubusercontent.com/yuhc/gpu-rodinia/master/opencl/particlefilter/particle_double.cl                    |
| `particle_single.cl` | https://raw.githubusercontent.com/yuhc/gpu-rodinia/master/opencl/particlefilter/particle_single.cl                    |

Both files fetched verbatim from the `yuhc/gpu-rodinia` master branch
(the canonical maintained mirror of the IMPACT Rodinia 3.1 OpenCL release).

Kernel symbols used by `benchmark.go`:
- `likelihood_kernel`
- `normalize_weights_kernel`
- `find_index_kernel`

Both `_double` and `_single` expose the same three symbols with identical
argument order and identical scalar layouts — they differ only in the
precision of the floating-point buffers (`double *` vs `float *`).

`makefile` picks the source via the `CL` variable. Default is the
single-precision variant. To rebuild the fp64 variant:
```
make CL=native/kernel/particle_double.cl
```

Other variants present in the upstream directory but not used here:
- `particle_naive.cl`  — naive port (not used)
