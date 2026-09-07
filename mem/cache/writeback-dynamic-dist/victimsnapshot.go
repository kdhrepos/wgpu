package writeback_dynamic_dist

// snapshotVictim captures a stand-alone copy of a block's full restore
// state before the slot is repurposed for an incoming fetch. The returned
// Block does NOT live in the directory; it is used purely to (a) carry
// pre-eviction state into the write-back stage, and (b) restore the
// resident line if a fetch is bypassed by hotness.
func snapshotVictim(b *Block) *Block {
	return &Block{
		PID:                    b.PID,
		Tag:                    b.Tag,
		CacheAddress:           b.CacheAddress,
		DirtyMask:              b.DirtyMask,
		virtualBitForLocalHome: b.virtualBitForLocalHome,
		Hotness:                b.Hotness,
		IsValid:                b.IsValid,
		IsDirty:                b.IsDirty,
		InstallPC:              b.InstallPC,
	}
}

// restoreVictim un-clobbers a block slot back to the snapshot's state.
// Used on bypass-by-hotness so the resident line keeps its identity.
//
// IsDirty is always cleared: any in-flight writeback of the original
// dirty data is allowed to complete (the writeback reads from storage,
// which was never overwritten because the bypass path skips
// storage.Write), so the block ends up clean and coherent with memory.
func restoreVictim(block, snapshot *Block) {
	block.Tag = snapshot.Tag
	block.PID = snapshot.PID
	block.Hotness = snapshot.Hotness
	block.IsValid = snapshot.IsValid
	block.virtualBitForLocalHome = snapshot.virtualBitForLocalHome
	block.DirtyMask = snapshot.DirtyMask
	block.IsDirty = false
	block.IsLocked = false
}
