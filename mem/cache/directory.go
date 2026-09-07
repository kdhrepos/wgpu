package cache

import "gitlab.com/akita/util/ca"

// golden is the 64-bit golden-ratio constant used by getSet's
// multiplicative set-index hash. Package-level so the PageWalkCache
// directory (different struct, same package) can reuse it.
const golden = uint64(0x9E3779B97F4A7C15) // 2^64 * (sqrt(5)-1)/2

// A Block of a cache is the information that is associated with a cache line
type Block struct {
	PID          ca.PID
	Tag          uint64
	WayID        int
	SetID        int
	CacheAddress uint64
	IsValid      bool
	IsDirty      bool
	ReadCount    int
	IsLocked     bool
	DirtyMask    []bool

	// InstallPC is the PC of the request that last installed this line.
	// Tracing-only (no HW counterpart); used by MemoryAccessTracer to emit
	// victim PC on evict/overwrite (type 8/9 rows).
	InstallPC uint64
}

// A Set is a list of blocks where a certain piece memory can be stored at
type Set struct {
	Blocks   []*Block
	LRUQueue []*Block
}

// A Directory stores the information about what is stored in the cache.
type Directory interface {
	Lookup(pid ca.PID, address uint64) *Block
	FindVictim(address uint64) *Block
	Visit(block *Block)
	TotalSize() uint64
	WayAssociativity() int
	GetSets() []Set
	Reset()
}

// A DirectoryImpl is the default implementation of a Directory
//
// The directory can translate from the request address (can be either virtual
// address or physical address) to the cache based address.
type DirectoryImpl struct {
	NumSets   int
	NumWays   int
	NumBanks  int
	BlockSize int

	Sets []Set

	// SetAccessCount[i] = number of times getSet returned setID=i.
	// Exported so external readers (runner / tracers) can dump the
	// per-set access histogram to verify address-distribution against
	// the L2 set-indexing function.
	SetAccessCount []uint64

	// setHashShift is log2(NumSets), used by the XOR-based set hash in
	// getSet to break stride/set-period alignment introduced by the
	// LASP allocator (see commit message of "fix(cache): hash setID").
	setHashShift uint

	victimFinder VictimFinder
}

// NewDirectory returns a new directory object. numBanks is the upstream
// stripe factor (number of sibling banks that share the cacheline-granular
// interleaving above this directory). Pass 1 when there is no upstream
// stripe (e.g. L1 caches). NumBanks is used to skip the bank-index bits
// before computing setID, preventing set/bank aliasing.
func NewDirectory(
	set, way, blockSize, numBanks int,
	victimFinder VictimFinder,
) *DirectoryImpl {
	if numBanks <= 0 {
		numBanks = 1
	}
	d := new(DirectoryImpl)
	d.victimFinder = victimFinder
	d.Sets = make([]Set, set)
	d.SetAccessCount = make([]uint64, set)

	d.NumSets = set
	d.NumWays = way
	d.NumBanks = numBanks
	d.BlockSize = blockSize

	// Precompute log2(NumSets) for the XOR set hash. Falls back to a
	// non-zero shift if NumSets is not a power of two.
	d.setHashShift = 1
	for (1 << d.setHashShift) < uint(set) {
		d.setHashShift++
	}

	d.Reset()

	return d
}

// GetSetAccessCount returns the per-setID access histogram (length = NumSets).
// Index i holds the cumulative number of getSet calls that resolved to setID i.
func (d *DirectoryImpl) GetSetAccessCount() []uint64 {
	return d.SetAccessCount
}

// TotalSize returns the maximum number of bytes can be stored in the cache
func (d *DirectoryImpl) TotalSize() uint64 {
	return uint64(d.NumSets) * uint64(d.NumWays) * uint64(d.BlockSize)
}

