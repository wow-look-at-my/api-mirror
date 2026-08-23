package main

import (
	"fmt"
	"strings"
	"time"
)

// Spec is one mirror declaration: the whole contents of a mirror XML file.
//
// The engine reads a Spec and derives everything else from it -- the SQLite
// schema, the HTTP routes, the webhook handlers, the reveal rules. Nothing in
// this file knows about any particular upstream API.
type Spec struct {
	Name      string
	Vars      []Var
	Upstream  Upstream
	Resources []*Resource
	Routes    []*Route
	Events    *Events
}

// Var is a named value available to every template in the spec as `.var.<name>`.
type Var struct {
	Name  string
	Value string // template source
}

// Upstream is the API being mirrored.
type Upstream struct {
	Base    string // template source, e.g. "https://api.github.com"
	Headers []Header
	// Forward names the request headers copied from the caller to the upstream.
	// The caller's own credential rides these, which is what makes a probe
	// prove THAT caller's access rather than the mirror's.
	Forward []string
}

// Header is one header sent upstream.
type Header struct {
	Name  string
	Value string // template source
}

// StoreMode says what a resource keeps per key.
type StoreMode string

const (
	// StoreColumns absorbs the upstream document into declared columns. A read
	// rebuilds the answer from those columns, so a field nobody declared is a
	// field nobody serves.
	StoreColumns StoreMode = "columns"
	// StoreDocument keeps the response document itself, minus the declared
	// drops. For an answer that is a blob to every consumer.
	StoreDocument StoreMode = "document"
)

// Resource is one kind of stored fact: one derived SQLite table, one row per
// key. There is deliberately no actor column and no way to declare one -- one
// true state upstream is one row here, and who may SEE that row is the reveal
// layer's question, never the storage layer's.
type Resource struct {
	Name   string
	Store  StoreMode
	TTL    time.Duration
	Keys   []Key
	Fields []Field
	// Drop lists document keys removed before storing, in StoreDocument mode.
	// A URL is the canonical example: it re-points at the upstream, so serving
	// one hands the consumer a way around the mirror.
	Drop []string
	// Reveal gates every read of this resource. A resource without one cannot
	// be served; see validate.
	Reveal *Reveal
}

// Key is one component of a resource's identity.
type Key struct {
	Name string
	// From is the path into an absorbed document that yields this key's value,
	// used when a write arrives without the route's path parameters (a webhook
	// payload). Empty means the key only ever arrives from a route or an event
	// subject.
	From string
}

// FieldType is a stored column's type. The set is deliberately small: a mirror
// stores what an API returns, and an API returns JSON.
type FieldType string

const (
	FieldText FieldType = "text"
	FieldInt  FieldType = "int"
	FieldBool FieldType = "bool"
	FieldTime FieldType = "time"
	FieldJSON FieldType = "json"
)

// Field is one stored column.
type Field struct {
	Name string
	Type FieldType
	// From is a dotted path into the absorbed document ("owner.login"). Exactly
	// one of From or Expr is set.
	From string
	// Expr is template source evaluated against the absorbed document.
	Expr string
}

// Route binds an HTTP path the consumer asks for to a resource that answers it.
// A path the spec does not declare is a passthrough: forwarded verbatim and
// reported as uncached. There is no third state and no "correctly uncached"
// verdict.
type Route struct {
	Method   string
	Path     string // "/repos/{owner}/{repo}"
	Resource string
	TTL      time.Duration // zero means the resource's own TTL
	// Params maps a path parameter to the resource key it supplies. Empty means
	// the names match.
	Params map[string]string
	// List marks a route whose answer is an ARRAY of the resource's rows rather
	// than one row. The parent keys select the rows.
	List bool
}

// Reveal is the proof a caller must have before a stored fact is revealed to
// them. Every branch is deliberate: a resource with no reveal rule is a config
// error, not an open resource.
type Reveal struct {
	// Public is a predicate over the stored row (`.row.<field>`). True means
	// anyone may read it, with no upstream call.
	Public string
	// Probe re-asks the upstream with the CALLER's own forwarded credential. A
	// 2xx earns a grant; an authoritative denial is cached for DenyTTL; a
	// transient failure is never cached.
	Probe *Probe
	// GrantTTL is how long a proven access stays proven.
	GrantTTL time.Duration
	// DenyTTL is how long an authoritative denial is replayed without asking.
	DenyTTL time.Duration
}

