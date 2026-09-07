# Kernel sources — shoc/radixsort

| File      | Origin                                                                                       |
|-----------|----------------------------------------------------------------------------------------------|
| `sort.cl` | https://raw.githubusercontent.com/vetter/shoc/master/src/opencl/level1/sort/sort.cl          |

Fetched verbatim from the official SHOC repository (`vetter/shoc`, master).
This file is the LSB radix-sort kernel pipeline used by SHOC's `Sort.cpp`
host driver; it defines the three kernel symbols our `benchmark.go` loads:

- `reduce`       — per-block histogram of the current digit
- `top_scan`     — single-block scan of the block-sum array
- `bottom_scan`  — scatter step that uses the scanned offsets

Note: the bindata.go stub names it `sort.cl` already — no rename needed.
