// Package runner defines how default benchmark samples are executed.
package runner

import (
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"runtime/debug"
	"runtime/pprof"
	"time"

	// Enable profiling
	_ "net/http/pprof"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/tebeka/atexit"
	"gitlab.com/akita/akita"
	"gitlab.com/akita/mem"

	// ram "gitlab.com/akita/mem/dram"

	"gitlab.com/akita/mem/idealmemcontroller"
	"gitlab.com/akita/mem/vm/tlb"
	"gitlab.com/akita/mgpusim/benchmarks"
	"gitlab.com/akita/mgpusim/driver"
	"gitlab.com/akita/mgpusim/platform"
	"gitlab.com/akita/mgpusim/rdma"
	"gitlab.com/akita/util/global"
	"gitlab.com/akita/util/tracing"
)

var benchmarkName = flag.String("benchmark", "unknown", "The name of the benchmark to run, used for naming the output files")
var timingFlag = flag.Bool("timing", false, "Run detailed timing simulation.")
var maxInstCount = flag.Uint64("max-inst", 0,
	"Terminate the simulation after the given number of instructions is retired.")
var cpuProfileFile = flag.String("cpuprofile", "",
	"If set, write a pprof CPU profile of the whole simulation to this file.")
var memProfileFile = flag.String("memprofile", "",
	"If set, write a pprof heap profile at simulation end to this file.")
var checkpoint = flag.String("checkpoint", "",
	"Comma-separated integer percentages (1-99) of -max-inst at which to "+
		"dump extra metrics snapshots. e.g. -checkpoint=25,50,75 dumps three "+
		"snapshots. Each snapshot is saved as <-metric-file-name>_<n>%.csv "+
		"(e.g. metrics_25%.csv). Requires -max-inst > 0.")
var checkpointFileName = flag.String("checkpoint-file-name", "",
	"Comma-separated prefixes matching -checkpoint 1:1 (sorted ascending). "+
		"The k-th prefix receives the dump for the k-th percentage. "+
		"\".csv\" is appended. If empty, defaults to <-metric-file-name>_<n>% "+
		"for every percentage. Useful for placing each checkpoint dump in a "+
		"separate directory so post-run housekeeping is not required.")
var coreFilenameFlag = flag.String("core-file-name", "",
	"If set, dump a curated subset of aggregate metrics (driver totals, "+
		"chiplet_all cross-chiplet aggregates, per-chiplet l2 hit-rate "+
		"and WG finish time, plus the Parameters provenance header) to "+
		"<core-file-name>.csv. Leaves the main metric CSV untouched. If "+
		"empty (default), no core CSV is written.")
var checkpointCoreFileName = flag.String("checkpoint-core-file-name", "",
	"Comma-separated prefixes matching -checkpoint 1:1 (sorted ascending). "+
		"The k-th prefix receives the core-dump for the k-th percentage. "+
		"\".csv\" is appended. If empty, no checkpoint core CSV is written. "+
		"Useful for placing the checkpoint core dumps in separate directories.")

// Parsed at Init() from the comma-separated flag strings above.
// checkpointPercents is sorted ascending and deduped. The two file-path maps
// hold per-percent overrides; missing entries fall back to the default naming.
var (
	checkpointPercents      []int
	checkpointFilePaths     map[int]string
	checkpointCoreFilePaths map[int]string
)

// parseCheckpointFlags parses -checkpoint, -checkpoint-file-name, and
// -checkpoint-core-file-name from their comma-separated form into the
// package-level slices/maps consumed by the inst tracer and report code.
// Called once from Init() after flag.Parse() has run.
func parseCheckpointFlags() {
	checkpointPercents = nil
	checkpointFilePaths = nil
	checkpointCoreFilePaths = nil

	raw := strings.TrimSpace(*checkpoint)
	if raw == "" {
		return
	}

	seen := map[int]bool{}
	for _, s := range strings.Split(raw, ",") {
		s = strings.TrimSpace(s)
		if s == "" {
			continue
		}
		v, err := strconv.Atoi(s)
		if err != nil {
			log.Fatalf("invalid -checkpoint percentage %q: %v "+
				"(expected integer 1-99)", s, err)
		}
		if v <= 0 || v >= 100 {
			log.Fatalf("-checkpoint percentage out of range: %d "+
				"(expected 1-99)", v)
		}
		if seen[v] {
			continue
		}
		seen[v] = true
		checkpointPercents = append(checkpointPercents, v)
	}
	sort.Ints(checkpointPercents)

	checkpointFilePaths = parseCheckpointPathSpec(
		*checkpointFileName, "checkpoint-file-name")
	checkpointCoreFilePaths = parseCheckpointPathSpec(
		*checkpointCoreFileName, "checkpoint-core-file-name")
}

func parseCheckpointPathSpec(spec, flagName string) map[int]string {
	spec = strings.TrimSpace(spec)
	if spec == "" {
		return nil
	}
	parts := strings.Split(spec, ",")
	if len(parts) != len(checkpointPercents) {
		log.Fatalf("-%s has %d entries but -checkpoint has %d; "+
			"counts must match", flagName,
			len(parts), len(checkpointPercents))
	}
	m := make(map[int]string, len(parts))
	for i, p := range parts {
		m[checkpointPercents[i]] = strings.TrimSpace(p)
	}
	return m
}

var parallelFlag = flag.Bool("parallel", false,
	"Run the simulation in parallel.")
var isaDebug = flag.Bool("debug-isa", false, "Generate the ISA debugging file.")
var visTracing = flag.Bool("trace-vis", false,
	"Generate trace for visualization purposes.")
var l2tlbsqlTracingFlag = flag.Bool("trace-l2sql", false,
	"Generate trace from L2 TLB into a new MySql database.")
var verifyFlag = flag.Bool("verify", false, "Verify the emulation result.")
var memTracing = flag.Bool("trace-mem", false, "Generate memory trace")
var memTraceFilename = flag.String("mem-trace-filename", "mem.trace", "Give the name of the memory trace file")
var tlbTracing = flag.Bool("trace-tlb", false, "Generate memory trace")
var tlbTraceFilename = flag.String("tlb-trace-filename", "tlb.trace", "Give the name of the tlb trace file")
var disableProgressBar = flag.Bool("no-progress-bar", false,
	"Disables the progress bar")
