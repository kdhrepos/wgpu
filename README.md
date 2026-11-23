# wgpu — Wafer-Scale GPU Simulator

Cycle-level simulator for a **wafer-scale GPU**: an `xDim × yDim`
mesh of GPU chiplets, extending [MGPUSim](https://github.com/sarchlab/mgpusim)
on the [Akita](https://gitlab.com/akita) engine.

Platforms (`-platform-type`):

- `mesh` (default): chiplets connected by a 2D-mesh network-on-chip.
- `ideal`: same chiplets on a point to point network (ideal network).

## Layout

Each top-level directory is a Go module, wired together by `go.work`.

| dir | contents |
|---|---|
| `akita/` | event-based simulation engine |
| `mem/` | L2 cache, DRAM, TLB/MMU, page table, allocators |
| `noc/` | network-on-chip: mesh/ideal chiplet networks, switches, routing |
| `protocol/` | memory/translation message types |
| `lmf/` | address → chiplet/bank mapping |
| `util/` | tracing, pipelining, shared helpers |
| `mgpusim/` | GPU model (CU, caches, CP, RDMA), driver, platforms, runner, benchmarks |
| `scripts/compile.py` | builds every benchmark under `mgpusim/samples/` |

## Build

Requires Go 1.25+.

```bash
python3 scripts/compile.py                   # all benchmarks
cd mgpusim/samples/pagerank && go build      # one benchmark
```

## Run

Every `mgpusim/samples/<benchmark>/` has a `mesh` script that builds and runs
an example configuration:

```bash
cd mgpusim/samples/pagerank
./mesh
```

Or call the binary directly:

```bash
./pagerank -node=2048 -sparsity=0.5 -iterations=1 \
  -report-all -verify -no-progress-bar \
  -platform-type=mesh -xDim=6 -yDim=6
```

Metrics go to `metrics.csv` (`-metric-file-name`). All flags:
`mgpusim/runner/runner.go`.

## License

`akita/`, `mem/`, `noc/`, `util/` and `mgpusim/` keep their upstream MIT
`LICENSE` files.
