package tracing

import (
	"testing"

	"gitlab.com/akita/protocol"
)

func TestPerPCLookupTracer_NearfetchPath(t *testing.T) {
	tr := NewPerPCLookupTracer()
	tr.StartTask(Task{Kind: "data-forwarding-hit", Detail: &DataForwardingDetail{PC: 100}})
	tr.StartTask(Task{Kind: "data-forwarding-mshr-hit", Detail: &DataForwardingDetail{PC: 100}})
	tr.StartTask(Task{Kind: "data-forwarding-miss", Detail: &DataForwardingDetail{PC: 100}})
	tr.StartTask(Task{Kind: "data-forwarding-reached-gh", Detail: &DataForwardingDetail{PC: 100}})
	// PTW must be skipped.
	tr.StartTask(Task{Kind: "data-forwarding-hit", Detail: &DataForwardingDetail{PC: 100, IsPtw: true}})

	snap := tr.Snapshot()
	c := snap[100]
	if c.Hit != 1 || c.MshrHit != 1 || c.Miss != 1 || c.ReachedGH != 1 {
		t.Fatalf("nearfetch counts wrong: %+v", c)
	}
	if _, has := snap[0]; has {
		t.Fatalf("PTW leaked into PC=0 bucket: %+v", snap[0])
	}
}

func TestPerPCLookupTracer_DynClusteringPath(t *testing.T) {
	tr := NewPerPCLookupTracer()
	mk := func(pc uint64, ptw bool) *protocol.ReadReq {
		r := protocol.ReadReqBuilder{}.Build()
		r.Info = pc
		r.IsPtw = ptw
		return r
	}
	tr.StartTask(Task{Kind: "lookup-hit", Detail: mk(200, false)})
	tr.StartTask(Task{Kind: "lookup-mshr-hit", Detail: mk(200, false)})
	tr.StartTask(Task{Kind: "lookup-distribution-hit", Detail: mk(200, false)})
	tr.StartTask(Task{Kind: "lookup-miss", Detail: mk(200, false)})
	tr.StartTask(Task{Kind: "lookup-hit", Detail: mk(0, true)}) // PTW

	snap := tr.Snapshot()
	c := snap[200]
	if c.Hit != 1 || c.MshrHit != 1 || c.DistHit != 1 || c.Miss != 1 {
		t.Fatalf("dyn-clustering counts wrong: %+v", c)
	}
}

func TestPCFromInfo(t *testing.T) {
	if PCFromInfo(uint64(42)) != 42 {
		t.Fatalf("uint64 cast failed")
	}
	if PCFromInfo(nil) != 0 {
		t.Fatalf("nil should be 0")
	}
	if PCFromInfo(int(5)) != 0 {
		t.Fatalf("non-uint64 should be 0")
	}
}