var instCountReportFlag = flag.Bool("report-inst-count", false,
	"Report the number of instructions executed in each compute unit.")
var cacheLatencyReportFlag = flag.Bool("report-cache-latency", false,
	"Report the average cache latency.")
var cacheHitRateReportFlag = flag.Bool("report-cache-hit-rate", false,
	"Report the cache hit rate of each cache.")
var tlbLatencyReportFlag = flag.Bool("report-tlb-latency", false,
	"Report the average tlb latency.")
var tlbConditionalStatsReportFlag = flag.Bool("report-tlb-conditional", false,
	"Report the tlb stats about how much is remote and how much is local.")
var tlbHitRateReportFlag = flag.Bool("report-tlb-hit-rate", false,
	"Report the tlb hit rate of each tlb.")
var tlbCoalesceReportFlag = flag.Bool("report-tlb-coalesce", false,
	"Report the average tlb coalesce.")
var rtuCoalesceReportFlag = flag.Bool("report-rtu-coalesce", false,
	"Report the average amount of coalescing possible at the RTU.")
var pagewalkLatencyReportFlag = flag.Bool("report-pagewalk-latency", false,
	"Report the average page walk latency.")
var mmuConditionalStatsReportFlag = flag.Bool("report-mmu-conditional", false,
	"Report the mmu stats about how much is remote and how much is local.")
var activeWalkerReportFlag = flag.Bool("report-active-walkers", false,
	"Report the average acitve page walker count.")
var dramLatencyReportFlag = flag.Bool("report-dram-latency", false,
	"Report the average dram latency.")
var pwcHitRateReportFlag = flag.Bool("report-pwc-hit-rate", false,
	"Report the pwc hit rate of each level.")
var rdmaTransactionCountReportFlag = flag.Bool("report-rdma-transaction-count",
	false, "Report the number of transactions going through the RDMA engines.")
var cdmaTransactionCountReportFlag = flag.Bool("report-cdma-transaction-count",
	false, "Report the number of transactions through the chip RDMA engines.")
var rtuTransactionCountReportFlag = flag.Bool("report-rtu-transaction-count",
	false, "Report the number of transactions through the remote trans. engines.")
var dramTransactionCountReportFlag = flag.Bool("report-dram-transaction-count",
	false, "Report the number of transactions accessing the DRAMs.")
var translationReqLatencyFlag = flag.Bool("report-translation-request-latency",
	false, "Report the latency of  requests thrugh the remote trans. engines.")
var addressTranslatorLatencyFlag = flag.Bool("report-address-translator-latency",
	false, "Report the latency of  requests thrugh the addr. trans.")
var l2tlbQueueingImbalanceFlag = flag.Bool("report-l2tlb-queueing-imbalance",
	false, "Report the imbalnce of  requests thrugh the L2 TLB .")
var pageWalkerImbalanceFlag = flag.Bool("report-pagewalker-imbalance",
	false, "Report the imbalnce of  use of the Page walker.")
var entropyFlag = flag.Bool("report-entropy",
	false, "Report the entropy of requests are various L2 TLBs")
var referenceTracingFlag = flag.Bool("report-reference-tracing",
	false, "Report the number of references to differnet addresses")
var gpuFlag = flag.String("gpus", "",
	"The GPUs to use, use a format like 1,2,3,4. By default, GPU 1 is used.")
var unifiedGPUFlag = flag.String("unified-gpus", "",
	`Run multi-GPU benchmark in a unified mode.
Use a format like 1,2,3,4. Cannot coexist with -gpus.`)
var useUnifiedMemoryFlag = flag.Bool("use-unified-memory", false,
	"Run benchmark with Unified Memory or not")
var reportAll = flag.Bool("report-all", false, "Report all metrics to .csv file.")
var reportEssential = flag.Bool("report-essential", false, "Report essential metrics to .csv file.")
var reportWarpStallFlag = flag.Bool("report-warp-stall", false,
	"Report per-CU warp stall breakdown and average memory request latency.")
var filenameFlag = flag.String("metric-file-name", "metrics",
	"Modify the name of the output csv file.")
var platformType = flag.String("platform-type", "mcmgpu",
	"Name of the platform to build (ideal, mcmgpu, distlb, etc)")
var schedulingAlg = flag.String("scheduling", "chiplet-aware",
	"Name of the scheduling algorithm to use (chiplet-aware, greedy, round-robin, lasp)")
var useCoalescingRTU = flag.Bool("use-coalescing-rtu", false,
	"Specify if the RTU should be coalescing.")
var memAllocatorType = flag.String("mem-allocator-type", "interleaved",
	"Specify the type of memory allocator.")
var fastH2D = flag.Bool("fast-h2d", false,
	"Skip CP/DMA/DRAM simulation for Host-to-Device copies and write "+
		"bytes directly into chiplet DRAM storage. H2D traffic becomes "+
		"invisible to -trace-mem and related metrics.")
var useLASPMemoryAllocFlag = flag.Bool("use-lasp-mem-alloc", false,
	"Set this if we are going to use the LASP memory allocation API in applications.")
var useLASPHSLMemoryAllocFlag = flag.Bool("use-lasp-hsl-mem-alloc", false,
	"Set this if we are going to use HSL directed rearranged of data.")
var pteCaching = flag.Bool("use-pte-caching", true,
	"Specify if page table pages should be cached locally")
var log2PageSize = flag.Uint64("log2-page-size", 12,
	"Specify the page size")

// Dual-stripe interleaving is hard-wired via CommonBuilder:
//   - bank stripe    = log2CacheLineSize             (64B by default)
//   - chiplet stripe = numBanksPerChiplet × pageSize (64KB by default,
//     derived from those two — not independently tunable)
//
// The chiplet stripe must equal the LASP allocator's per-chiplet chunk size
// (mem/device/devicelaspmemstate.go:setInitialAddress), so it is auto-derived
// via CommonBuilder.chipletInterleavingSize(). To experiment with the bank
// stripe, call WithLog2MemoryBankInterleavingSize on the builder.
var useCustomHSL = flag.Bool("use-custom-hsl", false,
	"Specify if to use custom HSL as defined in program")
var customHSL = flag.Uint64("custom-hsl", 1,
	"Specify the value of custom HSL directly to the builder")
var logDataAllocation = flag.Bool("log-data-allocation", false,
	"Specify the data allocation logging interval")
