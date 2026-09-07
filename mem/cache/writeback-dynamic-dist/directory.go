package writeback_dynamic_dist

import (
	"math/rand/v2"

	"gitlab.com/akita/protocol"
	"gitlab.com/akita/util/ca"
)

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

	Hotness protocol.Hotness // 2bit counter for hotness, 0-3, 0 is cold, 3 is global

	// IsPte marks the resident line as a page-table entry brought in by
	// a TLB-miss page walk. Used by shouldBypassByHotness to prefer
	// PTEs over plain data on hotness ties so a PTE is not evicted by
	// an equally-ranked data line (preventing repeat page walks).
	IsPte bool

	virtualBitForLocalHome bool

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
	FindVictimWithoutDemotion(address uint64) *Block
	FindVictimAndPassedOver(address uint64) (*Block, []*Block)
	ApplyCooling(blocks []*Block)
	Visit(block *Block)
	PromoteOnHit(block *Block)
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

	// setHashShift is log2(NumSets), used by the XOR-based set hash in
	// getSet to break stride/set-period alignment from the LASP
	// allocator.
	setHashShift uint

	victimFinder VictimFinder

	// hotnessPromotePercent is the probability (0-100) that a read-hit
	// invocation of PromoteOnHit bumps the block's Hotness by one. 0
	// disables promotion; the directory then behaves exactly as before
	// this feature was added.
	hotnessPromotePercent int
}

// NewDirectory returns a new directory object. `hotnessPromotePercent` is the
// per-read-hit hotness promotion chance (0-100); pass 0 to disable.
// numBanks is the upstream stripe factor — pass 1 when there is no upstream
// stripe.
func NewDirectory(
	set, way, blockSize, numBanks int,
	victimFinder VictimFinder,
	hotnessPromotePercent int,
) *DirectoryImpl {
	if numBanks <= 0 {
		numBanks = 1
	}
	d := new(DirectoryImpl)
	d.victimFinder = victimFinder
	d.hotnessPromotePercent = hotnessPromotePercent
	d.Sets = make([]Set, set)

	d.NumSets = set
	d.NumWays = way
	d.NumBanks = numBanks
	d.BlockSize = blockSize

	d.setHashShift = 1
	for (1 << d.setHashShift) < uint(set) {
		d.setHashShift++
	}

	d.Reset()

	return d
}

// TotalSize returns the maximum number of bytes can be stored in the cache
func (d *DirectoryImpl) TotalSize() uint64 {
	return uint64(d.NumSets) * uint64(d.NumWays) * uint64(d.BlockSize)
}

// Get the set that a certain address should store at.
//
// Uses MurmurHash3 64-bit finalizer (Wang's hash) for max de-aliasing
// strength — see mem/cache/directory.go for the rationale.
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

// Lookup finds the block that reqAddr. If the reqAddr is valid
// in the cache, return the block information. Otherwise, return nil
func (d *DirectoryImpl) Lookup(PID ca.PID, reqAddr uint64) *Block {
	if PID != 1 {
		pidCheckLog("[PID-CHECK] directory.Lookup pid=%d addr=%d", PID, reqAddr)
	}
	set, _ := d.getSet(reqAddr)
	for _, block := range set.Blocks {
		if block.IsValid && block.Tag == reqAddr && block.PID == PID {
			// if block.IsValid && block.Tag == reqAddr {
			return block
		}
	}
	return nil
}

// FindVictimWithoutDemotion selects a victim slot for addr WITHOUT mutating
// any block state (no demotion, no LRU rotation). It is safe to call from
// readiness probes that may run many times before a transaction commits.
// It delegates to FindVictimAndPassedOver so a probe and the committing
// path always agree on which block is the victim.
func (d *DirectoryImpl) FindVictimWithoutDemotion(addr uint64) *Block {
	victim, _ := d.FindVictimAndPassedOver(addr)
	return victim
}

