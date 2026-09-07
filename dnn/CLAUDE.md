# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Purpose

A small DNN framework used by MGPUSim's DNN benchmarks (`lenet`, `vgg16`, `relu`, `maxpooling`, `im2col`, ...). Provides tensor ops, layers, and a training loop with mockable interfaces for unit testing. **Not** a production framework — it exists so DNN workloads can be expressed against the same `mgpusim/driver` API as everything else.

## Layout

- `tensor/` — `Tensor` type, element-wise/matmul `Operator` interface. The actual GPU-backed implementation lives in `../mgpusim/benchmarks/dnn`; this package defines the *interface*.
- `layers/` — `Layer` interface plus implementations: `Conv2D`, `FullyConnected`, `ReLU`, `MaxPooling`, `AvgPooling`.
- `training/` — `Trainer`, `Tester`, `DataSource`, loss functions (`CrossEntropy`, `SoftmaxCrossEntropy`), and `optimization/` (optimizer interfaces).
- `example/` — minimal usage examples.
- `dataset/` directory is gitignored (`.gitignore` excludes `dnn/dataset/*`).

## Tests

Heavy mockgen usage. Each subpackage has `*_suite_test.go` (Ginkgo) and `mock_*_test.go` (generated). Regenerate after interface changes:
```bash
cd layers && go generate ./...
cd ../training && go generate ./...
go test ./...
```

## Note on go.mod

`mgpusim/go.mod` requires `gitlab.com/akita/dnn` but has the `replace gitlab.com/akita/dnn => ../dnn` line **commented out**. Changes here are picked up from the pinned module version unless that replace is re-enabled. Re-enable it when iterating on the DNN code together with the DNN benchmarks.