var logDataAllocationFilename = flag.String(
	"log-data-allocation-filename",
	"",
	"Specify the filename for data allocation logging")

// wafer-scale gpu flags
var xDim = flag.Int("xDim", 2, "Specify the x dimension size of wafer-scale gpu")
var yDim = flag.Int("yDim", 2, "Specify the y dimension size of wafer-scale gpu")
var numReqPerCycle = flag.Int("num-req-per-cycle", 16, "Specify the interconnect bandwidth (default: 32, 1TBs)")
var switchLatency = flag.Int("switch-latency", 32, "Specify the interconnect latency in ns (default: 32, 32ns)")

// Experimental knobs for the "expose hop-count gap" study. Defaults leave
// behaviour unchanged; raise one at a time to ablate which on-chiplet
// throughput knob lets the inter-chiplet network dominate.
var l2MSHR = flag.Int("l2-mshr", 32,
	"L2 cache MSHR entries per bank. Default 32.")
var zipfS = flag.Float64("zipf-s", 0.0,
	"Zipf skew for the CSR matrix/graph generator (spmv/pagerank/pannotia). "+
		"0 = uniform (default); ~1.0 = moderate power-law hubs.")
var vmemEgressWidth = flag.Int("vmem-egress-width", 16,
	"CU vector-mem egress lanes per cycle (transactions/cycle toward ROB). Default 1.")
var robNumReqPerCycle = flag.Int("rob-num-req-per-cycle", 32,
	"L1V ROB pipeline width: topDown/parseBottom/bottomUp ops per cycle. Default 4.")
var l1vCacheNumReqs = flag.Int("l1v-cache-num-reqs", 32,
	"L1V cache requests accepted per cycle from the ROB. Default 2.")
var atNumReqPerCycle = flag.Int("at-num-req-per-cycle", 32,
	"Address translator req/cycle (translate/sendToBottom/parseTranslation/respond). Default 4.")
var robBufferSize = flag.Int("rob-buffer-size", 16384,
	"L1V ROB MLP window: max in-flight vector memory transactions per CU. Default 128 (hw); 16384 for hop-gap study.")
var oooDebug = flag.Bool("ooo-debug", false,
	"Enable [ooo-dbg]/[ooo-bug] verbose prints in CU vmem path for OOO ROB diagnosis.")
var prioritizePTE = flag.Bool("prioritize-pte", false,
	"L2: prioritize PTW (page-table walk) requests over data within each destination tier. Default true (current behaviour); set false to ablate.")
var robPerWfSubFifo = flag.Bool("rob-per-wf-sub-fifo", true,
	"L1V/L1S/L1I ROBs: use per-wavefront sub-FIFOs so cross-WF head-of-line blocking is eliminated while per-WF program order is preserved. Default true (HoL-free); set false for single-FIFO in-order fallback.")

// custom tracer
var accessCountPerAddrsTraceFlag = flag.Bool("trace-access-count-per-addrs",
	false, "Trace access counts per addresses generated from L1 caches")
var accessCountPerAddrsTraceFileName = flag.String("trace-access-count-per-addrs-filename",
	"access_count", "Filename for tracing access counts per addresses generated from L1 caches")
var memoryAccessTraceFlag = flag.Bool("trace-memory-access",
	false, "Trace access counts per addresses generated from L1 caches")
var memoryAccessTraceFileName = flag.String("trace-memory-access-filename",
	"memory_access", "Filename for tracing memory accesses")
var cachelineHopStatsFlag = flag.Bool("trace-cacheline-hop-stats",
	true, "Accumulate per-cache-line demand-access-count and real-hop-distance "+
		"stats for the whole run, and add a top-K% hop-distance percentile report "+
		"to the main metric-file-name CSV at exit")
var pteAccessTraceFlag = flag.Bool("trace-pte-access",
	false, "Trace the accesses to the page table entries")
var pteAccessTraceFileName = flag.String("trace-pte-access-filename",
	"pte_access", "Filename for tracing the accesses to the page table entries")
var setAccessTraceFlag = flag.Bool("trace-set-access",
	false, "Emit per-L2-bank per-setID access counts into the metrics CSV")
var reportWorkgroupCountFlag = flag.Bool("report-workgroup-count",
	false, "Report the number of workgroups")
var reportHopPerRDMAFlag = flag.Bool("report-hop-count-per-rdma",
	false, "Report average & total hop count per rdma")
var tracePortQueueFlag = flag.Bool("trace-port-queue",
	false, "Report per-port queueing delay (Recv→Retrieve wait time) "+
		"at endpoint NetworkPorts and RDMA engine ports")

// verbose setting for console
var verbose = flag.Bool("verbose",
	false, "Print messages on console (default: false)")
var logDir = flag.String("log-dir",
	"", "")

// refresh per n instructions
var refresh = flag.Int("refresh",
	0, "Refresh all the components per certain number of instructions (default: 0)")

// time-limit for fast simulation
var timeLimit = flag.Int("time-limit",
	0, "Time limit for fast simulation in seconds (default: 0, no time limit)")

// LASP parameters
var schedulingPartitions = flag.String("sched-partition", "Xdiv", "Specify the scheduling partitions")
var dataPartitionings = flag.String("data-partitioning", "kernel-wide", "Specify the data partitioning")
var dataStrides = flag.String("data-strides", "0", "Specify the data strides for stride-aware partitioning, separated by comma")
var colSizeForLasp = flag.Int("col-size-for-lasp", 0, "Specify the column size for LASP scheduling")

var terminated bool
var lastKernelLaunchTime akita.VTimeInSec

type verificationPreEnablingBenchmark interface {
	benchmarks.Benchmark

	EnableVerification()
}

type instCountTracer struct {
	tracer *instTracer
	cu     akita.Component
}

type cacheLatencyTracer struct {
	tracer *tracing.AverageTimeTracer
	cache  akita.Component
}

type cacheHitRateTracer struct {
	tracer *tracing.StepCountTracer
	cache  akita.Component
}

type TLBLatencyTracer struct {
	tracer *tracing.AverageTimeTracer
	tlb    tracing.NamedHookable
}

type TLBPipelineLatencyTracer struct {
	tracer   *tracing.AverageTimeTracer
	pipeline tracing.NamedHookable
}

type L2PipelineLatencyTracer struct {
	tracer   *tracing.AverageTimeTracer
	pipeline tracing.NamedHookable
}

