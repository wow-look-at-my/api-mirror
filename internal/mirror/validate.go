package mirror

import (
	"fmt"
	"slices"
	"strings"
	"time"
)

// Duration parses a Go duration string, rejecting the empty and the negative.
func parseDuration(what, s string) (time.Duration, error) {
	d, err := time.ParseDuration(s)
	if err != nil {
		return 0, fmt.Errorf("%s: %q is not a duration: %w", what, s, err)
	}
	if d <= 0 {
		return 0, fmt.Errorf("%s: %q must be positive", what, s)
	}
	return d, nil
}

// validate rejects a spec the engine cannot serve honestly. Every check here
// exists because its absence is a silent failure at runtime: a resource nobody
// gated, a route pointing at nothing, an invalidation with no stated reason.
func (s *Spec) validate() error {
	if strings.TrimSpace(s.Name) == "" {
		return fmt.Errorf("<mirror> needs a name")
	}
	if strings.TrimSpace(s.Upstream.Base) == "" {
		return fmt.Errorf("<upstream> needs a base URL")
	}
	byName := make(map[string]*Resource, len(s.Resources))
	for _, r := range s.Resources {
		if err := r.validate(); err != nil {
			return err
		}
		if _, dup := byName[r.Name]; dup {
			return fmt.Errorf("resource %q declared twice", r.Name)
		}
		byName[r.Name] = r
	}
	for _, rt := range s.Routes {
		if err := rt.validate(byName); err != nil {
			return err
		}
	}
	if s.Events != nil {
		if err := s.Events.validate(byName); err != nil {
			return err
		}
	}
	return nil
}

func (r *Resource) validate() error {
	if strings.TrimSpace(r.Name) == "" {
		return fmt.Errorf("<resource> needs a name")
	}
	if len(r.Keys) == 0 {
		return fmt.Errorf("resource %q needs at least one <key>: a fact with no identity cannot be stored once", r.Name)
	}
	seen := make([]string, 0, len(r.Keys)+len(r.Fields))
	for _, k := range r.Keys {
		if k.Name == "" {
			return fmt.Errorf("resource %q: <key> needs a name", r.Name)
		}
		if slices.Contains(seen, k.Name) {
			return fmt.Errorf("resource %q: %q declared twice", r.Name, k.Name)
		}
		seen = append(seen, k.Name)
	}
	switch r.Store {
	case StoreColumns:
		if len(r.Fields) == 0 {
			return fmt.Errorf("resource %q stores columns but declares no <field>: it would serve an empty answer", r.Name)
		}
	case StoreDocument:
		if len(r.Fields) > 0 {
			return fmt.Errorf("resource %q stores a document, so its <field> declarations would never be read", r.Name)
		}
	default:
		return fmt.Errorf("resource %q: unknown store mode %q", r.Name, r.Store)
	}
	for _, f := range r.Fields {
		if f.Name == "" {
			return fmt.Errorf("resource %q: <field> needs a name", r.Name)
		}
		if slices.Contains(seen, f.Name) {
			return fmt.Errorf("resource %q: %q declared twice", r.Name, f.Name)
		}
		seen = append(seen, f.Name)
		switch f.Type {
		case FieldText, FieldInt, FieldBool, FieldTime, FieldJSON:
		default:
			return fmt.Errorf("resource %q field %q: unknown type %q", r.Name, f.Name, f.Type)
		}
		if (f.From == "") == (f.Expr == "") {
			return fmt.Errorf("resource %q field %q: give it a source path or an expr, not both and not neither", r.Name, f.Name)
		}
	}
	for _, k := range r.Keep {
		if k.Name == "" {
			return fmt.Errorf("resource %q: <keep> needs a name", r.Name)
		}
		if strings.TrimSpace(k.Reason) == "" {
			return fmt.Errorf("resource %q: <keep name=%q> needs a reason naming the consumer that needs the field -- an unexplained hole in the drop rule is how a URL key creeps back", r.Name, k.Name)
		}
	}
	if len(r.Keep) > 0 && len(r.Drop) == 0 {
		return fmt.Errorf("resource %q: <keep> rescues a key from a <drop> it does not have", r.Name)
	}
	if r.Reveal == nil {
		return fmt.Errorf("resource %q has no <reveal>: a stored fact with no rule about who may read it cannot be served", r.Name)
	}
	return r.Reveal.validate(r.Name)
}

