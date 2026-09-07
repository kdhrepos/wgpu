# Build spmm.hsaco for use by //go:embed in benchmark.go.
# Requires clang-ocl from a ROCm install on PATH (targets gfx803, code-object-v2).
# Run this on a ROCm-capable node; the committed spmm.hsaco is the build output.

all: spmm.hsaco spmm.disasm

spmm.hsaco: spmm.cl
	clang-ocl -mcpu=gfx803 -g $^ -o $@

spmm.disasm: spmm.hsaco
	llvm-objdump -d -l -S -g --mcpu=gfx803 $^ | cat > $@

clean:
	rm -f spmm.hsaco spmm.disasm
