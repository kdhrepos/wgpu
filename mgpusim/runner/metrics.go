package runner

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

type metric struct {
	where string
	what  string
	value float64
}

type collector struct {
	metrics []metric
}

func (c *collector) Collect(where, what string, value float64) {
	c.metrics = append(c.metrics, metric{
		where: where,
		what:  what,
		value: value,
	})
}

func (c *collector) Dump(name string) {
	if err := os.MkdirAll(filepath.Dir(name), 0o755); err != nil {
		panic(err)
	}

	f, err := os.Create(name + ".csv")
	if err != nil {
		panic(err)
	}
	defer f.Close()

	fmt.Fprintf(f, "where, what, value\n")
	for _, m := range c.metrics {
		fmt.Fprintf(f, "%s, %s, %.12f\n",
			m.where, m.what, m.value)
	}

	c.metrics = c.metrics[:0]
}

// DumpFiltered writes only the metrics for which keep returns true. Unlike
// Dump it does NOT clear c.metrics — call it BEFORE Dump on the same
// collector so the full set is still available for the main file.
func (c *collector) DumpFiltered(name string, keep func(metric) bool) {
	if err := os.MkdirAll(filepath.Dir(name), 0o755); err != nil {
		panic(err)
	}

	f, err := os.Create(name + ".csv")
	if err != nil {
		panic(err)
	}
	defer f.Close()

	fmt.Fprintf(f, "where, what, value\n")
	for _, m := range c.metrics {
		if !keep(m) {
			continue
		}
		fmt.Fprintf(f, "%s, %s, %.12f\n",
			m.where, m.what, m.value)
	}
}

// isCoreMetric is the predicate used for the "_core" CSV: system-wide and
// per-chiplet aggregate metrics that summarise the run, dropping per-bank /
// per-CU / per-pair detail.
//
// Kept rows:
//   - where == "Parameters"             — run provenance header
//   - where == "driver"                 — driver-level totals (simulation_time,
//                                         total_inst_count, total-*-avg-latency, …)
//   - where contains "chiplet_all"      — cross-chiplet aggregates emitted by
//                                         the reporters (Caches, DRAM_all,
//                                         TLBs, MMU, ChipRDMA, …)
//   - where matches ^chiplet[-_]\d+$    — per-chiplet summary records
//                                         (l2-total-{hit,miss}-rate,
//                                         avg_last_WG_finish_time)
func isCoreMetric(m metric) bool {
	if m.where == "Parameters" || m.where == "driver" {
		return true
	}
	if strings.Contains(m.where, "chiplet_all") {
		return true
	}
	if (strings.HasPrefix(m.where, "chiplet_") ||
		strings.HasPrefix(m.where, "chiplet-")) &&
		!strings.ContainsAny(m.where, ". ") {
		return true
	}
	return false
}
