package xsbench

import (
	"flag"
	"log"

	"gitlab.com/akita/mgpusim/footprint"
)

// wgSizeReplay mirrors the hardcoded local size in exec() (benchmark.go:241).
const wgSizeReplay = 256

// replay reproduces the address stream of macro_xs_lookup_kernel
// (kernel.cl:285-337) on the host.
//
// Every global read the kernel issues is determined by the per-thread LCG seed
// and the host-built grids, which is exactly what Verify()/computeLookup()
// already recompute. This walks the same index chain — fastForwardLCG ->
// pEnergy -> pickMat -> gridSearch -> indexGrid -> nuclideGrid — but keeps only
// the indices: the interpolation arithmetic in calculateMicroXS feeds no
// address, so it is dropped.
//
// Per-access counts follow the emitted ISA (kernel.disasm), not the source: the
// compiler hoists the num_nucs[mat] loop bound out of the nuclide loop and CSEs
// the three source-level index_data[] reads into one.
type replay struct {
	isotopes   *int
	gridpoints *int
	lookups    *int
	gridType   *string
	hashBins   *int
}

func init() {
	footprint.Register("xsbench", func() footprint.Replay { return &replay{} })
}

func (r *replay) Name() string { return "xsbench" }

func (r *replay) Flags(fs *flag.FlagSet) {
	r.isotopes = fs.Int("isotopes", 68,
		"Number of isotopes (68 = H-M Small, 355 = H-M Large).")
	r.gridpoints = fs.Int("gridpoints", 11303,
		"Number of gridpoints per isotope.")
	r.lookups = fs.Int("lookups", 500000,
		"Number of macroscopic XS lookups (work items).")
	r.gridType = fs.String("grid-type", "unionized",
		"Lookup grid type: unionized, nuclide, or hash.")
	r.hashBins = fs.Int("hash-bins", 10000,
		"Number of hash bins (hash grid type only).")
}