// FindVictimAndPassedOver selects a victim slot for addr WITHOUT mutating any
// block state, and additionally returns the resident hot blocks that were
// "passed over" during the LRU scan (the hot, non-locked blocks skipped to
// protect them). The caller cools that set via ApplyCooling, but ONLY once
// the eviction actually commits — decoupling hotness decay from victim
// search so speculative probes and retried commit attempts do not repeatedly
// cool the set.
//
// Selection order (front-to-back LRU), mirroring the legacy behavior minus
// the side effects:
//
//	(1) first invalid, non-locked slot — no displacement, passedOver is nil,
//	(2) else the first non-hot, non-locked block; the hot non-locked blocks
//	    ahead of it become passedOver,
//	(3) else (every non-locked block is hot) the front-most hot non-locked
//	    block is the victim and the remaining hot non-locked blocks become
//	    passedOver — this preserves the all-hot cooling release valve,
//	(4) else (every block is locked) the LRU head, no cooling; the caller's
//	    IsLocked/ReadCount guard rejects it and retries next cycle.
func (d *DirectoryImpl) FindVictimAndPassedOver(
	addr uint64,
) (*Block, []*Block) {
	set, _ := d.getSet(addr)

	// (1) prefer an invalid (empty) slot — no displacement, no cooling.
	for _, block := range set.LRUQueue {
		if !block.IsValid && !block.IsLocked {
			return block, nil
		}
	}

	// (2) first non-hot non-locked block is the victim; the hot non-locked
	// blocks scanned before it are the cooling set.
	var passedOver []*Block
	for _, block := range set.LRUQueue {
		if block.IsLocked {
			continue
		}
		if block.Hotness >= protocol.Warm {
			passedOver = append(passedOver, block)
			continue
		}
		return block, passedOver
	}

	// (3) every non-locked block is hot: force-evict the front-most one and
	// cool the rest. passedOver[0] is the front-most non-locked block.
	if len(passedOver) > 0 {
		return passedOver[0], passedOver[1:]
	}

	// (4) every block is locked.
	return set.LRUQueue[0], nil
}

// ApplyCooling demotes (Hotness -= 1) and rotates to the LRU tail each block
// in the passedOver set returned by FindVictimAndPassedOver. It is the
// deferred companion to victim selection and MUST be invoked only when the
// paired eviction actually commits, so hotness decays once per real eviction
// rather than once per (possibly repeated) victim search.
func (d *DirectoryImpl) ApplyCooling(blocks []*Block) {
	for _, block := range blocks {
		if block.Hotness >= protocol.Warm {
			block.Hotness -= 1
			d.moveToBack(block)
		}
	}
}

// Visit moves the block to the end of the LRUQueue
func (d *DirectoryImpl) Visit(block *Block) {
	d.moveToBack(block)
}

// PromoteOnHit probabilistically bumps a resident block's Hotness by one
// on read hit. The promotion only fires for blocks currently at Warm or
// Hot — Cold blocks stay Cold (so untouched data does not gain protection)
// and Global is already saturated. Bypass / Streaming and any _NonShared
// values never appear on a resident block (see bypasspolicy.go's
// storableRdmaHotness and writebufferstage.go's install paths, which
// always map _NonShared to its plain counterpart at install time), so the
// explicit Warm/Hot check is sufficient — Hotness++ here can only produce
// Hot or Global.
//
// Percent of 0 disables the feature entirely; the cache then behaves
// exactly as it did before this hook was introduced. Sister convention at
// mem/cache/writeback-dynamic-clustering-rep/directory.go:122.
func (d *DirectoryImpl) PromoteOnHit(block *Block) {
	if d.hotnessPromotePercent <= 0 {
		return
	}
	if block.Hotness != protocol.Warm && block.Hotness != protocol.Hot {
		return
	}
	if rand.IntN(100) < d.hotnessPromotePercent {
		block.Hotness++
	}
}