func (rv *Reveal) validate(resource string) error {
	if rv.Public == "" && rv.Probe == nil {
		return fmt.Errorf("resource %q: <reveal> proves nothing -- declare a <public> predicate, a <probe>, or both", resource)
	}
	if rv.Probe != nil {
		if rv.Probe.Path == "" {
			return fmt.Errorf("resource %q: <probe> needs a path", resource)
		}
		if rv.GrantTTL <= 0 {
			return fmt.Errorf("resource %q: <probe> needs a <grant ttl=>: a proof that never expires is not a proof", resource)
		}
		if rv.DenyTTL <= 0 {
			return fmt.Errorf("resource %q: <probe> needs a <deny ttl=>", resource)
		}
	}
	return nil
}

func (rt *Route) validate(resources map[string]*Resource) error {
	if rt.Path == "" || !strings.HasPrefix(rt.Path, "/") {
		return fmt.Errorf("<route> needs an absolute path, got %q", rt.Path)
	}
	if rt.Method == "" {
		return fmt.Errorf("route %s needs a method", rt.Path)
	}
	if rt.Method != "GET" && rt.Method != "HEAD" {
		return fmt.Errorf("route %s %s: only reads are cached; a write belongs in passthrough", rt.Method, rt.Path)
	}
	res, ok := resources[rt.Resource]
	if !ok {
		return fmt.Errorf("route %s names resource %q, which is not declared", rt.Path, rt.Resource)
	}
	params := pathParams(rt.Path)
	supplied := make([]string, 0, len(params))
	for _, p := range params {
		name := p
		if mapped, ok := rt.Params[p]; ok {
			name = mapped
		}
		supplied = append(supplied, name)
	}
	supplied = append(supplied, rt.queryKeys()...)
	if rt.Complete && !rt.List {
		return fmt.Errorf("route %s: complete=\"true\" describes a list answer, and this route answers one row", rt.Path)
	}
	if !rt.List {
		for _, k := range res.Keys {
			if !slices.Contains(supplied, k.Name) {
				return fmt.Errorf("route %s cannot key resource %q: nothing supplies %q", rt.Path, res.Name, k.Name)
			}
		}
	}
	return rt.validateQuery()
}

// queryKeys names the resource columns this route's query supplies.
func (rt *Route) queryKeys() []string {
	out := make([]string, 0, len(rt.Query))
	for _, q := range rt.Query {
		if q.Key {
			out = append(out, rt.column(q.Name))
		}
	}
	return out
}

// column resolves an incoming parameter name to the resource column it fills.
// A <map> exists because a URL's spelling is the upstream's choice and a
// column's is the spec's, and neither should have to bend to the other.
func (rt *Route) column(param string) string {
	if mapped, ok := rt.Params[param]; ok {
		return mapped
	}
	return param
}

func (rt *Route) validateQuery() error {
	seen := make([]string, 0, len(rt.Query))
	for _, q := range rt.Query {
		if q.Name == "" {
			return fmt.Errorf("route %s: <param> needs a name", rt.Path)
		}
		if slices.Contains(seen, q.Name) {
			return fmt.Errorf("route %s: parameter %q declared twice", rt.Path, q.Name)
		}
		seen = append(seen, q.Name)
		switch q.Type {
		case FieldText, FieldInt, FieldBool:
		case "":
			return fmt.Errorf("route %s parameter %q: needs a type", rt.Path, q.Name)
		default:
			return fmt.Errorf("route %s parameter %q: a query value is text, int or bool, not %q", rt.Path, q.Name, q.Type)
		}
		if q.Type == FieldInt && q.Max < q.Min {
			return fmt.Errorf("route %s parameter %q: max %d is below min %d", rt.Path, q.Name, q.Max, q.Min)
		}
		// An unkeyed parameter with no default would let two different
		// questions share one row, which is the same answer served twice.
		if !q.Key && q.Default == "" {
			return fmt.Errorf("route %s parameter %q: give it a default or mark it key=\"true\" -- an unkeyed parameter with no default lets two different requests share one cached answer", rt.Path, q.Name)
		}
	}
	for _, a := range rt.Accept {
		if !strings.Contains(a, "/") {
			return fmt.Errorf("route %s: %q is not a media type", rt.Path, a)
		}
	}
	for _, code := range rt.Absorb {
		if code < 100 || code > 599 {
			return fmt.Errorf("route %s: %d is not an HTTP status", rt.Path, code)
		}
		if code >= 500 || code == 429 {
			return fmt.Errorf("route %s: refusing to absorb %d -- a transient failure stored is an outage remembered long after it ended", rt.Path, code)
		}
	}
	return nil
}

