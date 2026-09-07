# Build kernel.hsaco for use by //go:embed in benchmark.go.
# Requires clang-ocl from a ROCm install on PATH.

all: kernel.hsaco kernel.disasm

kernel.hsaco: kernel.cl
	clang-ocl -mcpu=gfx803 -g $^ -o $@

kernel.disasm: kernel.hsaco
	llvm-objdump -d -l -S -g --mcpu=gfx803 $^ | cat > $@

clean:
	rm -f kernel.hsaco kernel.disasm