// Probe is the upstream request that proves a caller's access.
type Probe struct {
	Method string
	Path   string // template source over the resource's keys
}

// Events is the webhook ingest declaration.
type Events struct {
	// Path is where the upstream posts deliveries.
	Path string
	// Secret is template source yielding the HMAC key.
	Secret string
	// SignatureHeader carries the hex digest, GitHub's scheme.
	SignatureHeader string
	// TypeHeader carries the event type.
	TypeHeader string
	// ReorderWindow holds a delivery so other deliveries for the same subject
	// can be sorted with it and applied oldest-first.
	ReorderWindow time.Duration
	List          []*Event
}

// Event maps one delivery to stored rows.
type Event struct {
	Type     string
	Resource string
	// Subject is what this delivery is a view OF: the grain both the reorder
	// window and the watermark key on. Two deliveries about different subjects
	// never order against each other.
	Subject string
	// Clock is the payload path stating the moment this view is from. An
	// upstream orders nothing it sends, so this is what lets a late delivery be
	// recognised as late instead of overwriting newer truth.
	Clock string
	// Unordered is the explicit opt-out for a payload that states no moment.
	// Arrival order is then all there is, and the spec has to say so out loud.
	Unordered bool
	Sets      []Set
	// Invalidate is the last resort: the payload does not carry the new value
	// and cannot derive it. Reason is required, so an invalidation always says
	// why the payload could not answer.
	Invalidate *Invalidate
}

// Set writes one column from the delivery payload.
type Set struct {
	Field string
	From  string // path into the payload
	Expr  string // or template source
}

// Invalidate drops the stored row for this event's subject.
type Invalidate struct {
	Reason string
}

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
	seen := make(map[string]bool, len(r.Keys)+len(r.Fields))
	for _, k := range r.Keys {
		if k.Name == "" {
			return fmt.Errorf("resource %q: <key> needs a name", r.Name)
		}
		if seen[k.Name] {
			return fmt.Errorf("resource %q: %q declared twice", r.Name, k.Name)
		}
		seen[k.Name] = true
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
		if seen[f.Name] {
			return fmt.Errorf("resource %q: %q declared twice", r.Name, f.Name)
		}
		seen[f.Name] = true
		switch f.Type {
		case FieldText, FieldInt, FieldBool, FieldTime, FieldJSON:
		default:
			return fmt.Errorf("resource %q field %q: unknown type %q", r.Name, f.Name, f.Type)
		}
		if (f.From == "") == (f.Expr == "") {
			return fmt.Errorf("resource %q field %q: give it a source path or an expr, not both and not neither", r.Name, f.Name)
		}
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
	supplied := make(map[string]bool, len(params))
	for _, p := range params {
		name := p
		if mapped, ok := rt.Params[p]; ok {
			name = mapped
		}
		supplied[name] = true
	}
	if !rt.List {
		for _, k := range res.Keys {
			if !supplied[k.Name] {
				return fmt.Errorf("route %s cannot key resource %q: nothing supplies %q", rt.Path, res.Name, k.Name)
			}
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
	seen := make(map[string]bool, len(e.List))
	for _, ev := range e.List {
		if ev.Type == "" {
			return fmt.Errorf("<event> needs a type")
		}
		if seen[ev.Type] {
			return fmt.Errorf("event %q declared twice", ev.Type)
		}
		seen[ev.Type] = true
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
		known := make(map[string]bool, len(res.Fields)+len(res.Keys))
		for _, f := range res.Fields {
			known[f.Name] = true
		}
		for _, k := range res.Keys {
			known[k.Name] = true
		}
		for _, st := range ev.Sets {
			if !known[st.Field] {
				return fmt.Errorf("event %q sets %q, which resource %q does not declare", ev.Type, st.Field, res.Name)
			}
			if (st.From == "") == (st.Expr == "") {
				return fmt.Errorf("event %q set %q: give it a payload path or an expr, not both and not neither", ev.Type, st.Field)
			}
		}
	}
	return nil
}
