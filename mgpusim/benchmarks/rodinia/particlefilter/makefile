# Build kernels.hsaco for use by //go:embed in benchmark.go.
# Requires clang-ocl from a ROCm install on PATH.
#
# Default build uses the fp32 (single-precision) variant.
# To rebuild the fp64 variant, set CL=native/kernel/particle_double.cl.

CL ?= native/kernel/particle_single.cl

all: kernels.hsaco kernels.disasm

kernels.hsaco: $(CL)
	clang-ocl -mcpu=gfx803 -g $^ -o $@

kernels.disasm: kernels.hsaco
	llvm-objdump -d -l -S -g --mcpu=gfx803 $^ | cat > $@

clean:
	rm -f kernels.hsaco kernels.disasm