type ConditionalRemoteTLBLatencyTracer struct {
	tracer *tracing.ConditionalAverageTimeTracer
	tlb    akita.Component
}

type AddressTranslatorLatencyTracer struct {
	tracer            *tracing.ConditionalAverageTimeTracer
	addressTranslator akita.Named
}

type MMUMemoryLatencyTracer struct {
	tracer *tracing.ConditionalAverageTimeTracer
	mmu    akita.Component
}

type TLBHitRateTracer struct {
	tracer *tracing.StepCountTracer
	tlb    tracing.NamedHookable
}

type CDMAAccessTracer struct {
	tracer     *tracing.StepCountTracer
	cdmaEngine akita.Component
}

type PWCHitRateTracer struct {
	tracer *tracing.StepCountTracer
	mmu    akita.Component
}

type PageWalkLatencyTracer struct {
	tracer *tracing.AverageTimeTracer
	mmu    akita.Component
}

type ActivePageWalkerTracer struct {
	tracer *tracing.AverageCountTracer
	mmu    akita.Component
}

type RemoteTLBLatencyTracer struct {
	tracer *tracing.AverageTimeTracer
	rtu    akita.Component
}

type DRAMLatencyTracer struct {
	tracer *tracing.AverageTimeTracer
	dram   akita.Component
}

type dramTransactionCountTracer struct {
	tracer *tracing.AverageTimeTracer
	dram   *idealmemcontroller.Comp
	// dram *dram.MemController
}

type rdmaTransactionCountTracer struct {
	outgoingTracer *tracing.AverageTimeTracer
	incomingTracer *tracing.AverageTimeTracer
	rdmaEngine     rdma.RDMAEngine
}

type TLBQueueImbalanceTracer struct {
	tracer *tlb.GlobalTLBQueueingTracer
	tlb    tlb.L2TLB
}

type EntropyTracer struct {
	tracer *tlb.EntropyTracer
	tlb    tlb.L2TLB
}

type TLBSetMissTracer struct {
	tracer *tracing.ReferenceTracer
	tlb    tlb.L2TLB
}

type TLBMSHRStallTracer struct {
	tracer *tracing.TotalTimeTracer
	tlb    tlb.L2TLB
}

type TLBReqStallTracer struct {
	tracer *tracing.StepCountTracer
	tlb    tlb.L2TLB
}

type L1TLBMSHRWaitTracerEntry struct {
	tracer *tracing.TLBMSHRWaitTracer
	tlb    *tlb.TLB
}

type L2TLBMSHRWaitTracerEntry struct {
	tracer *tracing.TLBMSHRWaitTracer
	tlb    tlb.L2TLB
}

// Runner is a class that helps running the benchmarks in the official samples.
type Runner struct {
	Engine                  akita.Engine
	GPUDriver               *driver.Driver
	maxInstStopper          *instTracer
	KernelTimeCounter       *tracing.BusyTimeTracer
	PerGPUKernelTimeCounter []*tracing.BusyTimeTracer
	InstCountTracers        []instCountTracer
	CacheLatencyTracers     []cacheLatencyTracer
	TLBLatencyTracers       []TLBLatencyTracer
	DownTLBLatencyTracers   []TLBLatencyTracer
	// L2PipelineLatencyTracers           []L2PipelineLatencyTracer
	// L2PipelineLocalLatencyTracers      []L2PipelineLatencyTracer
	// L2PipelineRemoteLatencyTracers     []L2PipelineLatencyTracer
	TLBPipelineLatencyTracers          []TLBPipelineLatencyTracer
	MMUMemoryLatencyTracers            []MMUMemoryLatencyTracer
	PageWalkLatencyTracers             []PageWalkLatencyTracer
	DRAMLatencyTracers                 []DRAMLatencyTracer
	AddressTranslatorLatencyTracers    []AddressTranslatorLatencyTracer
	CacheHitRateTracers                []cacheHitRateTracer
	RTUCoalescingTracers               [][][][]*tracing.AverageCountTracer
	TLBHitRateTracers                  []TLBHitRateTracer
	PWCHitRateTracers                  []PWCHitRateTracer
	TranslationReqTracer               *tracing.TranslationReqTracer
	ConditionalRemoteTLBLatencyTracers []ConditionalRemoteTLBLatencyTracer
	RDMATransactionCounters            []rdmaTransactionCountTracer
	CDMATransactionCounters            []rdmaTransactionCountTracer
	PageTransactionCounters            []rdmaTransactionCountTracer
	CDMAAccessTracers                  []CDMAAccessTracer
	PageAccessTracers                  []CDMAAccessTracer
	DRAMTransactionCounters            []dramTransactionCountTracer
	RemoteTLBLatencyTracers            []RemoteTLBLatencyTracer
	L2TLBBufLenTracers                 []*tracing.AverageCountTracer
	L2TLBBufLenG0Tracers               []*tracing.AverageCountTracer
	L2TLBCoalesceAddrTracers           []*tracing.AverageCountTracer
	L2TLBCoalesceTracers               []*tracing.AverageCountTracer
	L2TLBMSHRLenTracers                []*tracing.AverageCountTracer
	L2TLBMSHRUniqLenTracers            []*tracing.AverageCountTracer
	L2TLBMSHRLenG0Tracers              []*tracing.AverageCountTracer
	L2TLBMSHRUniqLenG0Tracers          []*tracing.AverageCountTracer
	ActivePageWalkerTracers            []ActivePageWalkerTracer
	L2TLBMySQLTracer                   tracing.MySQLTracer
	L2TLBQueueingImbalanceTracers      []TLBQueueImbalanceTracer
	EntropyTracers                     []EntropyTracer
	TLBSetMissTracers                  []TLBSetMissTracer
	TLBMSHRStallTracers                []TLBMSHRStallTracer
	TLBReqStallTracers                 []TLBReqStallTracer
	L1TLBMSHRWaitTracers               []L1TLBMSHRWaitTracerEntry
	L2TLBMSHRWaitTracers               []L2TLBMSHRWaitTracerEntry
	Benchmarks                         []benchmarks.Benchmark
	Timing                             bool
	Verify                             bool
	Parallel                           bool
	ReportInstCount                    bool
	ReportCacheLatency                 bool
	ReportTLBLatency                   bool
	ReportTLBConditionalStats          bool
	ReportMMUConditionalStats          bool
	ReportPageWalkLatency              bool
	ReportDRAMLatency                  bool
	ReportTranslationReqLatency        bool
	ReportAddressTranslatorLatency     bool
	ReportCacheHitRate                 bool
	ReportTLBCoalesce                  bool
	ReportRTUCoalesce                  bool
	ReportTLBHitRate                   bool
	ReportPWCHitRate                   bool
	ReportL2TLBMSHRLen                 bool
	ReportDRAMTransactionCount         bool
	ReportRDMATransactionCount         bool
	ReportCDMATransactionCount         bool
	ReportRTUTransactionCount          bool
	ReportActiveWalkerCount            bool
	L2TLBSQLTracing                    bool
	ReportL2TLBQueueImbalance          bool
	ReportPageWalkerImbalance          bool
	ReportEntropy                      bool
	ReportReferenceTracing             bool
	ReportTLBSetMissTracing            bool
	ReportTLBMSHRStallTracing          bool
	ReportTLBReqStalls                 bool
	ReportTLBMSHRWait                  bool
	ReportWarpStall                    bool
	UseUnifiedMemory                   bool
	UseLASPMemoryAlloc                 bool
	UseLASPHSLMemoryAlloc              bool
	UseCustomHSL                       bool
	metricsCollector                   *collector

	// custom tracing
	SimulationStartTime            time.Time
	TraceAccessCountPerAddrs       bool
	TraceMemoryAccess              bool
	TraceCacheLineHopStats         bool
	TracePteAccess                 bool
	ReportWorkgroupCount           bool
	ReportHopPerRDMA               bool
	ReportL2MissTrace              bool
	ReportMSHRFailTrace            bool
	WorkgroupCountPerKernelTracer  *tracing.WorkgroupCountPerKernelTracer
	WorkgroupDimensionTracer       *tracing.WorkgroupDimensionTracer
	WorkgroupCountPerChipletTracer *tracing.WorkgroupCountPerChipletTracer
	HopTracer                      *tracing.HopTracer
	NetworkFlightTracer            *tracing.NetworkFlightTracer
	RDMAFlightTracer               *tracing.RDMAFlightTracer
	SendOutFailTracer              *tracing.SendOutFailTracer
	PortQueueTracer                *tracing.PortQueueTracer
	TracePortQueue                 bool
	RemotePtwTracer                *tracing.RemotePtwTracer
	MemoryUsageTracer              *tracing.MemoryUsageTracer
	L2MissTracer                   *tracing.L2MissTracer
	L1MSHRFailTracer               *tracing.MSHRFailTracer
	L2MSHRFailTracer               *tracing.MSHRFailTracer
	AccessCountPerAddrTracer       *tracing.AccessCountTracer
	MemoryAccessTracer             *tracing.MemoryAccessTracer
	CacheLineHopTracer             *tracing.CacheLineHopTracer
	PteAccessTracer                *tracing.PteAccessTracer
	KernelTracer                   *tracing.KernelTracer
	FlushTracer                    *tracing.FlushTracer
	NumFlitTracer                  *tracing.NumFlitTracer
	PteCachingTracer               *tracing.PteCachingTracer
	EvictTracer                    *tracing.EvictTracer
	LocalRemoteTracer              *tracing.LocalRemoteTracer

	GPUIDs []int
}

