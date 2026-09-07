package footprint

import "testing"

// TestSummarize_SharedPerChiplet checks the per-chiplet shared-capacity
// breakdown against a hand-traced sequence.
//
// Lines: A=0 (addr 0), B=1 (addr 64), C=2 (addr 128). 3 chiplets.
// Sequence (chiplet, line): (0,A) (0,C) (0,B) (1,A) (0,C) (2,B) (0,A)
//
// Final classification: A touched by {0,1} -> shared. B touched by {0,2} ->
// shared. C touched only by chiplet 0 -> private.
//
// A shared line counts toward every chiplet that touched it: chiplet 0
// touched both A and B (2 shared lines), chiplet 1 touched only A (1 shared
// line), chiplet 2 touched only B (1 shared line). Chiplet 0 also owns C
// exclusively (1 private line).
func TestSummarize_SharedPerChiplet(t *testing.T) {
	const numChiplets = 3
	const lineBytes = 64
	addrOf := func(line uint64) uint64 { return line * lineBytes }
	const A, B, C = 0, 1, 2

	seq := []struct {
		chiplet int
		line    uint64
	}{
		{0, A}, {0, C}, {0, B}, {1, A}, {0, C}, {2, B}, {0, A},
	}

	r := NewRecorder(numChiplets)
	for _, ev := range seq {
		r.Touch(ev.chiplet, addrOf(ev.line))
	}

	s := r.Summarize()

	wantSharedLines := []int{2, 1, 1}
	for c, want := range wantSharedLines {
		if s.SharedLinesPerChiplet[c] != want {
			t.Errorf("SharedLinesPerChiplet[%d] = %d, want %d",
				c, s.SharedLinesPerChiplet[c], want)
		}
	}

	wantPrivateLines := []int{1, 0, 0}
	for c, want := range wantPrivateLines {
		if s.PrivateLinesPerChiplet[c] != want {
			t.Errorf("PrivateLinesPerChiplet[%d] = %d, want %d",
				c, s.PrivateLinesPerChiplet[c], want)
		}
	}

	const mb = float64(lineBytes) / (1024 * 1024)
	const eps = 1e-12

	// SharedMBPerChiplet = [2, 1, 1] * mb -> mean (2+1+1)/3, median 1, max 2.
	wantMean := (2*mb + mb + mb) / 3
	if diff := s.ChipletCapacity.SharedMeanMB - wantMean; diff > eps || diff < -eps {
		t.Errorf("ChipletCapacity.SharedMeanMB = %v, want %v",
			s.ChipletCapacity.SharedMeanMB, wantMean)
	}
	if diff := s.ChipletCapacity.SharedMedianMB - mb; diff > eps || diff < -eps {
		t.Errorf("ChipletCapacity.SharedMedianMB = %v, want %v",
			s.ChipletCapacity.SharedMedianMB, mb)
	}
	if diff := s.ChipletCapacity.SharedMaxMB - 2*mb; diff > eps || diff < -eps {
		t.Errorf("ChipletCapacity.SharedMaxMB = %v, want %v",
			s.ChipletCapacity.SharedMaxMB, 2*mb)
	}

	// PrivateMBPerChiplet = [1, 0, 0] * mb -> mean mb/3, median 0, max mb.
	wantPrivMean := mb / 3
	if diff := s.ChipletCapacity.PrivateMeanMB - wantPrivMean; diff > eps || diff < -eps {
		t.Errorf("ChipletCapacity.PrivateMeanMB = %v, want %v",
			s.ChipletCapacity.PrivateMeanMB, wantPrivMean)
	}
	if s.ChipletCapacity.PrivateMedianMB != 0 {
		t.Errorf("ChipletCapacity.PrivateMedianMB = %v, want 0",
			s.ChipletCapacity.PrivateMedianMB)
	}
	if diff := s.ChipletCapacity.PrivateMaxMB - mb; diff > eps || diff < -eps {
		t.Errorf("ChipletCapacity.PrivateMaxMB = %v, want %v",
			s.ChipletCapacity.PrivateMaxMB, mb)
	}
}