func (r *replay) Run(rec *footprint.Recorder, cfg footprint.Config) {
	b := &Benchmark{
		NIsotopes:   int64(*r.isotopes),
		NGridpoints: int64(*r.gridpoints),
		Lookups:     int32(*r.lookups),
		HashBins:    int32(*r.hashBins),
	}
	b.SetGridType(*r.gridType)
	if b.GridType == gridHash {
		log.Fatalf("xsbench replay supports -grid-type=unionized|nuclide only")
	}

	b.gridInit()

	// initMem pads the unused acceleration buffers before allocating, so the
	// bases below land where a real run puts them (benchmark.go:191-196).
	if len(b.unionizedEnergyArray) == 0 {
		b.unionizedEnergyArray = make([]float32, 1)
	}
	if len(b.indexGrid) == 0 {
		b.indexGrid = make([]int32, 1)
	}

	// Allocation order matches initMem (benchmark.go:210-216).
	space := footprint.NewSpace()
	numNucsBase := space.Alloc(uint64(len(b.numNucs)) * 4)
	concsBase := space.Alloc(uint64(len(b.concs)) * 4)
	matsBase := space.Alloc(uint64(len(b.mats)) * 4)
	egridBase := space.Alloc(uint64(len(b.unionizedEnergyArray)) * 4)
	indexBase := space.Alloc(uint64(len(b.indexGrid)) * 4)
	nucGridBase := space.Alloc(uint64(len(b.nuclideGrid)) * 32)
	verifyBase := space.Alloc(uint64(b.Lookups) * 4)

	// exec() rounds the launch up to whole work-groups (benchmark.go:243);
	// the tail threads return before touching anything (kernel.cl:297).
	lookups := int(b.Lookups)
	grid := footprint.Grid{
		GlobalSize: [3]uint32{uint32(roundUp(lookups, wgSizeReplay)), 1, 1},
		LocalSize:  [3]uint16{wgSizeReplay, 1, 1},
	}
	partition := cfg.PartitionFor(0)
	numChiplets := cfg.NumChiplets()

	nIso := b.NIsotopes
	nGrid := b.NGridpoints
	maxNum := int64(b.maxNumNucs)
	unionized := b.GridType == gridUnionized
	egrid := b.unionizedEnergyArray
	nucGrid := b.nuclideGrid

	chiplet := 0
	for i := 0; i < lookups; i++ {
		// LASP maps a whole work-group to one chiplet, so this only moves
		// every wgSizeReplay lookups.
		if i%wgSizeReplay == 0 {
			chiplet = footprint.ChipletOfWG(
				i/wgSizeReplay, 0, grid, partition, numChiplets)
		}

		// kernel.cl:302-305
		seed := fastForwardLCG(startingSeed, uint64(2*i))
		pEnergy := lcgRandomFloat(&seed)
		mat := int64(pickMat(&seed))

		idx := int64(0)
		if unionized {
			// idx = grid_search(n_isotopes*n_gridpoints, ...) kernel.cl:265
			idx = gridSearchTouched(
				rec, chiplet, egridBase, egrid, nIso*nGrid, pEnergy)
		}

		// for( j = 0; j < num_nucs[mat]; j++ ) — kernel.cl:272. The bound is
		// loaded once and hoisted out of the loop (kernel.disasm BB0_43),
		// since calculate_macro_xs stores to no global.
		rec.Touch(chiplet, numNucsBase+uint64(mat)*4)

		numNuc := int64(b.numNucs[mat])
		matRow := mat * maxNum
		for j := int64(0); j < numNuc; j++ {
			// p_nuc = mats[...]; conc = concs[...] kernel.cl:275-276
			off := uint64(matRow+j) * 4
			rec.Touch(chiplet, matsBase+off)
			rec.Touch(chiplet, concsBase+off)

			nuc := int64(b.mats[matRow+j])
			base := nuc * nGrid

			var lowIdx int64
			if unionized {
				// The three source-level index_data[idx*n_isotopes+nuc] reads
				// (kernel.cl:205-208) CSE into a single load — kernel.disasm
				// BB0_68 emits one flat_load_dword for the whole branch.
				e := idx*nIso + nuc
				rec.Touch(chiplet, indexBase+uint64(e)*4)

				gp := int64(b.indexGrid[e])
				if gp == nGrid-1 {
					lowIdx = base + gp - 1
				} else {
					lowIdx = base + gp
				}
			} else {
				// kernel.cl:196-201
				nIdx := gridSearchNuclideTouched(rec, chiplet,
					nucGridBase+uint64(base)*32, nucGrid[base:],
					pEnergy, 0, nGrid-1)
				if nIdx == nGrid-1 {
					lowIdx = base + nIdx - 1
				} else {
					lowIdx = base + nIdx
				}
			}

			// low and high = low+1 are adjacent 32 B NuclideGridPoints; the six
			// live floats of each are read as dwordx4 + dwordx2, so four loads
			// at low+{0,16,32,48} (kernel.cl:241-247, kernel.disasm BB0_45).
			// Each is 16 B-aligned and <=16 B, so none straddles a cacheline.
			low := nucGridBase + uint64(lowIdx)*32
			rec.Touch(chiplet, low)
			rec.Touch(chiplet, low+16)
			rec.Touch(chiplet, low+32)
			rec.Touch(chiplet, low+48)
		}

		// verification_array[i] = max_idx+1; kernel.cl:336
		rec.Touch(chiplet, verifyBase+uint64(i)*4)
	}
}

// gridSearchTouched mirrors gridSearch (benchmark.go:567) / grid_search
// (kernel.cl:142-162), recording the one A[examinationPoint] load per probe.
func gridSearchTouched(rec *footprint.Recorder, chiplet int, base uint64,
	a []float32, n int64, quarry float32,
) int64 {
	lower := int64(0)
	upper := n - 1
	length := upper - lower
	for length > 1 {
		mid := lower + length/2
		rec.Touch(chiplet, base+uint64(mid)*4)
		if a[mid] > quarry {
			upper = mid
		} else {
			lower = mid
		}
		length = upper - lower
	}

	return lower
}

// gridSearchNuclideTouched mirrors gridSearchNuclide (benchmark.go:583) /
// grid_search_nuclide (kernel.cl:164-184). Only the .energy field is read per
// probe, at the start of the 32 B element.
func gridSearchNuclideTouched(rec *footprint.Recorder, chiplet int, base uint64,
	a []NuclideGridPoint, quarry float32, low, high int64,
) int64 {
	lower := low
	upper := high
	length := upper - lower
	for length > 1 {
		mid := lower + length/2
		rec.Touch(chiplet, base+uint64(mid)*32)
		if a[mid].Energy > quarry {
			upper = mid
		} else {
			lower = mid
		}
		length = upper - lower
	}

	return lower
}