func parseLists() {
	// per kernel
	instance := global.GetInstance()

	schedPartitions := strings.Split(*schedulingPartitions, ",")
	dataStrides := strings.Split(*dataStrides, ",")
	var strides []int
	for _, s := range dataStrides {
		val, _ := strconv.Atoi(strings.TrimSpace(s))
		strides = append(strides, val)
	}

	for _, schedPartition := range schedPartitions {
		trimmed := strings.TrimSpace(schedPartition)
		instance.AddWgPartition(trimmed)
	}

	dataPartitionings := strings.Split(*dataPartitionings, ",")
	for _, dataPartitioning := range dataPartitionings {
		trimmed := strings.TrimSpace(dataPartitioning)
		instance.AddDataPartitioning(trimmed)
	}

	// FIXME: only one column size is supported for now.
	instance.SetColSizeForLasp(*colSizeForLasp)

	instance.SetDataStrides(strides)

	// set benchmark name
	instance.SetBenchmarkName(*benchmarkName)

	// experimental hardware knobs (default values = no behaviour change)
	instance.SetL2MSHREntry(*l2MSHR)
	instance.SetZipfS(*zipfS)
	instance.SetVmemEgressWidth(*vmemEgressWidth)
	instance.SetROBNumReqPerCycle(*robNumReqPerCycle)
	instance.SetL1VCacheNumReqs(*l1vCacheNumReqs)
	instance.SetATNumReqPerCycle(*atNumReqPerCycle)
	instance.SetROBBufferSize(*robBufferSize)
	instance.SetVerbose(*verbose)
	instance.SetOOODebug(*oooDebug)
	instance.SetPrioritizePTEAtL2(*prioritizePTE)
	instance.SetROBPerWfSubFifo(*robPerWfSubFifo)
}

