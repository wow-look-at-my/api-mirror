package mirror

import (
	"fmt"
	"slices"
	"strings"
)

// validatePath checks the parameters a route path declares. A rest parameter
// binds everything after it, so it can only end the path. A <match> on a name
// the path does not bind constrains nothing.
func (rt *Route) validatePath(params []string) error {
	segs := strings.Split(strings.Trim(rt.Path, "/"), "/")
	for i, seg := range segs {
		if _, _, rest := pathParam(seg); rest && i != len(segs)-1 {
			return fmt.Errorf("route %s: %s binds the rest of the path, so it must be the last segment", rt.Path, seg)
		}
	}
	for _, m := range rt.Match {
		if !slices.Contains(params, m.Param) {
			return fmt.Errorf("route %s: <match param=%q> names no path parameter", rt.Path, m.Param)
		}
	}
	return nil
}
