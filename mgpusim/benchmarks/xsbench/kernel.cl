// XSBench OpenCL kernel — adapted from upstream opencl/.
//
// MGPUSim's emu does not implement the FP64 OR FP32 divide opcode
// sequences (v_div_scale / v_div_fmas / v_div_fixup, in either
// precision), so every FP divide must avoid the IEEE-correct lowering
// path. Two techniques used below:
//
//  1. Where possible, the divide is algebraically rewritten as a
//     multiplication by a precomputed reciprocal (the uint64 -> float
//     LCG normalisation used to be `(double)seed / 2^63` — now
//     `seed * 2^-63`).
//  2. Where a runtime divide is unavoidable (the log-energy
//     interpolation factor below), `native_divide(a, b)` is used. It
//     lowers to `v_rcp_f32` + `v_mul_f32` — both supported — at the
//     cost of ~2 ULP versus IEEE-correct (~1 ULP). XSBench is a
//     stochastic Monte Carlo workload, so this precision loss is
//     irrelevant to the reported macroscopic XS aggregates.
//
// The FP math is demoted to FP32 end-to-end. The LCG itself stays in
// uint64 for state.
//
// All array indices use 32-bit int (NOT long). With `long` indices the
// compiler emits 64-bit address arithmetic (v_lshlrev_b64 + v_addc_u32) whose
// high dword the MGPUSim emu mis-executes, corrupting the pointer to a wild
// 0xfffffffe_xxxxxxxx VAddr -> MMU unmapped walk -> DRAM "address is smaller
// than offset" panic. int indices generate 32-bit offset math (v_lshlrev_b32 +
// add/addc-from-0) which the emu handles. Magnitudes fit int for all H-M
// sizes (max idx*n_isotopes ~= 355*11303*355/... <= ~1.4e9 < 2^31).

// Padded to 32 B (power of two stride) so per-element addressing reduces to
// a shift, avoiding the 64-bit MAD instructions the MGPUSim emu mis-handles.
typedef struct{
	float energy;
	float total_xs;
	float elastic_xs;
	float absorbtion_xs;
	float fission_xs;
	float nu_fission_xs;
	float pad0;
	float pad1;
} NuclideGridPoint;

typedef struct{
	int nthreads;
	long n_isotopes;
	long n_gridpoints;
	int lookups;
	int HM;
	int grid_type; // 0: Unionized Grid (default)    1: Nuclide Grid
	int hash_bins;
	int particles;
	int simulation_method;
	int binary_mode;
	int kernel_id;
  int platform_id;
  int device_id;
} Inputs;

// Grid types
#define UNIONIZED 0
#define NUCLIDE 1
#define HASH 2

// Simulation types
#define HISTORY_BASED 1
#define EVENT_BASED 2

// Starting Seed
#define STARTING_SEED 1070

unsigned long fast_forward_LCG(unsigned long seed, unsigned long n)
{
	const unsigned long m = 9223372036854775808UL; // 2^63
	unsigned long a = 2806196910506780709UL;
	unsigned long c = 1UL;

	n = n % m;

	unsigned long a_new = 1;
	unsigned long c_new = 0;

	while(n > 0)
	{
		if(n & 1)
		{
			a_new *= a;
			c_new = c_new * a + c;
		}
		c *= (a + 1);
		a *= a;

		n >>= 1;
	}

	return (a_new * seed + c_new) % m;
}

float LCG_random_float(unsigned long * seed)
{
	const unsigned long m = 9223372036854775808UL; // 2^63
	const unsigned long a = 2806196910506780709UL;
	const unsigned long c = 1UL;
	*seed = (a * (*seed) + c) % m;
	// Normalise to [0,1) via the high 31 bits + u32->f32 (single v_cvt_f32_u32),
	// NOT (float)(*seed) * 2^-63: that cast lowers to a v_lshlrev_b64-based
	// u64->f32 mantissa normalisation the emu mis-executes (header's 64-bit op
	// family), losing the mantissa and collapsing ~2^24 energies to a few
	// hundred -> every lookup hits the same cachelines. Host mirrors this.
	return (float)(unsigned int)(*seed >> 32) * (1.0f / 2147483648.0f); // /2^31
}

