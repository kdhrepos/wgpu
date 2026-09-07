package footprint

import "testing"

// upstreamChiplet is a verbatim copy of the assignment in
// timing/cp/internal/dispatching/lasp_alg.go's StartNewKernel. Kept separate
// from ChipletOfWG so a transcription slip in either one shows up as a
// mismatch rather than passing silently.
func upstreamChiplet(id, n, p int) int {
	base := n / p
	remained := n % p
	cutoff := remained * (base + 1)

	chiplet := 0
	if p > 0 {
		if id < cutoff {
			chiplet = id / (base + 1)
		} else if base > 0 {
			chiplet = remained + (id-cutoff)/base
		} else {
			chiplet = id
		}
	}

	return chiplet
}

func TestChipletOfWGMatchesUpstream(t *testing.T) {
	for _, p := range []int{1, 4, 16, 36, 64} {
		for _, n := range []int{1, 2, 7, 36, 37, 128, 1000, 4096} {
			grid := Grid{
				GlobalSize: [3]uint32{uint32(n), uint32(n), 1},
				LocalSize:  [3]uint16{1, 1, 1},
			}

			for id := 0; id < n; id++ {
				got := ChipletOfWG(id, 0, grid, "Xdiv", p)
				if want := upstreamChiplet(id, n, p); got != want {
					t.Fatalf("Xdiv n=%d p=%d id=%d: got %d, want %d",
						n, p, id, got, want)
				}

				got = ChipletOfWG(0, id, grid, "Ydiv", p)
				if want := upstreamChiplet(id, n, p); got != want {
					t.Fatalf("Ydiv n=%d p=%d id=%d: got %d, want %d",
						n, p, id, got, want)
				}
			}
		}
	}
}

// The dispatch must stay inside the mesh and hand each chiplet a contiguous,
// evenly sized block of work-groups — the property the footprint replay relies
// on when it assumes a chiplet owns a contiguous index range.
func TestChipletOfWGPartitionsEvenly(t *testing.T) {
	const p = 36

	for _, n := range []int{36, 37, 71, 512, 4097} {
		grid := Grid{
			GlobalSize: [3]uint32{uint32(n), 1, 1},
			LocalSize:  [3]uint16{1, 1, 1},
		}

		counts := make([]int, p)
		prev := -1
		for id := 0; id < n; id++ {
			c := ChipletOfWG(id, 0, grid, "Xdiv", p)
			if c < 0 || c >= p {
				t.Fatalf("n=%d id=%d: chiplet %d out of range", n, id, c)
			}
			if c < prev {
				t.Fatalf("n=%d id=%d: assignment not monotonic (%d after %d)",
					n, id, c, prev)
			}
			prev = c
			counts[c]++
		}

		if n < p {
			continue
		}

		lo, hi := counts[0], counts[0]
		for _, c := range counts {
			if c < lo {
				lo = c
			}
			if c > hi {
				hi = c
			}
		}
		if hi-lo > 1 {
			t.Fatalf("n=%d: work-group counts unbalanced, min %d max %d",
				n, lo, hi)
		}
	}
}

func TestGridWGCounts(t *testing.T) {
	g := Grid{
		GlobalSize: [3]uint32{4194304, 1, 1},
		LocalSize:  [3]uint16{128, 1, 1},
	}

	if got, want := g.NumWGX(), 32768; got != want {
		t.Fatalf("NumWGX = %d, want %d", got, want)
	}
	if got, want := g.NumWGY(), 1; got != want {
		t.Fatalf("NumWGY = %d, want %d", got, want)
	}

	// Non-multiple global size must round up, as gridBuilderImpl does.
	g = Grid{
		GlobalSize: [3]uint32{100, 1, 1},
		LocalSize:  [3]uint16{64, 1, 1},
	}
	if got, want := g.NumWGX(), 2; got != want {
		t.Fatalf("NumWGX = %d, want %d", got, want)
	}
}

func TestRecorderCountsAndSharing(t *testing.T) {
	r := NewRecorder(4)

	r.Touch(0, 0)
	r.Touch(1, 8)  // same line as addr 0
	r.Touch(1, 64) // next line
	r.TouchN(2, 64, 10)

	s := r.Summarize()
	if s.TotalAccesses != 13 {
		t.Fatalf("TotalAccesses = %d, want 13", s.TotalAccesses)
	}
	if s.TotalLines != 2 {
		t.Fatalf("TotalLines = %d, want 2", s.TotalLines)
	}
	if s.SharedLines != 2 {
		t.Fatalf("SharedLines = %d, want 2", s.SharedLines)
	}
	if s.SharedAccesses != 13 {
		t.Fatalf("SharedAccesses = %d, want 13", s.SharedAccesses)
	}

	// An access straddling a line boundary reaches the L2 as two accesses.
	r = NewRecorder(2)
	r.TouchRange(0, 60, 8)
	if s := r.Summarize(); s.TotalLines != 2 || s.TotalAccesses != 2 {
		t.Fatalf("straddling access: lines %d accesses %d, want 2 and 2",
			s.TotalLines, s.TotalAccesses)
	}
}

// The table must survive growth past its initial capacity without losing or
// merging lines — it rehashes in place at load factor 0.5.
func TestRecorderGrowth(t *testing.T) {
	const n = 3_000_000

	r := NewRecorder(8)
	for i := uint64(0); i < n; i++ {
		r.Touch(int(i%8), i*LineBytes)
	}

	s := r.Summarize()
	if s.TotalLines != n {
		t.Fatalf("TotalLines = %d, want %d", s.TotalLines, n)
	}
	if s.TotalAccesses != n {
		t.Fatalf("TotalAccesses = %d, want %d", s.TotalAccesses, n)
	}
	if s.SharedLines != 0 {
		t.Fatalf("SharedLines = %d, want 0", s.SharedLines)
	}
}
