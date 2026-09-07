package gemm

import (
	"flag"

	"gitlab.com/akita/mgpusim/footprint"
)

// Local work-group size hardcoded in exec() (benchmark.go:150).
const (
	replayLocalX = 32
	replayLocalY = 8
)

// replay reproduces the address stream of the gemm kernel
// (benchmarks/polybench/gemm/native/gemm.cl) on the host.
//
// Kernel body, with j = get_global_id(0) and i = get_global_id(1):
//
//	c[i*nj+j] *= beta;
//	for (k = 0; k < nk; k++)
//		c[i*nj+j] += alpha * a[i*nk+k] * b[k*nj+j];
//
// The compiled form in kernels.hsaco keeps the c accumulator in a register but
// stores it back on every iteration, so one thread issues
//
//	c[i*nj+j]  1 load + 1 store + nk stores  = nk+2 accesses
//	a[i*nk+k]  nk loads, one per k
//	b[k*nj+j]  nk loads, one per k
//
// Closed form. LASP derives a work-group's chiplet from its grid index alone,
// and that index depends on exactly one axis (Xdiv -> wgx, Ydiv -> wgy). The
// threads of chiplet c are therefore the full cross product I(c) x J(c) of the
// i and j values its work-groups cover, which turns the per-access triple loop
// (~10^13 accesses at the configured sizes) into a per-element multiplier:
//
//	C  element (i,j), i in I and j in J        nk+2 accesses
//	A  every element of row i, i in I          |J| accesses (one per owned j)
//	B  the owned j columns of every k row      |I| accesses (one per owned i)
//
// making the work O(footprint) rather than O(accesses).
//
// One replay covers the mixtral_ffn and llama70b_ffn aliases too: params.py
// points both at the gemm sample and only varies -i/-j/-k.
type replay struct {
	ni, nj, nk *int
}

func init() {
	footprint.Register("gemm", func() footprint.Replay { return &replay{} })
}

func (r *replay) Name() string { return "gemm" }

func (r *replay) Flags(fs *flag.FlagSet) {
	r.ni = fs.Int("i", 16, "Dunno")
	r.nj = fs.Int("j", 16, "Dunno")
	r.nk = fs.Int("k", 16, "Dunno")
}

func (r *replay) Run(rec *footprint.Recorder, cfg footprint.Config) {
	ni, nj, nk := *r.ni, *r.nj, *r.nk
	if ni <= 0 || nj <= 0 {
		return
	}
	if nk < 0 {
		nk = 0
	}

	// Allocation order matches initMem (benchmark.go:114-135).
	space := footprint.NewSpace()
	aBase := space.Alloc(uint64(ni) * uint64(nk) * 4)
	bBase := space.Alloc(uint64(nk) * uint64(nj) * 4)
	cBase := space.Alloc(uint64(ni) * uint64(nj) * 4)

	grid := footprint.Grid{
		GlobalSize: [3]uint32{
			uint32(((nj-1)/replayLocalX + 1) * replayLocalX),
			uint32(((ni-1)/replayLocalY + 1) * replayLocalY),
			1,
		},
		LocalSize: [3]uint16{replayLocalX, replayLocalY, 1},
	}

	numChiplets := cfg.NumChiplets()
	iSpans, jSpans := ownedSpans(
		grid, cfg.PartitionFor(0), numChiplets, ni, nj)

	// nk stores in the loop, plus the load and store of `c[...] *= beta`. With
	// nk <= 0 the loop is skipped and only those two remain.
	cCount := uint64(nk) + 2

	nIs := make([]uint64, numChiplets)
	nJs := make([]uint64, numChiplets)
	inI := make([][]bool, numChiplets)
	for c := 0; c < numChiplets; c++ {
		nIs[c] = uint64(spanLen(iSpans[c]))
		nJs[c] = uint64(spanLen(jSpans[c]))
		inI[c] = mark(iSpans[c], ni)
	}

	// C and A are both indexed by row i, and A is read in full by every
	// chiplet that owns any (i, j) work (the cross-chiplet sharing this
	// replay is meant to expose). Interleaving by row instead of finishing
	// one chiplet's whole sweep before the next keeps the touches to a
	// shared row close together in the tick stream, matching real concurrent
	// execution — stack distance is order-sensitive, so a chiplet-major
	// sweep would make the sharing look far more spread out than it is.
	for i := 0; i < ni; i++ {
		row := cBase + uint64(i)*uint64(nj)*4
		aRow := aBase + uint64(i)*uint64(nk)*4

		for c := 0; c < numChiplets; c++ {
			if nJs[c] == 0 || !inI[c][i] {
				continue
			}

			for _, js := range jSpans[c] {
				touchDwords(rec, c,
					row+uint64(js.lo)*4, js.hi-js.lo, cCount)
			}
			touchDwords(rec, c, aRow, nk, nJs[c])
		}
	}

	// B is indexed by row k and read in full by every chiplet that owns any
	// (i, j) work, so interleave it by row for the same reason.
	for k := 0; k < nk; k++ {
		row := bBase + uint64(k)*uint64(nj)*4

		for c := 0; c < numChiplets; c++ {
			if nIs[c] == 0 || nJs[c] == 0 {
				continue
			}

			for _, js := range jSpans[c] {
				touchDwords(rec, c,
					row+uint64(js.lo)*4, js.hi-js.lo, nIs[c])
			}
		}
	}
}

