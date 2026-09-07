# Build kernels.hsaco for use by //go:embed in benchmark.go.
# Requires clang-ocl from a ROCm install on PATH.

all: kernels.hsaco kernels.disasm

kernels.hsaco: native/kernel/bc_kernel.cl
	clang-ocl -mcpu=gfx803 -g $^ -o $@

kernels.disasm: kernels.hsaco
	llvm-objdump -d -l -S -g --mcpu=gfx803 $^ | cat > $@

clean:
	rm -f kernels.hsaco kernels.disasm
