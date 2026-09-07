all: kernels.hsaco kernels.disasm

kernels.hsaco: native/kernel/mst_kernel.cl
	clang-ocl -mcpu=gfx803 -g $^ -o $@

kernels.disasm: kernels.hsaco
	llvm-objdump -d -l -S -g --mcpu=gfx803 $^ | cat > $@

clean:
	rm -f kernels.hsaco kernels.disasm
