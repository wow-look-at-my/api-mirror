package mirror

import (
	"fmt"
	"github.com/wow-look-at-my/go-containers/set"
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
	for _, p := range s.Purges {
		if err := p.validate(byName); err != nil {
			return err
		}
	}
	if s.Events != nil {
		if err := s.Events.validate(byName); err != nil {
			return err
		}
	}
	return s.validateOps(byName)
}

// validateOps checks the operational half.
//
// Each rule here is an operator would otherwise discover from behaviour: a
// window that adds latency to everything, a sweep that names a kind no route
// serves, a replayer that lists failures and cannot ask for any of them back.
func (s *Spec) validateOps(byName map[string]*Resource) error {
	if s.Upstream.Debounce > maxDebounceWindow {
		// A fat-fingered "5m" must fail here, not wedge the API for an hour.
		return fmt.Errorf("<upstream> debounce %s is longer than the %s cap: every uncacheable read waits it out",
			s.Upstream.Debounce, maxDebounceWindow)
	}
	if s.Dashboard.Path == "" {
		// Here, not at load: a Spec built in code is read off disk.
		s.Dashboard.Path = defaultDashboardPath
	}
	if !strings.HasPrefix(s.Dashboard.Path, "/") {
		return fmt.Errorf("<dashboard> path %q must start with /", s.Dashboard.Path)
	}
	if s.Refresh != nil {
		if s.Refresh.Interval <= 0 {
			return fmt.Errorf("<refresh> needs a positive interval")
		}
		kinds := set.New[string]()
		for _, rt := range s.Routes {
			kinds.Add(routeKind(rt))
		}
		for _, k := range s.Refresh.Kinds {
			if !kinds.Contains(k) {
				return fmt.Errorf("<refresh><kind>%s</kind> names nothing any route serves", k)
			}
		}
	}
	if s.Replay != nil {
		if s.Replay.List == "" || s.Replay.Redeliver == "" {
			return fmt.Errorf("<replay> needs both <list> and <redeliver>: listing failures it cannot ask back is not recovery")
		}
		if s.Replay.ID == "" {
			return fmt.Errorf("<replay> needs <id>: without it a delivery cannot be named, so every cycle would ask for all of them again")
		}
		if s.Replay.Interval <= 0 {
			return fmt.Errorf("<replay> needs a positive interval")
		}
	}
	if s.Notify != nil {
		if s.Events == nil {
			return fmt.Errorf("<notify> has nothing to announce: this spec declares no <events>")
		}
	}
	if s.Health != nil && s.Health.Live == "" && s.Health.PreUpdate == "" {
		return fmt.Errorf("<health> names no path, so it registers nothing and every check falls through to the upstream")
	}
	if s.CORS != nil && len(s.CORS.Origins) == 0 {
		return fmt.Errorf("<cors> names no <origin>, so it would allow nothing while looking like a policy")
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
	seen := set.New[string]()
	hasCredentialKey := false
	for _, k := range r.Keys {
		if k.Name == "" {
			return fmt.Errorf("resource %q: <key> needs a name", r.Name)
		}
		if seen.Contains(k.Name) {
			return fmt.Errorf("resource %q: %q declared twice", r.Name, k.Name)
		}
		seen.Add(k.Name)
		if k.Credential {
			if k.From != "" {
				return fmt.Errorf("resource %q: key %q is credential=\"true\" and also declares from=%q; pick one", r.Name, k.Name, k.From)
			}
			hasCredentialKey = true
		}
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
		if seen.Contains(f.Name) {
			return fmt.Errorf("resource %q: %q declared twice", r.Name, f.Name)
		}
		seen.Add(f.Name)
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
	if r.Reveal.Credential && !hasCredentialKey {
		return fmt.Errorf("resource %q: <reveal><credential/></reveal> needs a <key credential=\"true\">, or a different caller could read another's row", r.Name)
	}
	return r.Reveal.validate(r.Name)
}

func (rv *Reveal) validate(resource string) error {
	if rv.Credential {
		if rv.Public != "" || rv.Probe != nil {
			return fmt.Errorf("resource %q: <credential/> already gates every read; a <public> or <probe> alongside it is dead code", resource)
		}
		return nil
	}
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
	res, ok := resources[rt.Resource]
	if !ok {
		return fmt.Errorf("route %s names resource %q, which is not declared", rt.Path, rt.Resource)
	}
	if rt.Method != "GET" && rt.Method != "HEAD" {
		// A credential-gated mint replays a still-valid answer, so it is
		// the write worth caching. Everything else is passthrough or <purge>.
		if rt.Method != "POST" || !res.Reveal.Credential {
			return fmt.Errorf("route %s %s: only reads and credential-gated mints are cached; a write belongs in passthrough or <purge>", rt.Method, rt.Path)
		}
	}
	params := pathParams(rt.Path)
	supplied := set.New[string]()
	for _, p := range params {
		name := p
		if mapped, ok := rt.Params[p]; ok {
			name = mapped
		}
		supplied.Add(name)
	}
	supplied.AddRange(rt.queryKeys()...)
	if rt.Complete && !rt.List {
		return fmt.Errorf("route %s: complete=\"true\" describes a list answer, and this route answers one row", rt.Path)
	}
	if !rt.List {
		for _, k := range res.Keys {
			if k.Credential {
				// The engine fills this from the request, not a route param.
				continue
			}
			if !supplied.Contains(k.Name) {
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
func (rt *Route) column(param string) string {
	if mapped, ok := rt.Params[param]; ok {
		return mapped
	}
	return param
}

func (rt *Route) validateQuery() error {
	seen := set.New[string]()
	for _, q := range rt.Query {
		if q.Name == "" {
			return fmt.Errorf("route %s: <param> needs a name", rt.Path)
		}
		if seen.Contains(q.Name) {
			return fmt.Errorf("route %s: parameter %q declared twice", rt.Path, q.Name)
		}
		seen.Add(q.Name)
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
		// An unkeyed parameter with no default would let different
		// questions share row, which is the same answer served.
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

// validate checks a <purge>: a write with somewhere real to land and a key
// the path can actually supply.
func (p *Purge) validate(resources map[string]*Resource) error {
	if p.Path == "" || !strings.HasPrefix(p.Path, "/") {
		return fmt.Errorf("<purge> needs an absolute path, got %q", p.Path)
	}
	switch p.Method {
	case "POST", "PUT", "PATCH", "DELETE":
	default:
		return fmt.Errorf("purge %s: method must be POST, PUT, PATCH or DELETE, not %q", p.Path, p.Method)
	}
	res, ok := resources[p.Resource]
	if !ok {
		return fmt.Errorf("purge %s names resource %q, which is not declared", p.Path, p.Resource)
	}
	// The path may name a PREFIX of the key, and the delete reaches everything
	// beneath it. A gap is refused: a key named after a missing one would
	// widen the delete past what the path says.
	params := pathParams(p.Path)
	named, missing := 0, ""
	for _, k := range res.Keys {
		if k.Credential {
			continue
		}
		if !slices.Contains(params, k.Name) {
			if missing == "" {
				missing = k.Name
			}
			continue
		}
		if missing != "" {
			return fmt.Errorf("purge %s cannot key resource %q: %q is supplied but %q before it is not", p.Path, res.Name, k.Name, missing)
		}
		named++
	}
	if named == 0 && !anyCredentialKey(res) {
		return fmt.Errorf("purge %s cannot key resource %q: the path supplies no key, so this would delete every row", p.Path, res.Name)
	}
	return nil
}

// anyCredentialKey reports whether the caller's credential keys res, which
// makes it addressable with no path parameter.
func anyCredentialKey(res *Resource) bool {
	for _, k := range res.Keys {
		if k.Credential {
			return true
		}
	}
	return false
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
	seen := set.New[string]()
	for _, ev := range e.List {
		if ev.Type == "" {
			return fmt.Errorf("<event> needs a type")
		}
		if seen.Contains(ev.Type) {
			return fmt.Errorf("event %q declared twice", ev.Type)
		}
		seen.Add(ev.Type)
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
		known := set.New[string]()
		for _, f := range res.Fields {
			known.Add(f.Name)
		}
		for _, k := range res.Keys {
			known.Add(k.Name)
		}
		for _, st := range ev.Sets {
			if !known.Contains(st.Field) {
				return fmt.Errorf("event %q sets %q, which resource %q does not declare", ev.Type, st.Field, res.Name)
			}
			if (st.From == "") == (st.Expr == "") {
				return fmt.Errorf("event %q set %q: give it a payload path or an expr, not both and not neither", ev.Type, st.Field)
			}
		}
		if err := validateEventKeys(ev, res); err != nil {
			return err
		}
	}
	return nil
}

// validateEventKeys refuses an event that cannot address the row it is about.
//
// A resource's from= names a path in the UPSTREAM document, and a delivery
// wraps that document in an envelope, so the same path finds nothing at the
// payload root. The event has to say where the delivery carries each key
// column. Left to run time it is error per delivery, forever, on a mirror
// whose dashboard reports every of them accepted.
func validateEventKeys(ev *Event, res *Resource) error {
	for _, k := range ev.Keys {
		if !slices.ContainsFunc(res.Keys, func(rk Key) bool { return rk.Name == k.Field }) {
			return fmt.Errorf("event %q: <key field=%q> is not a key of resource %q", ev.Type, k.Field, res.Name)
		}
		if (k.From == "") == (k.Expr == "") {
			return fmt.Errorf("event %q key %q: give it a payload path or an expr, not both and not neither", ev.Type, k.Field)
		}
	}
	named := func(name string) bool {
		match := func(s Set) bool { return s.Field == name }
		return slices.ContainsFunc(ev.Keys, match) || slices.ContainsFunc(ev.Sets, match)
	}
	if ev.Invalidate != nil {
		// A partial key is legitimate here and deletes everything beneath it,
		// but naming none of them would delete the resource.
		if len(ev.Keys) == 0 {
			return fmt.Errorf("event %q invalidates every row of resource %q: add <key field=...> naming what this delivery is about",
				ev.Type, res.Name)
		}
		return nil
	}
	for _, k := range res.Keys {
		if !named(k.Name) {
			return fmt.Errorf("event %q cannot address key %q of resource %q: add <key field=%q>path</key> naming where the delivery carries it",
				ev.Type, k.Name, res.Name, k.Name)
		}
	}
	return nil
}
