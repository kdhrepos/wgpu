// LookupPathTracer records the actual chiplet-visit sequence of each L2-bypass
// lookup chain on the dynamic-dist(-prefetch) platform.
//
// Unlike plain X-Y routing (captured by HopTracer as a src->dst pair), a lookup
// request is steered through the local homes that lie on the requestor->global-
// home minimal path, so the chain of chiplets it actually visits matters. Each
// finished chain emits one "lookup-path" task whose Detail encodes the real
// sequence the request traversed:
//
//   - "hit-<R>-<LH0>-...-<LHk>-g<GH>": the chain hit at local home LHk; the
//     listed LHs are the ones actually visited (requestor first, hitting LH
//     last). The address's global home GH is shown as "g<id>" for reference;
//     the lookup never actually reached it, so "hit-" means the lookup succeeded.
//   - "miss-<R>-<LH0>-...-<LHn>-g<GH>": every probed local home missed and the
//     request fell back to the global home GH (flagged separately as "g<id>").
//
// Both forms end with "-g<GH>"; the "hit-"/"miss-" prefix is what tells them
// apart (success at an LH vs fallback to the GH).
//
// The tracer counts how many lookups followed each distinct path, so hit vs
// miss and the visited chiplets are both readable straight from the path key.
package tracing

// LookupPathTracer counts lookup chains per distinct chiplet-visit sequence.
type LookupPathTracer struct {
	paths map[string]uint64
}

func NewLookupPathTracer() *LookupPathTracer {
	return &LookupPathTracer{paths: make(map[string]uint64)}
}

func (t *LookupPathTracer) StartTask(task Task) {
	if task.Kind != "lookup-path" {
		return
	}
	path, ok := task.Detail.(string)
	if !ok {
		return
	}
	t.paths[path]++
}

func (t *LookupPathTracer) StepTask(task Task) {}
func (t *LookupPathTracer) EndTask(task Task)  {}

// GetPaths returns the visit-sequence -> count map.
func (t *LookupPathTracer) GetPaths() map[string]uint64 {
	return t.paths
}
