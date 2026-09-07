package tracing

import (
	"strconv"
	"strings"
)

// parseChipletIDFromComponentName extracts the chiplet index from a component
// name shaped like "GPU0.chiplet_03.<rest>". It is used only on the cold path
// of tracers that memoize task.Where (the component name from
// where.Name()) into a numeric ID. Producers (caches) now pass numeric chiplet
// strings directly in task.What/Detail, so the per-access hot path no longer
// needs any string parsing — only task.Where, which is constant per cache
// instance, is parsed once per name.
func parseChipletIDFromComponentName(name string) int {
	parts := strings.SplitN(name, ".", 3)
	if len(parts) < 2 {
		return 0
	}
	id, _ := strconv.Atoi(strings.TrimPrefix(parts[1], "chiplet_"))
	return id
}
