# Build kernels.hsaco for use by //go:embed in benchmark.go.
# Requires clang-ocl from a ROCm install on PATH.
#
# NOTE: native/kernel/kernel_cc.cl is NOT shipped — connected_comp is absent
# from the upstream pannotia repo (see native/kernel/SOURCES.md). Write the
# `hook_kernel` and `compress_kernel` by hand before `make` will succeed.

all: kernels.hsaco kernels.disasm

kernels.hsaco: lud_kernel.cl
	clang-ocl -mcpu=gfx803 -g $^ -o $@

kernels.disasm: kernels.hsaco
	llvm-objdump -d -l -S -g --mcpu=gfx803 $^ | cat > $@

clean:
	rm -f kernels.hsaco kernels.disasm