// ParseFlag applies the runner flag to runner object
//
//nolint:gocyclo
func (r *Runner) ParseFlag() *Runner {

	if *benchmarkName == "unknown" {
		*benchmarkName = filepath.Base(os.Args[0])
	}

	if *filenameFlag == "metrics" {
		*filenameFlag = fmt.Sprintf("%s_%s_%dx%d",
			*benchmarkName, *platformType, *xDim, *yDim)
	}

	if *parallelFlag {
		r.Parallel = true
	}

	if *verifyFlag {
		r.Verify = true
	}

	if *timingFlag {
		r.Timing = true
	}

	if *useUnifiedMemoryFlag {
		r.UseUnifiedMemory = true
	}

	if *useLASPMemoryAllocFlag {
		r.UseLASPMemoryAlloc = true
	}

	if *useLASPHSLMemoryAllocFlag {
		r.UseLASPHSLMemoryAlloc = true
	}

	if *useCustomHSL {
		r.UseCustomHSL = true
	}

	if *instCountReportFlag {
		r.ReportInstCount = true
	}

	if *cacheLatencyReportFlag {
		r.ReportCacheLatency = true
	}

	if *tlbLatencyReportFlag {
		r.ReportTLBLatency = true
	}

	if *tlbConditionalStatsReportFlag {
		r.ReportTLBConditionalStats = true
	}

	if *mmuConditionalStatsReportFlag {
		r.ReportMMUConditionalStats = true
	}

	if *pagewalkLatencyReportFlag {
		r.ReportPageWalkLatency = true
	}

	if *dramLatencyReportFlag {
		r.ReportDRAMLatency = true
	}

	if *translationReqLatencyFlag {
		r.ReportTranslationReqLatency = true
	}

	if *addressTranslatorLatencyFlag {
		r.ReportAddressTranslatorLatency = true
	}

	if *cacheHitRateReportFlag {
		r.ReportCacheHitRate = true
	}

	if *tlbHitRateReportFlag {
		r.ReportTLBHitRate = true
	}

	if *rtuCoalesceReportFlag {
		r.ReportRTUCoalesce = true
	}

	if *pwcHitRateReportFlag {
		r.ReportPWCHitRate = true
	}

	if *dramTransactionCountReportFlag {
		r.ReportDRAMTransactionCount = true
	}

	if *rdmaTransactionCountReportFlag {
		r.ReportRDMATransactionCount = true
	}

	if *cdmaTransactionCountReportFlag {
		r.ReportCDMATransactionCount = true
	}

	if *rtuTransactionCountReportFlag {
		r.ReportRTUTransactionCount = true
	}

	if *tlbCoalesceReportFlag {
		r.ReportTLBCoalesce = true
	}

	if *activeWalkerReportFlag {
		r.ReportActiveWalkerCount = true
	}

	if *l2tlbsqlTracingFlag {
		r.L2TLBSQLTracing = true
	}

	if *l2tlbQueueingImbalanceFlag {
		r.ReportL2TLBQueueImbalance = true
	}

	if *pageWalkerImbalanceFlag {
		r.ReportPageWalkerImbalance = true
	}

	if *entropyFlag {
		r.ReportEntropy = true
	}

	if *referenceTracingFlag {
		r.ReportReferenceTracing = true
	}

	// custom tracer
	if *accessCountPerAddrsTraceFlag {
		r.TraceAccessCountPerAddrs = true
	}

	if *memoryAccessTraceFlag {
		r.TraceMemoryAccess = true
	}

	if *cachelineHopStatsFlag {
		r.TraceCacheLineHopStats = true
	}

	if *pteAccessTraceFlag {
		r.TracePteAccess = true
	}

	// if *pcStateTraceFlag {
	// 	r.TracePCState = true
	// }

	if *reportWorkgroupCountFlag {
		r.ReportWorkgroupCount = true
	}

	if *reportHopPerRDMAFlag {
		r.ReportHopPerRDMA = true
	}

	if *tracePortQueueFlag {
		r.TracePortQueue = true
	}

	if *reportWarpStallFlag {
		r.ReportWarpStall = true
	}

	if *reportAll {
		r.ReportInstCount = true
		r.ReportWarpStall = true
		r.ReportCacheLatency = true
		// r.ReportTLBLatency = true
		// r.ReportPageWalkLatency = true
		r.ReportDRAMLatency = true
		r.ReportCacheHitRate = true
		// r.ReportTLBHitRate = true
		// r.ReportPWCHitRate = true
		r.ReportDRAMTransactionCount = true
		r.ReportRDMATransactionCount = true
		r.ReportCDMATransactionCount = true
		// r.ReportAddressTranslatorLatency = true
		r.ReportReferenceTracing = true

		// r.ReportTranslationReqLatency = true
		// r.ReportTLBConditionalStats = true
		// r.ReportMMUConditionalStats = true
		// r.ReportTLBCoalesce = true
		// r.ReportRTUCoalesce = true
		// r.ReportActiveWalkerCount = true
		// r.ReportRTUTransactionCount = true
		// r.ReportL2TLBMSHRLen = true
		// r.ReportL2TLBQueueImbalance = true
		// r.ReportPageWalkerImbalance = true
		// r.ReportEntropy = true
		// r.ReportTLBSetMissTracing = true
		// r.ReportTLBMSHRStallTracing = true
		// r.ReportTLBReqStalls = true
		// r.ReportTLBMSHRWait = true

		// custom tracer
		r.ReportWorkgroupCount = true
		r.ReportHopPerRDMA = true
		r.ReportL2MissTrace = true
		r.ReportMSHRFailTrace = true
		r.TracePortQueue = true

	}

	return r
}

func (r *Runner) startProfilingServer() {
	listener, err := net.Listen("tcp", ":0")
	if err != nil {
		panic(err)
	}

	fmt.Println("Profiling server running on:",
		listener.Addr().(*net.TCPAddr).Port)

	panic(http.Serve(listener, nil))
}

