package mirror

import "time"

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
	// Drop lists key patterns removed from an absorbed document. A URL is the
	// canonical case: it points back at the upstream, so serving one hands the
	// consumer a way around the mirror. A pattern is an exact name or a
	// "*suffix" form.
	//
	// It carries the weight in StoreDocument mode. In StoreColumns mode the
	// projection already drops everything undeclared, so a pattern there only
	// documents the intent.
	Drop []string
	// Keep rescues exact key names from Drop. Every entry is a claim that some
	// consumer needs that field, so the spec states the consumer in the keep's
	// reason attribute; an unexplained hole in the drop rule is how a URL key
	// creeps back.
	Keep []Keep
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
	// Fold lower-cases the value before it is stored or looked up. Declare it
	// wherever the upstream treats the component case-insensitively: without it
	// a differently-cased request URL lands on its own row, which a webhook
	// naming the canonical spelling then never reaches.
	Fold bool
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

// Keep rescues one document key from a resource's Drop patterns.
type Keep struct {
	Name   string
	Reason string
}

// QueryParam is one query parameter a route models. A request carrying a
// parameter the route does not declare is passed through rather than answered
// from a row keyed on a shape the spec never described.
type QueryParam struct {
	Name    string
	Type    FieldType
	Default string
	// Min and Max bound an int parameter. A value outside the range is a
	// passthrough, not a clamp: a clamped page number answers a question the
	// caller did not ask.
	Min, Max int
	// Key includes this parameter in the cache key. A parameter that changes
	// the answer must be keyed, or two different answers share one row.
	Key bool
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
	// Complete says this answer is the WHOLE set under its parent key, so an
	// item missing from it has been deleted upstream and its row goes with it.
	//
	// Without it a list only ever adds, and an item that vanished upstream is
	// served for good. It is declared rather than inferred because only the
	// spec author knows whether a page is the whole set: replace-syncing one
	// page of a paginated list would throw away every other page.
	Complete bool
	// Query is the modelled query shape. A parameter outside it, a repeated
	// parameter, or a value outside a declared range makes the request a
	// passthrough with a stated reason.
	Query []QueryParam
	// Accept lists the media types this route may answer. A caller asking for
	// something else gets a passthrough, because the mirror rebuilds JSON and
	// cannot rebuild a diff or a patch.
	Accept []string
	// Absorb lists the upstream statuses whose answer is stored. A 2xx is
	// always stored; naming a 4xx here declares it an authoritative verdict
	// worth caching. A status named nowhere relays unstored, every time,
	// because a transient failure cached is an outage remembered.
	Absorb []int
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
	// AbsorbWhenSuperseded lets a delivery the watermark refused still write its
	// fields. Declare it only where every field is an IMMUTABLE fact a newer
	// view never restates -- a commit's own contents, not a snapshot of state.
	// For anything else this reinstates the stale write the watermark stopped.
	AbsorbWhenSuperseded bool
	Sets                 []Set
	// Invalidate is the last resort: the payload does not carry the new value
	// and cannot derive it. Reason is required, so an invalidation always says
	// why the payload could not answer.
	Invalidate *Invalidate
}

// Set writes one column from the delivery payload.
//
// A write touches only the columns its event names, so a payload that does not
// carry a field can never blank it. That is the engine's answer to the whole
// class of bugs where a partial view overwrites known state with nothing.
type Set struct {
	Field string
	From  string // path into the payload
	Expr  string // or template source
	// AllowNull writes a null the payload actually states, instead of leaving
	// the column alone. Declare it where absent and empty differ -- a cleared
	// description, a disarmed setting -- and nowhere else, because everywhere
	// else it turns a missing field into a blanked one.
	AllowNull bool
}

// Invalidate drops the stored row for this event's subject.
type Invalidate struct {
	Reason string
}
