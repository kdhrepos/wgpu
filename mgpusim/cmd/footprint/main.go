// Command footprint derives a benchmark's inter-chiplet sharing footprint on
// the host, without a timing simulation.
//
// Timing-run memory traces are cut short by -max-inst, so a shared footprint
// measured from them undercounts. This tool replays the same address stream the
// GPU kernels would produce, driven by the benchmark's own data generators, and
// writes the per-line aggregate that
// analyze/cacheline/shared_footprint_capacity_cdf.py consumes.
//
// Usage:
//
//	footprint -benchmark=spmv -xDim=6 -yDim=6 -sched-partition=Xdiv \
//	    -zipf-s=0.8 -params="-dim=4194304 -sparsity=0.00001" \
//	    -output-dir=$HOME/mgvm/results/footprint
package main

import (
	"flag"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"gitlab.com/akita/mgpusim/benchmarks/matrix/csr"
	"gitlab.com/akita/mgpusim/footprint"
	"gitlab.com/akita/util/global"

	// Registers the per-benchmark replays.
	_ "gitlab.com/akita/mgpusim/benchmarks/clsparse/spmm"
	_ "gitlab.com/akita/mgpusim/benchmarks/heteromark/pagerank"
	_ "gitlab.com/akita/mgpusim/benchmarks/mafiaports/gups"
	_ "gitlab.com/akita/mgpusim/benchmarks/pannotia/cc"
	_ "gitlab.com/akita/mgpusim/benchmarks/pannotia/mis"
	_ "gitlab.com/akita/mgpusim/benchmarks/pannotia/mst"
	_ "gitlab.com/akita/mgpusim/benchmarks/polybench/gemm"
	_ "gitlab.com/akita/mgpusim/benchmarks/polybench/syr2k"
	_ "gitlab.com/akita/mgpusim/benchmarks/rodinia/bptree"
	_ "gitlab.com/akita/mgpusim/benchmarks/shoc/spmv"
	_ "gitlab.com/akita/mgpusim/benchmarks/xsbench"
)

var (
	benchmark = flag.String("benchmark", "",
		"benchmark to replay, as named in scripts/params.py")
	params = flag.String("params", "",
		"benchmark-specific flags, verbatim from params.py's 'params' field")
	xDim      = flag.Int("xDim", 6, "chiplet mesh width")
	yDim      = flag.Int("yDim", 6, "chiplet mesh height")
	partition = flag.String("sched-partition", "Xdiv",
		"LASP work-group partition, comma-separated per kernel")
	zipfS = flag.Float64("zipf-s", 0,
		"Zipf skew for the CSR generator, 0 = uniform")
	seed = flag.Int64("matrix-seed", 1,
		"seed for the CSR matrix generator")
	outputDir   = flag.String("output-dir", ".", "where to write the results")
	list        = flag.Bool("list", false, "list available replays and exit")
	windowSizes = flag.String("window-sizes", "1000000,2000000,3000000,4000000,5000000,10000000,100000000",
		"comma-separated tick counts for windowed-WSS bucketing (one tick = "+
			"one line touch, see footprint.SetWindowSizes). Empty disables it.")
	stackDistanceMB = flag.String("stack-distance-thresholds-mb", defaultThresholdsMB,
		"comma-separated per-chiplet cache sizes in MB for EXACT stack-distance "+
			"hit-rate tracking (see footprint.SetStackDistanceThresholds). "+
			"Default is a power-of-2 sweep from 1 MB to 4096 MB, wide enough to "+
			"see where hit rate saturates without having to guess a range up "+
			"front; empty disables it entirely. Meaningfully more expensive "+
			"than -window-sizes (O(log ticks) per touch, one Fenwick tree per "+
			"chiplet sized to that chiplet's own tick count) — extra "+
			"thresholds are nearly free once the tree walk is already "+
			"happening, so the wide default isn't the costly part. Chiplet-"+
			"local: each chiplet's own full reference stream, shared and "+
			"private lines mixed together as it actually sees them. For the "+
			"lifetime shared/private capacity split (no stack-distance "+
			"gating), see summary.json's shared_mb_per_chiplet / "+
			"private_mb_per_chiplet / chiplet_capacity.")
)

