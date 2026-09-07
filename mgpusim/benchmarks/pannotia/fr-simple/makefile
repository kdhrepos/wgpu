# Build kernels.hsaco for use by //go:embed in benchmark.go.
# Requires clang-ocl from a ROCm install on PATH (build on a ROCm node).
#
# NOTE: the kernels.hsaco currently in this directory is a PLACEHOLDER copied
# from ../fr so that `go build` (go:embed) succeeds. It contains fr_kernel,
# NOT frsimple_kernel, so the benchmark will panic ("Failed to load
# frsimple_kernel") at runtime until you rebuild here:
#     cd benchmarks/pannotia/fr-simple && make

all: kernels.hsaco kernels.disasm

kernels.hsaco: native/kernel/frsimple_kernel.cl
	clang-ocl -mcpu=gfx803 -g $^ -o $@

kernels.disasm: kernels.hsaco
	llvm-objdump -d -l -S -g --mcpu=gfx803 $^ | cat > $@

clean:
	rm -f kernels.hsaco kernels.disasm
