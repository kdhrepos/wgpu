package writebacksac

import (
	"gitlab.com/akita/util/global"
)

// chipRequestDirectory (CRD, SAC ISCA'23 §3.4) predicts the SM-side LLC hit
// rate while the cache runs in the memory-side configuration. During the
// profiling window it observes every demand request whose home chiplet is
// this cache's chiplet (local requests on TopPort plus inbound requests on
// RDMAPort), set-samples them, and records per requesting chiplet whether
// the line was accessed before. A repeated access by the same chiplet would
// have been a hit in that chiplet's local L2 under the SM-side
// configuration.
type chipRequestDirectory struct {
	numSets     int    // sampled sets (paper: 8)
	numWays     int    // ways per sampled set (paper: 16)
	realNumSets uint64 // sets of the real directory; sampling filter
	sets        [][]crdEntry
	useStamp    uint64 // monotonic counter for LRU within a set
}

type crdEntry struct {
	valid    bool
	tag      uint64 // cache-line aligned address
	chipMask uint64 // bit i: chiplet i accessed this line before
	lastUse  uint64
}

func newChipRequestDirectory(
	numSets, numWays int,
	realNumSets uint64,
) *chipRequestDirectory {
	if realNumSets == 0 {
		realNumSets = 1
	}
	c := &chipRequestDirectory{
		numSets:     numSets,
		numWays:     numWays,
		realNumSets: realNumSets,
		sets:        make([][]crdEntry, numSets),
	}
	for i := range c.sets {
		c.sets[i] = make([]crdEntry, numWays)
	}
	return c
}

// Reset clears all sampled entries (called at each kernel start).
func (c *chipRequestDirectory) Reset() {
	for i := range c.sets {
		for j := range c.sets[i] {
			c.sets[i][j] = crdEntry{}
		}
	}
	c.useStamp = 0
}

// Access records one demand access to lineID (cache-line aligned address)
// by the given requester chiplet, updating the global CRD counters. Only
// requests mapping to one of the sampled sets are recorded.
func (c *chipRequestDirectory) Access(
	lineID uint64,
	requester int,
	log2BlockSize uint64,
	s *global.SACState,
) {
	setIdx := (lineID >> log2BlockSize) % c.realNumSets
	if setIdx >= uint64(c.numSets) {
		return
	}
	if requester < 0 || requester >= 64 {
		return
	}

	s.CRDReqs++
	c.useStamp++

	set := c.sets[setIdx]
	bit := uint64(1) << uint(requester)

	victim := 0
	for i := range set {
		e := &set[i]
		if e.valid && e.tag == lineID {
			if e.chipMask&bit != 0 {
				// The same chiplet accessed this line before: under the
				// SM-side configuration this would hit its local L2.
				s.CRDHits++
			}
			e.chipMask |= bit
			e.lastUse = c.useStamp
			return
		}
		if !set[victim].valid {
			continue
		}
		if !e.valid || e.lastUse < set[victim].lastUse {
			victim = i
		}
	}

	set[victim] = crdEntry{
		valid:    true,
		tag:      lineID,
		chipMask: bit,
		lastUse:  c.useStamp,
	}
}