// moveToBack pulls a block out of its set's LRU queue and appends it
// at the tail. Used by both Visit (on hit) and the LRU scan demote path
// (when a passed-over hot block gets cooled).
func (d *DirectoryImpl) moveToBack(block *Block) {
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
			block.PID = 1
			d.Sets[i].Blocks = append(d.Sets[i].Blocks, block)
			d.Sets[i].LRUQueue = append(d.Sets[i].LRUQueue, block)
		}
	}
}

// WayAssociativity returns the number of ways per set in the cache.
func (d *DirectoryImpl) WayAssociativity() int {
	return d.NumWays
}

// A VictimFinder decides which block should be evicted
type VictimFinder interface {
	FindVictim(set *Set) *Block
	FindVictimWithoutDemotion(set *Set) *Block
}

// LRUVictimFinder evicts the least recently used block to evict
type LRUVictimFinder struct {
}

// NewLRUVictimFinder returns a newly constructed lru evictor
func NewLRUVictimFinder() *LRUVictimFinder {
	e := new(LRUVictimFinder)
	return e
}

// FindVictim walks the LRU queue front-to-back. Hot blocks encountered
// before a suitable victim are demoted (Hotness -= 1) and rotated to the
// tail — this is the cooling pressure that ensures forward progress even
// when bypass-by-hotness fires often. The first non-hot, non-locked block
// is returned as the victim. If every candidate is locked or hot, the
// (possibly already-demoted) LRU head is returned as a fallback.
//
// Iteration is over a snapshot of LRUQueue so the in-place rotations done
// by moveToBack do not perturb the visit order.
func (e *LRUVictimFinder) FindVictim(set *Set) *Block {
	moveToBack := func(block *Block) {
		for i, b := range set.LRUQueue {
			if b == block {
				set.LRUQueue = append(set.LRUQueue[:i], set.LRUQueue[i+1:]...)
				set.LRUQueue = append(set.LRUQueue, block)
				return
			}
		}
	}

	// (1) Prefer an invalid (empty) slot.
	for _, block := range append([]*Block(nil), set.LRUQueue...) {
		if !block.IsValid && !block.IsLocked {
			return block
		}
	}

	// (2) Walk LRU. Hot blocks: demote in place, rotate to tail,
	// continue. First non-hot non-locked block: return as victim.
	for _, block := range append([]*Block(nil), set.LRUQueue...) {
		if !block.IsLocked {
			if block.Hotness >= protocol.Warm {
				block.Hotness -= 1
				moveToBack(block)
				continue
			}
			return block
		}
	}

	// (3) Fallback: every candidate is locked or was hot. Return the
	// LRU head; after the rotations above it is itself a freshly
	// demoted block when the set was all-hot, satisfying the rule
	// "a hot block chosen as victim has its hotness immediately
	// lowered." If it is locked, the upper layer will retry.
	return set.LRUQueue[0]
}

func (e *LRUVictimFinder) FindVictimWithoutDemotion(set *Set) *Block {
	// moveToBack := func(block *Block) {
	// 	for i, b := range set.LRUQueue {
	// 		if b == block {
	// 			set.LRUQueue = append(set.LRUQueue[:i], set.LRUQueue[i+1:]...)
	// 			set.LRUQueue = append(set.LRUQueue, block)
	// 			return
	// 		}
	// 	}
	// }

	// (1) Prefer an invalid (empty) slot.
	for _, block := range append([]*Block(nil), set.LRUQueue...) {
		if !block.IsValid && !block.IsLocked {
			return block
		}
	}

	// (2) Walk LRU. Hot blocks: demote in place, rotate to tail,
	// continue. First non-hot non-locked block: return as victim.
	for _, block := range append([]*Block(nil), set.LRUQueue...) {
		if !block.IsLocked {
			if block.Hotness >= protocol.Warm {
				continue
			}
			return block
		}
	}

	// (3) Fallback: every candidate is locked or was hot. Return the
	// LRU head; after the rotations above it is itself a freshly
	// demoted block when the set was all-hot, satisfying the rule
	// "a hot block chosen as victim has its hotness immediately
	// lowered." If it is locked, the upper layer will retry.
	return set.LRUQueue[0]
}