// Get the set that a certain address should store at.
//
// setID uses the MurmurHash3 64-bit finalizer (Wang's hash) for the
// strongest practical de-aliasing. Three avalanche rounds (XOR-shift,
// multiply, XOR-shift, multiply, XOR-shift) ensure every input bit
// affects ~half of the output bits. Unlike Knuth's single multiplicative
// hash which fails when stride is a multiple of NumSets (LASP's
// numChiplets×NumBanks×pageSize stride is exactly 18×128 for a 6×6
// mesh), Wang's hash uses the FULL hashed output via modulo so no
// stride survives.
//
// Cost: ~25 cycles (2 IMUL + 3 XOR + 3 SHR + 1 MOD). Negligible vs the
// rest of the L2 stage pipeline.
//
// Not realistic for commercial GPU L2. Real hardware uses simpler XOR
// hashing plus OS-side physical-page scatter. MGPUSim's LASP allocator
// hands out deterministic contiguous physical pages, so the cache has
// to de-alias on its own.
func (d *DirectoryImpl) getSet(reqAddr uint64) (set *Set, setID int) {
	li := reqAddr / uint64(d.BlockSize) / uint64(d.NumBanks)
	h := li
	h ^= h >> 33
	h *= 0xff51afd7ed558ccd
	h ^= h >> 33
	h *= 0xc4ceb9fe1a85ec53
	h ^= h >> 33
	setID = int(h % uint64(d.NumSets))
	set = &d.Sets[setID]
	return
}

// RecordSetAccess increments SetAccessCount for the setID that `reqAddr`
// hashes to. Use this from outside (directory stage success paths) when
// you want to count *transactions* rather than getSet probes — re-probes
// from PeekReady stalls would otherwise inflate counts dramatically for
// stalled (e.g. miss-in-flight) lines.
func (d *DirectoryImpl) RecordSetAccess(reqAddr uint64) {
	li := reqAddr / uint64(d.BlockSize) / uint64(d.NumBanks)
	h := li
	h ^= h >> 33
	h *= 0xff51afd7ed558ccd
	h ^= h >> 33
	h *= 0xc4ceb9fe1a85ec53
	h ^= h >> 33
	setID := int(h % uint64(d.NumSets))
	d.SetAccessCount[setID]++
}

// Lookup finds the block that reqAddr. If the reqAddr is valid
// in the cache, return the block information. Otherwise, return nil
func (d *DirectoryImpl) Lookup(PID ca.PID, reqAddr uint64) *Block {
	set, _ := d.getSet(reqAddr)
	for _, block := range set.Blocks {
		if block.IsValid && block.Tag == reqAddr && block.PID == PID {
			return block
		}
	}
	return nil
}

// FindVictim returns a block that can be used to stored data at address addr.
//
// If it is valid, the cache controller need to decide what to do to evict the
// the data in the block
func (d *DirectoryImpl) FindVictim(addr uint64) *Block {
	set, _ := d.getSet(addr)
	block := d.victimFinder.FindVictim(set)
	return block
}

// Visit moves the block to the end of the LRUQueue
func (d *DirectoryImpl) Visit(block *Block) {
	// Pointer, not value copy: reassigning set.LRUQueue below must write the
	// updated slice header (len/cap) back to the actual Set. With a value copy
	// the header change is lost and the LRU order only survives by accident
	// (shared backing array + net-zero length + no realloc).
	set := &d.Sets[block.SetID]
	for i, b := range set.LRUQueue {
		if b == block {
			set.LRUQueue = append(set.LRUQueue[:i], set.LRUQueue[i+1:]...)
			break
		}
	}
	set.LRUQueue = append(set.LRUQueue, block)
}

// GetSets returns all the sets in a directory
func (d *DirectoryImpl) GetSets() []Set {
	return d.Sets
}

// Reset will mark all the blocks in the directory invalid
func (d *DirectoryImpl) Reset() {
	d.Sets = make([]Set, d.NumSets)
	for i := 0; i < d.NumSets; i++ {
		for j := 0; j < d.NumWays; j++ {
			block := new(Block)
			block.IsValid = false
			block.SetID = i
			block.WayID = j
			block.CacheAddress = uint64(i*d.NumWays+j) * uint64(d.BlockSize)
			d.Sets[i].Blocks = append(d.Sets[i].Blocks, block)
			d.Sets[i].LRUQueue = append(d.Sets[i].LRUQueue, block)
		}
	}
}

// WayAssociativity returns the number of ways per set in the cache.
func (d *DirectoryImpl) WayAssociativity() int {
	return d.NumWays
}
