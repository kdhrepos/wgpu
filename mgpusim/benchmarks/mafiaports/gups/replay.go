package gups

import (
	"flag"

	"gitlab.com/akita/mgpusim/footprint"
)

const polyReplay = 0x7

// numAccess DIVERGENCE FROM THE COMPILED KERNEL — read before trusting counts.
//
// gups_kernel.cl:7-11,20-22 hardcodes THREADBLOCKSIZE=32 and NTHREADSBLOCKS=128
// (4096 threads) when it computes numAccess = TableSize/THREADBLOCKSIZE/
// NTHREADSBLOCKS, instead of deriving thread count from get_global_size. The
// sweep launches -thread-block-size=64 -nthread-blocks=8192 (params.py) =
// 524288 threads, so the real .hsaco currently in the repo runs numAccess =
// TableSize/4096 per thread regardless — 128x the "1*TableSize total, spread
// evenly over however many threads actually launched" the surrounding host
// code (NUpdates = 4*TableSize, main.go:160) is written to assume.
//
// This matters here beyond a reuse-count skew: numAccess drives an LFSR walk
// (ran = (ran<<1)^poly(ran) each step), so every extra iteration touches a
// *new* table address, not a repeat access to one already recorded. Replaying
// the hardcoded 4096 would therefore not just inflate per-line reuse, it would
// inflate the footprint itself — each thread would sweep 128x further through
// the table than the algorithm intends, manufacturing cross-chiplet sharing
// that a corrected kernel would not produce.
//
// The .hsaco is prebuilt (gups_kernel.cl -> kernels.hsaco via clang-ocl
// -mcpu=fiji) and this environment has no ROCm toolchain to rebuild it, so the
// actual compiled kernel keeps the bug regardless of anything here. This
// replay intentionally does NOT reproduce that bug: it computes numAccess from
// the real launch geometry (tbs*ntb), i.e. what the kernel would do if it read
// its own thread count instead of the hardcoded constants. That is a
// deliberate divergence from the currently-compiled binary, scoped to this
// offline footprint tool only — no timing-sim behavior changes.
func numAccessFor(tableSize, totalThreads uint64) int64 {
	return int64(tableSize / totalThreads)
}

// replay reproduces the address stream of RandomAccessUpdate
// (gups_kernel.cl:13-30) on the host.
//
// Each thread seeds ran from Starts[get_global_id(0)] and then walks a pure
// GF(2) LFSR recurrence. Nothing in the chain depends on memory, so the whole
// per-thread address sequence is a function of the thread id alone.
type replay struct {
	threadBlockSize *int
	nThreadBlocks   *int
	tableParam      *int
}

func init() {
	footprint.Register("gups", func() footprint.Replay { return &replay{} })
}

func (r *replay) Name() string { return "gups" }

func (r *replay) Flags(fs *flag.FlagSet) {
	r.threadBlockSize = fs.Int("thread-block-size", 32, "Dunno")
	r.nThreadBlocks = fs.Int("nthread-blocks", 128, "Dunno")
	r.tableParam = fs.Int("table-param", 1024, "")
}

func (r *replay) Run(rec *footprint.Recorder, cfg footprint.Config) {
	tbs := uint64(*r.threadBlockSize)
	ntb := uint64(*r.nThreadBlocks)

	// initMem (main.go:154-167).
	tableSize := uint64(*r.tableParam) * tbs * ntb
	totalThreads := ntb * tbs
	stride := (4 * tableSize / tbs) / ntb

	// Allocation order matches initMem (main.go:173-185).
	space := footprint.NewSpace()
	tableBase := space.Alloc(tableSize * 8)
	startsBase := space.Alloc(totalThreads * 8)

	// exec() launches with the host block geometry (main.go:204-205), which is
	// what decides the work-group count and therefore the chiplet assignment.
	grid := footprint.Grid{
		GlobalSize: [3]uint32{uint32(totalThreads), 1, 1},
		LocalSize:  [3]uint16{uint16(tbs), 1, 1},
	}
	partition := cfg.PartitionFor(0)
	numChiplets := cfg.NumChiplets()

	// See numAccessFor's doc comment: intended semantics, not the currently
	// compiled kernel's hardcoded-4096-thread arithmetic.
	numAccess := numAccessFor(tableSize, totalThreads)
	if numAccess <= 0 {
		return
	}

	mask := tableSize - 1
	for tid := uint64(0); tid < totalThreads; tid++ {
		chiplet := footprint.ChipletOfWG(
			int(tid/tbs), 0, grid, partition, numChiplets)

		// Starts[threadIdx] (.cl:19): lanes read consecutive words, so a
		// work-group's seeds coalesce into one pass over its own slice.
		if tid%tbs == 0 {
			rec.TouchRange(chiplet, startsBase+tid*8, tbs*8)
		}

		ran := HPCC_Starts(int64(stride * tid))
		for i := int64(0); i < numAccess; i++ {
			ran = (ran << 1) ^ hpccPolyMask(ran, polyReplay)

			// Table[index] ^= ran: one load plus one store (.cl:26-28).
			rec.TouchN(chiplet, tableBase+(ran&mask)*8, 2)
		}
	}
}
