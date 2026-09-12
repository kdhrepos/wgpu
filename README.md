# wgpu — Wafer-Scale GPU Simulator

A cycle-level simulator for a **mesh-based wafer-scale GPU**, extending the
crossbar-based [MCM-GPU](https://ieeexplore.ieee.org/document/8574585)/[MGPUSim](https://github.com/sarchlab/mgpusim)
model onto an `xDim x yDim` mesh of chiplets connected by an on-chip network,
built on the [Akita](https://gitlab.com/akita) discrete-event simulation framework.

This is a cleaned-up export of an internal research tree — trimmed down to the
general model and stripped of research-only extensions (custom schedulers,
alternative cache/replication policies, offline analysis tooling). Cleanup is
still in progress, so a few non-`mesh` platform variants and dormant research
flags are still present in `mgpusim/platform/` and `mgpusim/runner/`.

## Layout

Each top-level directory is its own Go module (no single root `go.mod`; wired
together with `go.work` + `replace` directives):

| dir | contents |
|---|---|
| `akita/` | core discrete-event sim framework: `Component`, `Port`, `Engine`, `Event`, `Connection` |
| `mem/` | memory hierarchy: caches (`mem/cache/writeback`, `writethrough`), DRAM, TLB/MMU |
| `noc/` | network-on-chip: flits, mesh/direct chiplet interconnect, routing, arbitration |
| `protocol/` | coherence/distribution protocol message types |
| `util/` | shared utilities (tracing, pipelining, cache helpers) |
| `mgpusim/` | the GPU simulator: ISA decode + functional emulator, timing model (CU/wavefront/caches), driver, and the `mesh` platform builder |
| `scripts/compile.py` | builds every benchmark under `mgpusim/samples/` |

## Build

Requires Go 1.25+.

```bash
cd mgpusim
go build ./...
```

or build every benchmark:

```bash
python3 scripts/compile.py
```

## Run

Each `mgpusim/samples/<benchmark>/` builds a standalone executable named after
its directory. A ready-to-use `mesh` wrapper script sits next to it, e.g.:

```bash
cd mgpusim/samples/pagerank
go build
./mesh
```

or invoke the runner flags directly:

```bash
./pagerank -node=2048 -sparsity=0.5 -iterations=1 \
  -timing -report-all -verify -no-progress-bar \
  -platform-type=mesh -xDim=6 -yDim=6
```

See `mgpusim/runner/runner.go` for the full flag reference.

## License

Each module carries its own `LICENSE` file (MIT), inherited from upstream
Akita/MGPUSim.
