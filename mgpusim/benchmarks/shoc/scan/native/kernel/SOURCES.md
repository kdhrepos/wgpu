# Kernel sources — shoc/scan

| File      | Origin                                                                                       |
|-----------|----------------------------------------------------------------------------------------------|
| `scan.cl` | https://raw.githubusercontent.com/vetter/shoc/master/src/opencl/level1/scan/scan.cl          |

Fetched verbatim from the official SHOC repository (`vetter/shoc`, master).

**Filename case**: upstream is lowercase `scan.cl`. The benchmark's
`bindata.go` stub references the file as `Scan.cl` (capital S) — this is a
typo in the stub. When you run `clang-ocl`, either pass the actual filename
(`scan.cl`) or rename the stub.

Defines the three Blelloch-scan kernel symbols our `benchmark.go` loads:

- `reduce`       — per-block reduction
- `top_scan`     — single-block scan of the block-sum array
- `bottom_scan`  — final scan + add step