// Init initializes the platform simulate
func (r *Runner) Init() *Runner {
	go r.startProfilingServer()

	parseLists()
	parseCheckpointFlags()
	r.ParseFlag()

	l := global.GetInstance()
	l.SetVerbose(*verbose)
	l.LogDataAllocation = *logDataAllocation
	l.LogDataAllocationFilename = *logDataAllocationFilename

	log.SetFlags(log.Llongfile | log.Ldate | log.Ltime)

	if *timeLimit > 0 {
		fmt.Println("Time limit is set to ", *timeLimit, " hours.")
		l.SetTimeLimit(*timeLimit)
	}

	if len(checkpointPercents) > 0 {
		labels := make([]string, len(checkpointPercents))
		for i, p := range checkpointPercents {
			labels[i] = fmt.Sprintf("%d%%", p)
		}
		fmt.Printf("Checkpoint enabled: dumping snapshots at %s of -max-inst.\n",
			strings.Join(labels, ", "))
	}

	if *fastH2D {
		fmt.Println("Fast H2D enabled: bypassing timing simulation for H2D copies.")
	}

	if r.Timing {
		r.buildTimingPlatform()
	} else {
		// r.buildEmuPlatform()
		r.buildTimingPlatform()
	}

	r.parseGPUFlag()

	r.metricsCollector = &collector{}
	r.addMaxInstStopper()
	r.addKernelTimeTracer()
	r.addInstCountTracer()
	r.enableWarpStallTracing()
	r.addCacheLatencyTracer()
	r.addDRAMLatencyTracer()
	r.addDRAMTracer()
	r.addCacheHitRateTracer()
	r.addRDMAEngineTracer()
	r.addCDMAEngineTracer()
	// r.addPageWalkLatencyTracer()
	// r.addTLBLatencyTracer()
	// r.addTLBHitRateTracer()
	// r.addPWCHitRateTracer()
	// r.addAddressTranslatorLatencyTracer()

	// r.addTLBConditionalTracer()
	// r.addTLBCoalesceTracer()
	// r.addL2TLBMSHRLenTracer()
	// r.addRTUCoalesceTracer()
	// r.addMMUConditionalTracer()
	// r.addRTUTracer()
	// r.addActiveWalkerTracer()
	// r.addL2TLBQueueingImbalanceTracer()
	// r.addL2TLBEntropyTracer()
	// r.addRemoteReferenceTracer()
	// r.addTLBSetMissTracer()
	// r.addTLBMSHRStallTracer()
	// r.addTLBReqStallTracer()
	// r.addPageWalkerImbalanceTracker()
	// r.addL2TLBSQLTracer()
	// if *platformType != "ideal" {
	// r.addTranslationReqTracer()
	// }

	// custom tracer
	r.addMemoryUsageTracer()
	r.addWorkgroupCountTracer()
	r.addHopTracer()
	r.addRDMAFlightTracer()
	r.addSendOutFailTracer()
	r.addPortQueueTracer()
	// r.addRemoteRtwTracer()
	r.addL2MissTracer()
	r.addMSHRFailTracer()
	// r.addTLBMSHRWaitTracer()
	r.addAccessCountPerAddrTracer()
	r.addMemoryAccessTracer(*memoryAccessTraceFileName)
	r.addCacheLineHopTracer()
	// r.addPteCachingTracer()
	// r.addPteAccessTracer(*pteAccessTraceFileName)
	r.addEvictTracer() // custom tracer
	r.addKernelTracer()
	r.addWorkgroupDimensionTracer()
	r.addFlushTracer()
	r.addNumFlitTracer()
	r.addLocalRemoteTracer() // custom tracer for local vs. remote access

	atexit.Register(func() { runWithStack("reportStats", r.reportStats) })

	return r
}

// runWithStack invokes the given function and, on panic, prints the panic
// value plus the full goroutine stack to stderr before re-panicking. This
// surfaces the original failure site when an atexit handler crashes, since
// the tebeka/atexit package recovers panics without a stack trace.
func runWithStack(name string, fn func()) {
	defer func() {
		if err := recover(); err != nil {
			fmt.Fprintf(os.Stderr,
				"panic in atexit handler %q: %v\n%s\n",
				name, err, debug.Stack())
			panic(err)
		}
	}()
	fn()
}

func (r *Runner) buildTimingPlatform() {
	fmt.Println(*platformType, *schedulingAlg)
	fmt.Println("GPU Size: ", *xDim, *yDim)

	xDim := *xDim // pointer -> value
	yDim := *yDim // pointer -> value

	var b platform.Builder

	switch *platformType {
	case "mesh":
		b = platform.MakeMeshGPUPlatformBuilder()
	case "ideal":
		b = platform.MakeIdealGPUPlatformBuilder()
	default:
		panic("Runner.go: wrong platform selected")
	}

	if *isaDebug {
		b.WithISADebugging()
	}

	if *disableProgressBar {
		b.WithoutProgressBar()
	}

	b.WithPteCaching(*pteCaching)

	// mesh
	b.WithGridSize(xDim, yDim)

	// interconnect bandwidth
	b.WithNumReqPerCycle(*numReqPerCycle)

	// interconnect latency
	b.WithSwitchLatency(*switchLatency)

	// lowAddr is fixed as 1GB
	b.WithTotalMemSize(uint64(xDim*yDim) * 4 * mem.GB)

	// scheduling
	b.WithAlg(*schedulingAlg)

	b.WithMemAllocatorType(*memAllocatorType)
	b.WithLog2PageSize(*log2PageSize)
	b.WithPC(true)
	r.Engine, r.GPUDriver = b.Build()
	r.GPUDriver.FastH2D = *fastH2D
}

func (r *Runner) parseGPUFlag() {
	if *gpuFlag == "" && *unifiedGPUFlag == "" {
		r.GPUIDs = []int{1}
		return
	}

	if *gpuFlag != "" && *unifiedGPUFlag != "" {
		panic("cannot use -gpus and -unified-gpus together")
	}

	if *unifiedGPUFlag != "" {
		gpuIDs := r.gpuIDStringToList(*unifiedGPUFlag)
		unifiedGPUID := r.GPUDriver.CreateUnifiedGPU(nil, gpuIDs)
		r.GPUIDs = []int{unifiedGPUID}
		return
	}

	gpuIDs := r.gpuIDStringToList(*gpuFlag)
	r.GPUIDs = gpuIDs
}

func (r *Runner) gpuIDStringToList(gpuIDsString string) []int {
	gpuIDs := make([]int, 0)
	gpuIDTokens := strings.Split(gpuIDsString, ",")
	for _, t := range gpuIDTokens {
		gpuID, err := strconv.Atoi(t)
		if err != nil {
			panic(err)
		}
		gpuIDs = append(gpuIDs, gpuID)
	}
	return gpuIDs
}

// AddBenchmark adds an benchmark that the driver runs
func (r *Runner) AddBenchmark(b benchmarks.Benchmark) {
	b.SelectGPU(r.GPUIDs)
	if r.UseUnifiedMemory {
		b.SetUnifiedMemory()
	}
	if r.UseLASPMemoryAlloc {
		b.SetLASPMemoryAlloc()
	}

	// if r.UseLASPHSLMemoryAlloc {
	// 	b.SetLASPHSLMemoryAlloc()
	// }
	// if r.UseCustomHSL {
	// 	b.SetCustomHSL()
	// }
	r.Benchmarks = append(r.Benchmarks, b)
}

// AddBenchmarkWithoutSettingGPUsToUse allows for user specified GPUs for
// the benchmark to run.
func (r *Runner) AddBenchmarkWithoutSettingGPUsToUse(b benchmarks.Benchmark) {
	if r.UseUnifiedMemory {
		b.SetUnifiedMemory()
	}
	if r.UseLASPMemoryAlloc {
		b.SetLASPMemoryAlloc()
	}
	r.Benchmarks = append(r.Benchmarks, b)
}

