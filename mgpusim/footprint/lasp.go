package footprint

import (
	"flag"
	"fmt"
	"strings"
)

// Grid describes one kernel launch's dispatch geometry, matching the
// globalSize/localSize pair passed to driver.LaunchKernel.
type Grid struct {
	GlobalSize [3]uint32
	LocalSize  [3]uint16
}

// NumWGX mirrors kernels.gridBuilderImpl.NumWGX.
func (g Grid) NumWGX() int {
	return int(g.GlobalSize[0]-1)/int(g.LocalSize[0]) + 1
}

// NumWGY mirrors kernels.gridBuilderImpl.NumWGY.
func (g Grid) NumWGY() int {
	return int(g.GlobalSize[1]-1)/int(g.LocalSize[1]) + 1
}

// NumWGZ mirrors kernels.gridBuilderImpl for the Z axis.
func (g Grid) NumWGZ() int {
	return int(g.GlobalSize[2]-1)/int(g.LocalSize[2]) + 1
}

// ChipletOfWG returns the chiplet that LASP dispatches work-group (idx, idy) to.
//
// This is a direct transcription of chipletLASPAlgorithm.StartNewKernel in
// timing/cp/internal/dispatching/lasp_alg.go. That assignment depends only on
// the work-group's grid index, the partition string and the chiplet count — it
// never consults CU occupancy or any timing state — so it reproduces exactly
// here.
func ChipletOfWG(idx, idy int, g Grid, partition string, numChiplets int) int {
	if numChiplets <= 0 {
		return 0
	}

	id, n := 0, 1
	switch {
	case strings.Contains(partition, "Xdiv"):
		id, n = idx, g.NumWGX()
	case strings.Contains(partition, "Ydiv"):
		id, n = idy, g.NumWGY()
	}

	base := n / numChiplets
	remained := n % numChiplets
	cutoff := remained * (base + 1)

	switch {
	case id < cutoff:
		return id / (base + 1)
	case base > 0:
		return remained + (id-cutoff)/base
	default:
		return id
	}
}

// Config carries the platform parameters a replay needs to reproduce the
// dispatch mapping. Mirrors the -xDim/-yDim/-sched-partition runner flags.
type Config struct {
	XDim, YDim int
	// Partitions holds the per-kernel -sched-partition values, in launch
	// order. A benchmark that launches more kernels than there are entries
	// reuses the last one, matching the runner's behavior.
	Partitions []string
}

// NumChiplets returns the mesh's chiplet count.
func (c Config) NumChiplets() int {
	return c.XDim * c.YDim
}

// PartitionFor returns the partition string for the i-th kernel launch.
func (c Config) PartitionFor(kernel int) string {
	if len(c.Partitions) == 0 {
		return "Xdiv"
	}

	if kernel >= len(c.Partitions) {
		return c.Partitions[len(c.Partitions)-1]
	}

	return c.Partitions[kernel]
}

// Space hands out non-overlapping, page-aligned base addresses, standing in
// for the driver's allocator.
//
// Absolute addresses do not need to match a timing run's: the trace records
// physical addresses, and virtual-to-physical translation is injective and
// page-aligned, so it maps cachelines one-to-one. Footprint sizes and per-line
// sharing degree are therefore identical either way; only the address labels
// differ.
type Space struct {
	next    uint64
	buffers []Buffer
}

// Buffer records one allocation, so a validation pass can restrict the
// comparison to a chosen buffer. A timing trace's D2H copy touches the output
// buffer from whichever chiplet ran the copy work-group, which a replay of the
// kernels alone does not model; the input buffers stay clean.
type Buffer struct {
	Name string `json:"name"`
	Base uint64 `json:"base"`
	Size uint64 `json:"size"`
}

const pageBytes = 4096

// NewSpace creates an address space starting at a nonzero, page-aligned base.
func NewSpace() *Space {
	return &Space{next: pageBytes}
}

// Alloc reserves nBytes and returns the base address.
func (s *Space) Alloc(nBytes uint64) uint64 {
	return s.AllocNamed("", nBytes)
}

// AllocNamed reserves nBytes under a name that appears in the run summary.
func (s *Space) AllocNamed(name string, nBytes uint64) uint64 {
	base := s.next
	s.next += (nBytes + pageBytes - 1) / pageBytes * pageBytes
	s.buffers = append(s.buffers, Buffer{Name: name, Base: base, Size: nBytes})

	return base
}

// Buffers lists the allocations made so far, in allocation order.
func (s *Space) Buffers() []Buffer {
	return s.buffers
}

// Replay reproduces one benchmark's global-memory address stream on the host.
//
// Implementations live next to the benchmark they replay, so they can reuse
// its own data generators and index arrays rather than reimplementing them.
type Replay interface {
	// Name is the benchmark name as it appears in scripts/params.py.
	Name() string

	// Flags binds the benchmark-specific flags, using the same names as the
	// matching samples/<bench> main so a params.py 'params' string can be
	// passed through verbatim.
	Flags(fs *flag.FlagSet)

	// Run walks the address stream the GPU kernels would produce, calling
	// r.Touch / r.TouchRange for every global-memory access.
	Run(r *Recorder, cfg Config)
}

var registry = map[string]func() Replay{}

// Register makes a replay available to the cmd/footprint tool.
func Register(name string, make func() Replay) {
	if _, dup := registry[name]; dup {
		panic(fmt.Sprintf("replay %q registered twice", name))
	}
	registry[name] = make
}

// Lookup returns the replay registered under name.
func Lookup(name string) (Replay, bool) {
	make, ok := registry[name]
	if !ok {
		return nil, false
	}

	return make(), true
}

// Registered lists every registered replay name.
func Registered() []string {
	names := make([]string, 0, len(registry))
	for name := range registry {
		names = append(names, name)
	}

	return names
}