// span is a half-open range of grid coordinates, [lo, hi).
type span struct{ lo, hi int }

func spanLen(spans []span) int {
	n := 0
	for _, s := range spans {
		n += s.hi - s.lo
	}

	return n
}

// mark expands spans into a membership lookup over [0, limit).
func mark(spans []span, limit int) []bool {
	in := make([]bool, limit)
	for _, s := range spans {
		for v := s.lo; v < s.hi && v < limit; v++ {
			in[v] = true
		}
	}

	return in
}

// ownedSpans returns, per chiplet, the i and j values its work-groups cover.
func ownedSpans(
	grid footprint.Grid,
	partition string,
	numChiplets, iLimit, jLimit int,
) (iSpans, jSpans [][]span) {
	numWGX, numWGY := grid.NumWGX(), grid.NumWGY()
	localX, localY := int(grid.LocalSize[0]), int(grid.LocalSize[1])

	seenX := make([][]bool, numChiplets)
	seenY := make([][]bool, numChiplets)
	for c := range seenX {
		seenX[c] = make([]bool, numWGX)
		seenY[c] = make([]bool, numWGY)
	}

	// A work-group whose whole tile fails the kernel's bounds test contributes
	// no accesses, but it still occupies a slot in the LASP mapping, so it is
	// skipped here rather than trimmed out of the grid.
	for wgy := 0; wgy < numWGY; wgy++ {
		if wgy*localY >= iLimit {
			continue
		}

		for wgx := 0; wgx < numWGX; wgx++ {
			if wgx*localX >= jLimit {
				continue
			}

			c := footprint.ChipletOfWG(
				wgx, wgy, grid, partition, numChiplets)
			seenX[c][wgx] = true
			seenY[c][wgy] = true
		}
	}

	iSpans = make([][]span, numChiplets)
	jSpans = make([][]span, numChiplets)
	for c := 0; c < numChiplets; c++ {
		iSpans[c] = mergeRuns(seenY[c], localY, iLimit)
		jSpans[c] = mergeRuns(seenX[c], localX, jLimit)
	}

	return iSpans, jSpans
}

// mergeRuns turns a set of owned work-group indices into merged coordinate
// spans, clamped to the kernel's bounds test.
func mergeRuns(seen []bool, wgSize, limit int) []span {
	var out []span

	for wg := range seen {
		if !seen[wg] {
			continue
		}

		lo := wg * wgSize
		hi := lo + wgSize
		if hi > limit {
			hi = limit
		}
		if lo >= hi {
			continue
		}

		if n := len(out); n > 0 && out[n-1].hi == lo {
			out[n-1].hi = hi

			continue
		}
		out = append(out, span{lo, hi})
	}

	return out
}

// touchDwords records m accesses to each of count consecutive 4-byte elements
// starting at addr, folded to line granularity: a line holding e of those
// elements takes e*m accesses. Recorder.TouchRangeN cannot be used directly
// because it credits a line once, not once per element it holds.
func touchDwords(
	rec *footprint.Recorder,
	chiplet int,
	addr uint64,
	count int,
	m uint64,
) {
	if count <= 0 || m == 0 {
		return
	}

	end := addr + uint64(count)*4
	for a := addr; a < end; {
		lineEnd := a&^uint64(footprint.LineBytes-1) + footprint.LineBytes
		if lineEnd > end {
			lineEnd = end
		}

		rec.TouchN(chiplet, a, (lineEnd-a)/4*m)
		a = lineEnd
	}
}