// Run runs the benchmark on the simulator
func (r *Runner) Run() {
	r.SimulationStartTime = time.Now()

	if *cpuProfileFile != "" {
		f, err := os.Create(*cpuProfileFile)
		if err != nil {
			log.Fatalf("could not create CPU profile %q: %v", *cpuProfileFile, err)
		}
		if err := pprof.StartCPUProfile(f); err != nil {
			log.Fatalf("could not start CPU profile: %v", err)
		}
		// Stop via atexit: the max-inst stopper exits through atexit.Exit
		// (insttracer.go), so a deferred stop here would never run.
		atexit.Register(func() {
			pprof.StopCPUProfile()
			f.Close()
		})
	}

	if *memProfileFile != "" {
		f, err := os.Create(*memProfileFile)
		if err != nil {
			log.Fatalf("could not create mem profile %q: %v", *memProfileFile, err)
		}
		atexit.Register(func() {
			runtime.GC()
			if err := pprof.WriteHeapProfile(f); err != nil {
				log.Printf("could not write mem profile: %v", err)
			}
			f.Close()
		})
	}

	// Here, driver schedules its event (tick) and run the engine.
	// Then, the engine runs driver's event.
	r.GPUDriver.Run()

	var wg sync.WaitGroup
	for _, b := range r.Benchmarks {
		t := reflect.TypeOf(b)

		if t.Kind() == reflect.Ptr {
			t = t.Elem()
		}

		fullPkgPath := t.PkgPath()
		segments := strings.Split(fullPkgPath, "/")
		pkgName := segments[len(segments)-1]

		fmt.Printf("Benchmark Name: %s\n", pkgName)

		wg.Add(1)
		go func(b benchmarks.Benchmark, wg *sync.WaitGroup) {
			if r.Verify {
				if b, ok := b.(verificationPreEnablingBenchmark); ok {
					b.EnableVerification()
				}
			}

			// Benchmark starts from here.
			// It initiates memory and allocates data structures.
			// Finally, kernel is launched by the driver.
			b.Run()

			if r.Verify {
				b.Verify()
			}
			wg.Done()
		}(b, &wg)
	}
	wg.Wait()

	r.GPUDriver.Terminate()
	r.Engine.Finished()

	//r.reportStats()

	// r.portSanityCheck()

	atexit.Exit(0)
}

func (r *Runner) reportStats() {
	r.collectAllMetrics()

	if *maxInstCount != 0 &&
		r.maxInstStopper.logger != nil &&
		r.maxInstStopper.logger.Writer() != nil {
		if c, ok := r.maxInstStopper.logger.Writer().(io.Closer); ok {
			_ = c.Close()
		}
	}

	r.dumpMetrics()
}

// reportStatsCheckpoint collects the same metrics as reportStats and dumps
// them to <-metric-file-name>_<percent>%.csv, leaving the main file untouched.
// When per-percent overrides are supplied via -checkpoint-file-name /
// -checkpoint-core-file-name, the matching entry is used instead.
func (r *Runner) reportStatsCheckpoint(percent int) {
	r.collectAllMetrics()

	name := fmt.Sprintf("%s_%d%%", *filenameFlag, percent)
	if override, ok := checkpointFilePaths[percent]; ok && override != "" {
		name = override
	}
	if override, ok := checkpointCoreFilePaths[percent]; ok && override != "" {
		r.metricsCollector.DumpFiltered(override, isCoreMetric)
	}
	r.metricsCollector.Dump(name)
}

func (r *Runner) collectAllMetrics() {
	r.reportAllParameters()  // custom tracer
	r.reportSimulationTime() // custom tracer
	r.reportExecutionTime()
	r.reportKernelTime()         // custom tracer
	r.reportMemoryUsage()        // custom tracer
	r.reportWorkgroupCount()     // custom tracer
	r.reportDispatchProgress()   // always-on WG progress (no flag)
	r.reportWorkgroupDimension() // custom tracer
	r.reportInstCount()
	r.reportWarpStall()
	r.reportDRAMTransactionCount()
	r.reportPageWalkLatency()
	r.reportCacheLatency()
	r.reportCacheHitRate()
	r.reportLocalRemoteCount() // custom tracer
	r.reportDRAMLatency()
	r.reportRDMATransactionCount()
	r.reportCDMATransactionCount()
	r.reportHopPerRDMA()        // custom tracer
	r.reportIBPA()              // IBPA (always reported)
	r.reportRDMAFlightLatency() // custom tracer
	r.reportSendOutFail()       // custom tracer
	r.reportPortQueue()         // custom tracer
	r.reportLinkUtilization()   // custom tracer
	// r.reportRemotePtw()          // custom tracer
	r.reportL2MissTrace()   // custom tracer
	r.reportMSHRFailTrace() // custom tracer
	// r.reportTLBMSHRWait()        // custom tracer
	r.reportFlushMetrics()       // custom tracer
	r.reportNumFlits()           // custom tracer
	r.reportMemAllocPerChiplet() // custom tracer
	// r.reportTLBLatency()
	// r.reportTLBHitRate()
	// r.reportPWCHitRate()
	// r.reportPteCaching() // custom tracer
	// r.reportAddressTranslatorLatency()
	r.reportEvictMetrics() // custom tracer

	// r.reportTLBConditionalStats()
	// r.reportMMUConditionalStats()
	// r.reportActiveWalkerCount()
	// r.reportRTUTransactionCount()
	// r.ReportRTUCoalescing()
	// r.reportL2TLBQueueImbalance()
	// r.reportPageWalkerImbalance()
	// r.reportL2TLBEntropy()
	// r.reportReferenceTracing()
	// r.reportTLBSetMissTracing()
	// r.reportTLBMSHRStallTracing()
	// r.reportTLBReqStalls()

	r.reportSetAccessHistogram() // per-L2-bank setID access histogram

	// custom tracer
	if r.TraceAccessCountPerAddrs {
		r.reportAccessCountPerAddrs(*accessCountPerAddrsTraceFileName)
	}
	if r.TraceMemoryAccess {
		r.reportMemoryAccessTraces()
	}
	if r.TraceCacheLineHopStats {
		r.reportCacheLineHopStats()
	}
	if r.TracePteAccess {
		r.reportPteAccessTraces()
	}
}

func (r *Runner) dumpMetrics() {
	if *coreFilenameFlag != "" {
		r.metricsCollector.DumpFiltered(*coreFilenameFlag, isCoreMetric)
	}
	r.metricsCollector.Dump(*filenameFlag)
}