int pick_mat( unsigned long * seed )
{
	float dist[12];
	dist[0]  = 0.140f;
	dist[1]  = 0.052f;
	dist[2]  = 0.275f;
	dist[3]  = 0.134f;
	dist[4]  = 0.154f;
	dist[5]  = 0.064f;
	dist[6]  = 0.066f;
	dist[7]  = 0.055f;
	dist[8]  = 0.008f;
	dist[9]  = 0.015f;
	dist[10] = 0.025f;
	dist[11] = 0.013f;

	float roll = LCG_random_float(seed);

	for( int i = 0; i < 12; i++ )
	{
		float running = 0;
		for( int j = i; j > 0; j-- )
			running += dist[j];
		if( roll < running )
			return i;
	}

	return 0;
}

int grid_search( int n, float quarry, __global const float * A)
{
	int lowerLimit = 0;
	int upperLimit = n-1;
	int examinationPoint;
	int length = upperLimit - lowerLimit;

	while( length > 1 )
	{
		examinationPoint = lowerLimit + ( length / 2 );

		if( A[examinationPoint] > quarry )
			upperLimit = examinationPoint;
		else
			lowerLimit = examinationPoint;

		length = upperLimit - lowerLimit;
	}

	return lowerLimit;
}

int grid_search_nuclide( int n, float quarry, __global const NuclideGridPoint * A, int low, int high)
{
	int lowerLimit = low;
	int upperLimit = high;
	int examinationPoint;
	int length = upperLimit - lowerLimit;

	while( length > 1 )
	{
		examinationPoint = lowerLimit + ( length / 2 );

		if( A[examinationPoint].energy > quarry )
			upperLimit = examinationPoint;
		else
			lowerLimit = examinationPoint;

		length = upperLimit - lowerLimit;
	}

	return lowerLimit;
}

void calculate_micro_xs(   float p_energy, int nuc, int n_isotopes,
                           int n_gridpoints,
                           __global const float * egrid, __global const int * index_data,
                           __global const NuclideGridPoint * nuclide_grids,
                           int idx, float * xs_vector, int grid_type, int hash_bins ){
	float f;
	__global const NuclideGridPoint * low, * high;

	if( grid_type == NUCLIDE )
	{
		idx = grid_search_nuclide( n_gridpoints, p_energy, &nuclide_grids[nuc*n_gridpoints], 0, n_gridpoints-1);

		if( idx == n_gridpoints - 1 )
			low = &nuclide_grids[nuc*n_gridpoints + idx - 1];
		else
			low = &nuclide_grids[nuc*n_gridpoints + idx];
	}
	else if( grid_type == UNIONIZED)
	{
		if( index_data[idx * n_isotopes + nuc] == n_gridpoints - 1 )
			low = &nuclide_grids[nuc*n_gridpoints + index_data[idx * n_isotopes + nuc] - 1];
		else
			low = &nuclide_grids[nuc*n_gridpoints + index_data[idx * n_isotopes + nuc]];
	}
	else // Hash grid
	{
		int u_low = index_data[idx * n_isotopes + nuc];

		int u_high;
		if( idx == hash_bins - 1 )
			u_high = n_gridpoints - 1;
		else
			u_high = index_data[(idx+1)*n_isotopes + nuc] + 1;

		float e_low  = nuclide_grids[nuc*n_gridpoints + u_low].energy;
		float e_high = nuclide_grids[nuc*n_gridpoints + u_high].energy;
		int lower;
		if( p_energy <= e_low )
			lower = 0;
		else if( p_energy >= e_high )
			lower = n_gridpoints - 1;
		else
			lower = grid_search_nuclide( n_gridpoints, p_energy, &nuclide_grids[nuc*n_gridpoints], u_low, u_high);

		if( lower == n_gridpoints - 1 )
			low = &nuclide_grids[nuc*n_gridpoints + lower - 1];
		else
			low = &nuclide_grids[nuc*n_gridpoints + lower];
	}

	high = low + 1;

	// native_divide -> v_rcp_f32 + v_mul_f32 (both implemented in emu).
	// Plain `/` would lower to v_div_scale_f32 / v_div_fmas_f32 /
	// v_div_fixup_f32 which the emu does NOT implement; see header.
	f = native_divide(high->energy - p_energy, high->energy - low->energy);

	xs_vector[0] = high->total_xs - f * (high->total_xs - low->total_xs);
	xs_vector[1] = high->elastic_xs - f * (high->elastic_xs - low->elastic_xs);
	xs_vector[2] = high->absorbtion_xs - f * (high->absorbtion_xs - low->absorbtion_xs);
	xs_vector[3] = high->fission_xs - f * (high->fission_xs - low->fission_xs);
	xs_vector[4] = high->nu_fission_xs - f * (high->nu_fission_xs - low->nu_fission_xs);
}

