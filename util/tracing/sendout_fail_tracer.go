// SendOutFailTracer counts how many cycles each switch's sendOut stage
// failed to push a flit to the downstream link, i.e. how often the
// pipeline stalled. Counts are aggregated per switch and per (switch,
// port). A high count points to a saturated downstream link or to
// backpressure from the next-hop switch.
package tracing

const SendOutFailKind = "sendout-fail"

type SendOutFailTracer struct {
	filter    TaskFilter
	perSwitch map[string]uint64
	perPort   map[string]map[string]uint64
}

func NewSendOutFailTracer(filter TaskFilter) *SendOutFailTracer {
	return &SendOutFailTracer{
		filter:    filter,
		perSwitch: make(map[string]uint64),
		perPort:   make(map[string]map[string]uint64),
	}
}

func (t *SendOutFailTracer) StartTask(task Task) {
	if !t.filter(task) {
		return
	}

	sw := task.Where
	port, _ := task.Detail.(string)

	t.perSwitch[sw]++
	if t.perPort[sw] == nil {
		t.perPort[sw] = make(map[string]uint64)
	}
	t.perPort[sw][port]++
}

func (t *SendOutFailTracer) StepTask(_ Task) {}
func (t *SendOutFailTracer) EndTask(_ Task)  {}

// GetTotal returns the sum across all switches.
func (t *SendOutFailTracer) GetTotal() uint64 {
	var n uint64
	for _, v := range t.perSwitch {
		n += v
	}
	return n
}

// GetPerSwitch returns a copy of per-switch totals keyed by switch name.
func (t *SendOutFailTracer) GetPerSwitch() map[string]uint64 {
	out := make(map[string]uint64, len(t.perSwitch))
	for k, v := range t.perSwitch {
		out[k] = v
	}
	return out
}

// GetPerPort returns a copy of per-switch, per-port totals. Outer key is
// switch name; inner key is the local port name on that switch.
func (t *SendOutFailTracer) GetPerPort() map[string]map[string]uint64 {
	out := make(map[string]map[string]uint64, len(t.perPort))
	for sw, m := range t.perPort {
		inner := make(map[string]uint64, len(m))
		for k, v := range m {
			inner[k] = v
		}
		out[sw] = inner
	}
	return out
}