// pathParams returns the {name} placeholders of a route path, in order.
func pathParams(path string) []string {
	var out []string
	for _, seg := range strings.Split(path, "/") {
		if len(seg) > 2 && seg[0] == '{' && seg[len(seg)-1] == '}' {
			out = append(out, seg[1:len(seg)-1])
		}
	}
	return out
}

func (e *Events) validate(resources map[string]*Resource) error {
	if e.Path == "" {
		return fmt.Errorf("<events> needs a path")
	}
	if e.Secret == "" {
		return fmt.Errorf("<events> needs a secret: an unverified delivery is anyone's delivery")
	}
	if e.SignatureHeader == "" {
		return fmt.Errorf("<events> needs a signature header")
	}
	if e.TypeHeader == "" {
		return fmt.Errorf("<events> needs a type header")
	}
	seen := make([]string, 0, len(e.List))
	for _, ev := range e.List {
		if ev.Type == "" {
			return fmt.Errorf("<event> needs a type")
		}
		if slices.Contains(seen, ev.Type) {
			return fmt.Errorf("event %q declared twice", ev.Type)
		}
		seen = append(seen, ev.Type)
		res, ok := resources[ev.Resource]
		if !ok {
			return fmt.Errorf("event %q names resource %q, which is not declared", ev.Type, ev.Resource)
		}
		if ev.Subject == "" {
			return fmt.Errorf("event %q needs a <subject>: without one the engine cannot tell which deliveries order against each other", ev.Type)
		}
		if ev.Clock == "" && !ev.Unordered {
			return fmt.Errorf("event %q needs a <clock>, or must declare unordered=\"true\": a delivery whose moment is unknown can overwrite newer truth silently", ev.Type)
		}
		if ev.Clock != "" && ev.Unordered {
			return fmt.Errorf("event %q declares both a clock and unordered", ev.Type)
		}
		if len(ev.Sets) == 0 && ev.Invalidate == nil {
			return fmt.Errorf("event %q does nothing: give it <set> fields, or an <invalidate> with a reason", ev.Type)
		}
		if len(ev.Sets) > 0 && ev.Invalidate != nil {
			return fmt.Errorf("event %q both writes and invalidates; the write already replaced the row", ev.Type)
		}
		if ev.Invalidate != nil && strings.TrimSpace(ev.Invalidate.Reason) == "" {
			return fmt.Errorf("event %q: <invalidate> needs a reason stating why the payload cannot answer -- throwing away a value the upstream just handed us is the bug this asks you to justify", ev.Type)
		}
		if res.Store == StoreDocument && len(ev.Sets) > 0 {
			return fmt.Errorf("event %q writes fields into resource %q, which stores a document", ev.Type, res.Name)
		}
		known := make([]string, 0, len(res.Fields)+len(res.Keys))
		for _, f := range res.Fields {
			known = append(known, f.Name)
		}
		for _, k := range res.Keys {
			known = append(known, k.Name)
		}
		for _, st := range ev.Sets {
			if !slices.Contains(known, st.Field) {
				return fmt.Errorf("event %q sets %q, which resource %q does not declare", ev.Type, st.Field, res.Name)
			}
			if (st.From == "") == (st.Expr == "") {
				return fmt.Errorf("event %q set %q: give it a payload path or an expr, not both and not neither", ev.Type, st.Field)
			}
		}
	}
	return nil
}