void calculate_macro_xs( float p_energy, int mat, int n_isotopes,
                         int n_gridpoints, __global const int * num_nucs,
                         __global const float * restrict concs,
                         __global const float * egrid, __global const int * index_data,
                         __global const NuclideGridPoint * nuclide_grids,
                         __global const int * mats,
                         float * macro_xs_vector, int grid_type, int hash_bins, int max_num_nucs ){
	int p_nuc;
	int idx = -1;
	float conc;

	for( int k = 0; k < 5; k++ )
		macro_xs_vector[k] = 0;

	if( grid_type == UNIONIZED )
		idx = grid_search( n_isotopes * n_gridpoints, p_energy, egrid);
	else if( grid_type == HASH )
	{
		// du = 1/hash_bins; idx = p_energy/du; collapse the divide.
		idx = (int)(p_energy * (float)hash_bins);
	}

	for( int j = 0; j < num_nucs[mat]; j++ )
	{
		float xs_vector[5];
		p_nuc = mats[mat*max_num_nucs + j];
		conc = concs[mat*max_num_nucs + j];
		calculate_micro_xs( p_energy, p_nuc, n_isotopes,
		                    n_gridpoints, egrid, index_data,
		                    nuclide_grids, idx, xs_vector, grid_type, hash_bins );
		for( int k = 0; k < 5; k++ )
			macro_xs_vector[k] += xs_vector[k] * conc;
	}
}

__kernel void macro_xs_lookup_kernel(		Inputs in,
											int max_num_nucs,
											__global const int *num_nucs,
											__global const float *concs,
											__global const float * unionized_energy_array,
											__global const int * index_grid,
											__global const NuclideGridPoint * nuclide_grid,
											__global const int * mats,
											__global int * verification_array )
{
	int i = get_global_id(0);

  if( i >= in.lookups )
    return;

	unsigned long seed = STARTING_SEED;

	seed = fast_forward_LCG(seed, 2*i);

	float p_energy = LCG_random_float(&seed);
	int mat         = pick_mat(&seed);

	float macro_xs_vector[5] = {0};

	calculate_macro_xs(
			p_energy,
			mat,
			(int)in.n_isotopes,
			(int)in.n_gridpoints,
			num_nucs,
			concs,
			unionized_energy_array,
			index_grid,
			nuclide_grid,
			mats,
			macro_xs_vector,
			in.grid_type,
			in.hash_bins,
			max_num_nucs
			);

	float max = -1.0f;
	int max_idx = 0;
	for(int j = 0; j < 5; j++ )
	{
		if( macro_xs_vector[j] > max )
		{
			max = macro_xs_vector[j];
			max_idx = j;
		}
	}
	verification_array[i] = max_idx+1;
}
