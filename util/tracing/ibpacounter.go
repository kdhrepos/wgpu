// IBPA accumulators for NearFetch Inter-module Bandwidth Per Access.
// flit-hop sum is the numerator (Σ flit×hop over inter-module messages);
// local/remote LLC miss counts form the denominator N = total memory access.
// Single-threaded engine (no parallel), so plain counters suffice.
package tracing

var (
	ibpaFlitHopSum    uint64
	ibpaLocalLLCMiss  uint64
	ibpaRemoteLLCMiss uint64
)

// AddFlitHops accumulates the flit count of one inter-switch hop of a message.
func AddFlitHops(flits uint64) { ibpaFlitHopSum += flits }

// AddLocalLLCMiss counts one LLC miss served by the local DRAM partition.
func AddLocalLLCMiss() { ibpaLocalLLCMiss++ }

// AddRemoteLLCMiss counts one LLC miss forwarded to a remote chiplet.
func AddRemoteLLCMiss() { ibpaRemoteLLCMiss++ }

// GetIBPAFlitHopSum returns Σ(flit×hop) over all inter-module traversals.
func GetIBPAFlitHopSum() uint64 { return ibpaFlitHopSum }

// GetIBPALocalLLCMiss returns the local-DRAM LLC miss count.
func GetIBPALocalLLCMiss() uint64 { return ibpaLocalLLCMiss }

// GetIBPARemoteLLCMiss returns the remote-chiplet LLC miss count.
func GetIBPARemoteLLCMiss() uint64 { return ibpaRemoteLLCMiss }

// ResetIBPACounters clears all IBPA accumulators (call at benchmark start).
func ResetIBPACounters() {
	ibpaFlitHopSum = 0
	ibpaLocalLLCMiss = 0
	ibpaRemoteLLCMiss = 0
}