// defaultThresholdsMB is a power-of-2 sweep from 1 MB to 4096 MB (per
// chiplet's 4GB DRAM share, so this spans everything from a tiny cache up to
// nearly a whole chiplet's memory) — wide enough to locate the hit-rate
// saturation point without a separate run to widen the range after the fact.
const defaultThresholdsMB = "1,2,4,8,16,32,64,128,256,512,1024,2048,4096"

func main() {
	flag.Parse()

	if *list {
		names := footprint.Registered()
		sort.Strings(names)
		fmt.Println(strings.Join(names, "\n"))

		return
	}

	global.GetInstance().SetZipfS(*zipfS)
	csr.SetSeed(*seed)

	cfg := footprint.Config{
		XDim:       *xDim,
		YDim:       *yDim,
		Partitions: splitPartitions(*partition),
	}

	rec := footprint.NewRecorder(cfg.NumChiplets())
	if sizes := parseWindowSizes(*windowSizes); len(sizes) > 0 {
		rec.SetWindowSizes(sizes)
	}
	if thresholds := parseStackDistanceThresholds(*stackDistanceMB); len(thresholds) > 0 {
		rec.SetStackDistanceThresholds(thresholds)
	}
	lookupReplay().Run(rec, cfg)

	write(rec)
}

// lookupReplay resolves -benchmark to a Replay and binds its flags from
// -params.
func lookupReplay() footprint.Replay {
	replay, ok := footprint.Lookup(replayFor(*benchmark))
	if !ok {
		names := footprint.Registered()
		sort.Strings(names)
		log.Fatalf("no replay for %q; available: %s",
			*benchmark, strings.Join(names, ", "))
	}

	fs := flag.NewFlagSet(*benchmark, flag.ExitOnError)
	replay.Flags(fs)
	if err := fs.Parse(strings.Fields(*params)); err != nil {
		log.Fatal(err)
	}

	return replay
}

// aliases maps a scripts/params.py benchmark name to the sample it actually
// runs. params.py defines mixtral_ffn and llama70b_ffn as the gemm sample with
// different -i/-j/-k, so one replay serves all three. Output still lands under
// the params.py name, which is what the analysis scripts iterate over.
var aliases = map[string]string{
	"mixtral_ffn":  "gemm",
	"llama70b_ffn": "gemm",
}

func replayFor(bench string) string {
	if target, ok := aliases[bench]; ok {
		return target
	}

	return bench
}

// parseWindowSizes parses -window-sizes into tick counts. Empty (or
// all-whitespace) input disables windowed-WSS tracking. A malformed entry is
// a hard error, not a silent skip — a mistyped size should fail loudly rather
// than quietly produce a summary.json with fewer windows than asked for.
func parseWindowSizes(s string) []int64 {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil
	}
	parts := strings.Split(s, ",")
	sizes := make([]int64, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		n, err := strconv.ParseInt(p, 10, 64)
		if err != nil || n <= 0 {
			log.Fatalf("-window-sizes: invalid size %q (must be a positive integer)", p)
		}
		sizes = append(sizes, n)
	}

	return sizes
}

// parseStackDistanceThresholds parses -stack-distance-thresholds-mb (MB) into
// line counts (footprint.LineBytes each). Empty disables stack-distance
// tracking, same convention as parseWindowSizes.
func parseStackDistanceThresholds(s string) []int64 {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil
	}
	parts := strings.Split(s, ",")
	lines := make([]int64, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		mb, err := strconv.ParseFloat(p, 64)
		if err != nil || mb <= 0 {
			log.Fatalf("-stack-distance-thresholds-mb: invalid size %q "+
				"(must be a positive number)", p)
		}
		lines = append(lines, int64(mb*1024*1024/footprint.LineBytes))
	}

	return lines
}

func splitPartitions(s string) []string {
	parts := strings.Split(s, ",")
	for i := range parts {
		parts[i] = strings.TrimSpace(parts[i])
	}

	return parts
}

