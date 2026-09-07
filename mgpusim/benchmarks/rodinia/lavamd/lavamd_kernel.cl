// lavaMD OpenCL kernel — adapted from rodinia/opencl/lavaMD/kernel/
// kernel_gpu_opencl.cl.
//
// Changes from upstream:
//   * box_str / nei_str struct-of-arrays flattened to three plain global
//     buffers (offsets, nn, nei_numbers). Per-box stride for nei_numbers is
//     padded to NEI_STRIDE = 32 (power of two) so element addressing reduces
//     to a shift — MGPUSim's emu mis-executes the 64-bit MAD shapes produced
//     by non-power-of-two strides (same family as the bptree / xsbench fix).
//   * FP32 only (emu lacks the FP64 divide opcodes).

#define fp float

#define NUMBER_PAR_PER_BOX 100
#define NUMBER_THREADS 128
#define MAX_NEI 26
#define NEI_STRIDE 32

#define DOT(A,B) ((A.x)*(B.x)+(A.y)*(B.y)+(A.z)*(B.z))

typedef struct { fp x, y, z; } THREE_VECTOR;
typedef struct { fp v, x, y, z; } FOUR_VECTOR;

__kernel void kernel_gpu_opencl(
		fp alpha,
		int number_boxes,
		__global long *d_offsets,
		__global int  *d_nn,
		__global int  *d_nei_numbers,
		__global FOUR_VECTOR *d_rv_gpu,
		__global fp  *d_qv_gpu,
		__global FOUR_VECTOR *d_fv_gpu)
{
	int bx  = get_group_id(0);
	int tx  = get_local_id(0);
	int wtx = tx;

	__local FOUR_VECTOR rA_shared[NUMBER_PAR_PER_BOX];
	__local FOUR_VECTOR rB_shared[NUMBER_PAR_PER_BOX];
	__local fp          qB_shared[NUMBER_PAR_PER_BOX];

	if (bx < number_boxes) {
		fp a2 = 2 * alpha * alpha;

		long first_i = d_offsets[bx];
		int  nn      = d_nn[bx];

		while (wtx < NUMBER_PAR_PER_BOX) {
			rA_shared[wtx] = d_rv_gpu[first_i + wtx];
			wtx += NUMBER_THREADS;
		}
		wtx = tx;
		barrier(CLK_LOCAL_MEM_FENCE);

		for (int k = 0; k < 1 + nn; k++) {
			int pointer;
			if (k == 0) {
				pointer = bx;
			} else {
				pointer = d_nei_numbers[bx * NEI_STRIDE + (k - 1)];
			}

			long first_j = d_offsets[pointer];

			while (wtx < NUMBER_PAR_PER_BOX) {
				rB_shared[wtx] = d_rv_gpu[first_j + wtx];
				qB_shared[wtx] = d_qv_gpu[first_j + wtx];
				wtx += NUMBER_THREADS;
			}
			wtx = tx;
			barrier(CLK_LOCAL_MEM_FENCE);

			while (wtx < NUMBER_PAR_PER_BOX) {
				for (int j = 0; j < NUMBER_PAR_PER_BOX; j++) {
					fp r2 = rA_shared[wtx].v + rB_shared[j].v
						  - DOT(rA_shared[wtx], rB_shared[j]);
					fp u2 = a2 * r2;
					fp vij = exp(-u2);
					fp fs  = 2 * vij;

					THREE_VECTOR d;
					d.x = rA_shared[wtx].x - rB_shared[j].x;
					fp fxij = fs * d.x;
					d.y = rA_shared[wtx].y - rB_shared[j].y;
					fp fyij = fs * d.y;
					d.z = rA_shared[wtx].z - rB_shared[j].z;
					fp fzij = fs * d.z;

					d_fv_gpu[first_i + wtx].v += qB_shared[j] * vij;
					d_fv_gpu[first_i + wtx].x += qB_shared[j] * fxij;
					d_fv_gpu[first_i + wtx].y += qB_shared[j] * fyij;
					d_fv_gpu[first_i + wtx].z += qB_shared[j] * fzij;
				}
				wtx += NUMBER_THREADS;
			}
			wtx = tx;
			barrier(CLK_LOCAL_MEM_FENCE);
		}
	}
}
