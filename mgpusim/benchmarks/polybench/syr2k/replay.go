package syr2k

import (
	"flag"

	"gitlab.com/akita/mgpusim/footprint"
)

// Local work-group size hardcoded in exec() (benchmark.go:144).
const (
	replayLocalX = 32
	replayLocalY = 8
)

// replay reproduces the address stream of syr2k_kernel
// (benchmarks/polybench/syr2k/native/syr2k.cl) on the host.
//
// Kernel body, with j = get_global_id(0) and i = get_global_id(1). Note that
// the kernel bounds-checks BOTH ids against nj, indexes c with stride nj, and
// indexes a and b with stride ni while running the k loop ni times — so it
// treats a and b as nj x ni matrices and c as nj x nj:
//
//	if (i < nj && j < nj) {
//		c[i*nj+j] *= beta;
//		for (k = 0; k < ni; k++)
//			c[i*nj+j] += alpha * a[i*ni+k] * b[j*ni+k]
//			           + alpha * b[i*ni+k] * a[j*ni+k];
//	}
//
// The compiled form in kernels.hsaco keeps the c accumulator in a register but
// stores it back on every iteration, so one thread issues
//
//	c[i*nj+j]  1 load + 1 store + ni stores  = ni+2 accesses
//	a[i*ni+k], b[j*ni+k], b[i*ni+k], a[j*ni+k]  ni loads each
//
// Closed form. LASP derives a work-group's chiplet from its grid index alone,
// and that index depends on exactly one axis (Xdiv -> wgx, Ydiv -> wgy). The
// threads of chiplet c are therefore the full cross product I(c) x J(c) of the
// i and j values its work-groups cover, which turns the per-access triple loop
// (~10^13 accesses at the configured size) into a per-element multiplier:
//
//	C  element (i,j), i in I and j in J     ni+2 accesses
//	A  row r, every element                 mult(r) accesses
//	B  row r, every element                 mult(r) accesses
//
//	mult(r) = (r in I ? |J| : 0) + (r in J ? |I| : 0)
//
// a and b share one multiplier because each is read at row i and at row j in
// the same iteration: row r is read once per owned j when it plays the i role,
// and once per owned i when it plays the j role. Under the configured Ydiv
// partition J(c) is every j, so every chiplet reads every row of both matrices
// — that cross-chiplet term is the sharing this replay is meant to expose.
//
// Work is O(footprint) rather than O(accesses).
type replay struct {
	ni, nj *uint
}

func init() {
	footprint.Register("syr2k", func() footprint.Replay { return &replay{} })
}

func (r *replay) Name() string { return "syr2k" }

func (r *replay) Flags(fs *flag.FlagSet) {
	r.ni = fs.Uint("ni", 64, "The height of the first matrix.")
	r.nj = fs.Uint("nj", 64, "The height of the first matrix.")
}

func (r *replay) Run(rec *footprint.Recorder, cfg footprint.Config) {
	ni, nj := int(*r.ni), int(*r.nj)
	if ni <= 0 || nj <= 0 {
		return
	}

	// Allocation order matches initMem (benchmark.go:108-136).
	space := footprint.NewSpace()
	aBase := space.Alloc(uint64(ni) * uint64(nj) * 4)
	bBase := space.Alloc(uint64(ni) * uint64(nj) * 4)
	cBase := space.Alloc(uint64(ni) * uint64(ni) * 4)

	grid := footprint.Grid{
		GlobalSize: [3]uint32{
			uint32(((nj-1)/replayLocalX + 1) * replayLocalX),
			uint32(((ni-1)/replayLocalY + 1) * replayLocalY),
			1,
		},
		LocalSize: [3]uint16{replayLocalX, replayLocalY, 1},
	}

	// The y extent is sized from ni but the guard tests i against nj, so i is
	// capped by whichever is smaller. j is capped by nj outright.
	iLimit := int(grid.GlobalSize[1])
	if nj < iLimit {
		iLimit = nj
	}
	jLimit := nj

	numChiplets := cfg.NumChiplets()
	iSpans, jSpans := ownedSpans(
		grid, cfg.PartitionFor(0), numChiplets, iLimit, jLimit)

	rowLimit := iLimit
	if jLimit > rowLimit {
		rowLimit = jLimit
	}

	// ni stores in the loop, plus the load and store of `c[...] *= beta`.
	cCount := uint64(ni) + 2

	nIs := make([]uint64, numChiplets)
	nJs := make([]uint64, numChiplets)
	inI := make([][]bool, numChiplets)
	inJ := make([][]bool, numChiplets)
	for c := 0; c < numChiplets; c++ {
		nIs[c] = uint64(spanLen(iSpans[c]))
		nJs[c] = uint64(spanLen(jSpans[c]))
		inI[c] = mark(iSpans[c], rowLimit)
		inJ[c] = mark(jSpans[c], rowLimit)
	}

	// C, A and B are all indexed by row, and A/B rows are read by every
	// chiplet under the configured Ydiv partition (that is the cross-chiplet
	// sharing this replay is meant to expose). Interleaving by row instead of
	// finishing one chiplet's whole sweep before the next keeps the touches
	// to a shared row close together in the tick stream, matching real
	// concurrent execution — stack distance is order-sensitive, so a
	// chiplet-major sweep would make the sharing look far more spread out
	// than it really is.
	for row := 0; row < rowLimit; row++ {
		for c := 0; c < numChiplets; c++ {
			if nIs[c] == 0 || nJs[c] == 0 {
				continue
			}

			if inI[c][row] {
				crow := cBase + uint64(row)*uint64(nj)*4
				for _, js := range jSpans[c] {
					touchDwords(rec, c,
						crow+uint64(js.lo)*4, js.hi-js.lo, cCount)
				}
			}

			mult := uint64(0)
			if inI[c][row] {
				mult += nJs[c]
			}
			if inJ[c][row] {
				mult += nIs[c]
			}
			if mult == 0 {
				continue
			}

			off := uint64(row) * uint64(ni) * 4
			touchDwords(rec, c, aBase+off, ni, mult)
			touchDwords(rec, c, bBase+off, ni, mult)
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