func write(rec *footprint.Recorder) {
	dir := filepath.Join(*outputDir,
		fmt.Sprintf("%s_%dx%d", *benchmark, *xDim, *yDim))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		log.Fatal(err)
	}

	csvPath := filepath.Join(dir, "shared_lines.csv")
	if err := rec.WriteSharedCSV(csvPath); err != nil {
		log.Fatal(err)
	}

	summaryPath := filepath.Join(dir, "summary.json")
	if err := rec.WriteSummaryJSON(summaryPath); err != nil {
		log.Fatal(err)
	}

	s := rec.Summarize()
	fmt.Printf("%s: %d accesses, %d lines (%.1f MB), "+
		"%d shared lines (%.1f MB), %d private lines (%.1f MB)\n",
		*benchmark, s.TotalAccesses, s.TotalLines, s.TotalMB,
		s.SharedLines, s.SharedMB, s.PrivateLines, s.PrivateMB)
	fmt.Printf("  per-chiplet capacity: shared MB mean=%.2f median=%.2f max=%.2f | "+
		"private MB mean=%.2f median=%.2f max=%.2f  "+
		"(full %d-chiplet breakdown in summary.json)\n",
		s.ChipletCapacity.SharedMeanMB, s.ChipletCapacity.SharedMedianMB, s.ChipletCapacity.SharedMaxMB,
		s.ChipletCapacity.PrivateMeanMB, s.ChipletCapacity.PrivateMedianMB, s.ChipletCapacity.PrivateMaxMB,
		s.NumChiplets)
	for _, w := range s.Windows {
		fmt.Printf("  window=%-8d n=%-8d "+
			"shared MB: mean=%.2f median=%.2f p95=%.2f max=%.2f | "+
			"private MB: mean=%.2f median=%.2f p95=%.2f max=%.2f\n",
			w.WindowSize, w.NumWindows,
			w.SharedMeanMB, w.SharedMedianMB, w.SharedP95MB, w.SharedMaxMB,
			w.PrivateMeanMB, w.PrivateMedianMB, w.PrivateP95MB, w.PrivateMaxMB)
		if worst, ok := worstChiplet(w.PrivateByChiplet); ok {
			fmt.Printf("    private-by-chiplet worst: chiplet%d "+
				"mean=%.2f median=%.2f p95=%.2f max=%.2f  "+
				"(full %d-chiplet breakdown in summary.json)\n",
				worst.Chiplet, worst.MeanMB, worst.MedianMB, worst.P95MB, worst.MaxMB,
				len(w.PrivateByChiplet))
		}
	}
	printStackDistanceSummary(s.StackDistance)
	fmt.Printf("wrote %s\n", csvPath)
}

// printStackDistanceSummary prints one line per threshold from the
// precomputed cross-chiplet Summary (mean/median/worst/best hit rate) — the
// full per-chiplet breakdown lives in summary.json's per_chiplet, not here.
func printStackDistanceSummary(r *footprint.StackDistanceResult) {
	if r == nil {
		return
	}

	for _, t := range r.Summary {
		fmt.Printf("  stack-distance @ %.1f MB: hit-rate mean=%.4f median=%.4f  "+
			"worst=chiplet%d (%.4f) best=chiplet%d (%.4f)  "+
			"(per-chiplet detail in summary.json)\n",
			t.ThresholdMB, t.MeanHitRate, t.MedianHitRate,
			t.MinHitRateChiplet, t.MinHitRate, t.MaxHitRateChiplet, t.MaxHitRate)
	}
}

// worstChiplet is the entry with the highest mean private MB — the one that
// would blow a fixed per-chiplet L2 budget first.
func worstChiplet(byChiplet []footprint.ChipletStat) (footprint.ChipletStat, bool) {
	if len(byChiplet) == 0 {
		return footprint.ChipletStat{}, false
	}
	worst := byChiplet[0]
	for _, c := range byChiplet[1:] {
		if c.MeanMB > worst.MeanMB {
			worst = c
		}
	}

	return worst, true
}
