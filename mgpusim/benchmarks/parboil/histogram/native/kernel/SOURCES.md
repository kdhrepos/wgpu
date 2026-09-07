# Kernel sources — parboil/histogram

| File                       | Origin                                                                                                              |
|----------------------------|---------------------------------------------------------------------------------------------------------------------|
| `histo_prescan.cl`         | https://raw.githubusercontent.com/abduld/Parboil/master/benchmarks/histo/src/opencl_base/histo_prescan.cl           |
| `histo_intermediates.cl`   | https://raw.githubusercontent.com/abduld/Parboil/master/benchmarks/histo/src/opencl_base/histo_intermediates.cl     |
| `histo_main.cl`            | https://raw.githubusercontent.com/abduld/Parboil/master/benchmarks/histo/src/opencl_base/histo_main.cl              |
| `histo_final.cl`           | https://raw.githubusercontent.com/abduld/Parboil/master/benchmarks/histo/src/opencl_base/histo_final.cl             |

Fetched from `abduld/Parboil` (a mirror of the Parboil 2.5 release that
preserves the OpenCL variants — the original IMPACT/UIUC distribution is no
longer available for direct download). Same content is also in `yuhc/gpu-parboil`
under `benchmarks/histo/src/opencl_base/`.

**Note on kernel layout**: the upstream Parboil histogram is a 4-stage pipeline
(prescan → intermediates → main → final) tuned for the IMPACT data set.
Our `benchmark.go` simplifies this to a **single naive atomic-histogram kernel**
(`histo_kernel`), per the comment block in `benchmark.go`:

> Each workgroup processes a contiguous chunk of the input array. Every element
> is mapped to a bin via `(element % NumBins)` and accumulated with an
> `atomic_add` into the global output histogram.

The 4 upstream files above are stored here for **reference / provenance**.
The `histo_kernel.cl` named by `bindata.go` does not exist in the upstream
release and must be written by hand (or by adapting `histo_main.cl`'s atomic
inner loop into a stand-alone kernel).
