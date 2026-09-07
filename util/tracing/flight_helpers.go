// Helpers shared by NetworkFlightTracer and RDMAFlightTracer: both parse
// the same chiplet name format ("...chiplet_NN...") from Task.Where.
package tracing

import (
	"strconv"
	"strings"
)

func chipletIdFromWhere(where string) int {
	parts := strings.Split(where, ".")
	if len(parts) < 2 {
		return 0
	}
	id, _ := strconv.Atoi(strings.TrimPrefix(parts[1], "chiplet_"))
	return id
}
