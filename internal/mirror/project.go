// project.go holds validateEmpties.

package mirror

import "fmt"

// A query flag that removes part of the answer.
//
// Some upstreams take a flag that empties one array of the answer and
// changes nothing else. The mirror stores the full answer once and empties
// the array when a caller asks. A delivery that writes the full answer then
// keeps both spellings current, and a couple of rows can never disagree.

// validateEmpties checks every <param empties=>. The flag must be a bool,
// because only true or false says whether to empty. It must not key the
// row, because the row is the same either way.
func (rt *Route) validateEmpties() error {
	for _, q := range rt.Query {
		if q.Empties == "" {
			continue
		}
		if q.Type != FieldBool {
			return fmt.Errorf("route %s parameter %q: empties= needs type=\"bool\"", rt.Path, q.Name)
		}
		if q.Key {
			return fmt.Errorf("route %s parameter %q: empties= reads one stored answer, so the parameter cannot key it", rt.Path, q.Name)
		}
	}
	return nil
}

// project empties the paths a caller's flags name. Only a path the answer
// has is emptied, so the answer stays a subset of the upstream's.
func project(rt *Route, query map[string]string, doc any) any {
	for _, q := range rt.Query {
		if q.Empties == "" || query[q.Name] != "true" {
			continue
		}
		switch d := doc.(type) {
		case map[string]any:
			emptyAt(d, q.Empties)
		case []any:
			for _, item := range d {
				if m, ok := item.(map[string]any); ok {
					emptyAt(m, q.Empties)
				}
			}
		}
	}
	return doc
}

func emptyAt(doc map[string]any, path string) {
	if lookupPath(doc, path) == nil {
		return
	}
	setPath(doc, path, []any{})
}
