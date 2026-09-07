# Build kernels.hsaco for use by //go:embed in benchmark.go.
# Requires clang-ocl from a ROCm install on PATH.
#
# NOTE: native/kernel/histo_kernel.cl is NOT shipped. Upstream Parboil splits
# the histogram into 4 stages (histo_prescan/intermediates/main/final, all in
# native/kernel/ for reference). Our benchmark.go expects a single naive
# atomic-histogram kernel named `histo_kernel` — write it (or adapt
# histo_main.cl's inner atomic_add loop) before `make` will succeed.
# See native/kernel/SOURCES.md for details.

all: kernels.hsaco kernels.disasm

kernels.hsaco: native/kernel/histo_kernel.cl
	clang-ocl -mcpu=gfx803 -g $^ -o $@

kernels.disasm: kernels.hsaco
	llvm-objdump -d -l -S -g --mcpu=gfx803 $^ | cat > $@

clean:
	rm -f kernels.hsaco kernels.disasm
