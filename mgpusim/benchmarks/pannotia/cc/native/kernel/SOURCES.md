# Kernel sources — pannotia/cc (Connected Components)

**No upstream OpenCL kernel available.** The Pannotia paper (Che et al., IISWC 2013)
lists CC as one of its benchmarks, but the open-source release at
https://github.com/pannotia/pannotia (master, the only branch) ships only:

    bc, color, fw, mis, pagerank (prk), sssp

i.e. `graph_app/connected_comp/` is **not** in the upstream tree. Probed (2026-05-18):

- https://github.com/pannotia/pannotia/tree/master/graph_app → no `connected_comp` dir
- `api.github.com/repos/pannotia/pannotia/git/trees/master?recursive=1` confirms it
- All searched forks (vancemiller, gauravjain14/pannotia_hip, lashhw/pannotia-cte,
  alvaroggutierrez1, lemonsien) also omit CC
- `accel-sim/gpu-app-collection`'s bundled pannotia copy is CUDA-only and likewise omits CC

The `bindata.go` stub claims a path of `graph_app/connected_comp/kernel/kernel_cc.cl`
— that path **does not exist**. The benchmark.go's "OpenCL source" comment is
aspirational only.

**Action required to run this benchmark**: write a CC kernel by hand
(same approach used by `pannotia/kcore/` and `pannotia/tc/` — see their
`native/kernel/*.cl`). The benchmark expects two kernel symbols, defined in
`benchmark.go`:

- `hook_kernel`     — Shiloach-Vishkin hook step (atomic CAS on `comp[]`)
- `compress_kernel` — path compression: `comp[i] = comp[comp[i]]`

Argument layouts are `HookKernelArgs` / `CompressKernelArgs` in `benchmark.go`.
