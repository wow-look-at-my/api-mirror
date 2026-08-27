package mirror

import "time"

// Spec is one mirror declaration: the whole contents of a mirror XML file.
type Spec struct {
	Name      string
	Vars      []Var
	Upstream  Upstream
	Resources []*Resource
	Routes    []*Route
	Purges    []*Purge
	Events    *Events
	// Dashboard is the operator surface. Always present: a default fills in.
	Dashboard Dashboard
	// CORS is the browser policy. Nil answers no preflight and sets no headers.
	CORS *CORS
	// Notify declares who the mirror tells after a delivery lands.
	Notify *Notify
	// Refresh is the periodic background sweep over declared kinds.
	Refresh *Refresh
	// Replay asks the upstream to re-send deliveries that never arrived.
	Replay *Replay
	// Health declares the liveness and pre-update paths an orchestrator polls.
	Health *Health
}

// Dashboard is the operator surface: the tabs, the admin JSON, and the token
// that gates them.
type Dashboard struct {
	// Path is the one prefix everything admin hangs off, gateable in one rule.
	Path string
	// Token is template source for the secret. Empty mints one and logs the URL.
	Token string
	// Title is what the page calls this mirror.
	Title string
}

// CORS is the browser-facing policy.
type CORS struct {
	// Origins is the allow-list; "*" is safe because reveal gates, not origin.
	Origins []string
	// Expose lets a script read X-Mirror-*; without it the browser hides them.
	Expose []string
	// MaxAge is how long a preflight answer may be reused.
	MaxAge time.Duration
}

// Notify declares the mirror telling its own subscribers after a delivery is
// applied, so a consumer stops racing the mirror's ingestion with its own copy
// of the upstream's webhooks.
type Notify struct {
	// Path is the subscription CRUD prefix.
	Path string
	// DB is a SEPARATE file: the cache nuke must not delete a registration.
	DB string
	// SignatureHeader carries the HMAC digest of each notification body.
	SignatureHeader string
	// Timeout bounds one delivery attempt.
	Timeout time.Duration
	// Retries is how many times a failed delivery is retried.
	Retries int
	// DisableAfter is the consecutive-failure count that parks a subscription.
	DisableAfter int
}

// Refresh is the periodic sweep that keeps declared kinds warm without a
// consumer having to ask first.
type Refresh struct {
	Interval time.Duration
	// Kinds names the resources swept. Empty means every routed resource.
	Kinds []string
}

// Replay asks the upstream to re-send deliveries the mirror never received.
//
// A lost delivery is the quietest failure a mirror has: every cache the
// delivery would have moved serves its last absorbed answer for the whole TTL,
// and nothing reports a gap. A shorter TTL hides that window; it does not close
// it.
type Replay struct {
	Interval time.Duration
	// List is the upstream path returning failed deliveries.
	List string
	// Redeliver asks for one to be re-sent; template source over `.delivery`.
	Redeliver string
	// Method is how a redelivery is asked for.
	Method string
	// ID is the path into a listed delivery that names it.
	ID string
	// At is the path into a listed delivery stating when it was attempted.
	At string
	// Lookback bounds how far back a cycle will ask.
	Lookback time.Duration
	// Max bounds how many redeliveries one cycle asks for.
	Max int
	// Requires names a value without which every cycle can only fail.
	Requires string
}

// Health declares the paths an orchestrator polls.
type Health struct {
	Live string
	// PreUpdate holds while a detached fetch is in flight.
	PreUpdate string
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
	Forward []string
	// Rate names the budget headers. Their spelling is the upstream's, not ours.
	Rate RateHeaders
	// Debounce holds an eligible passthrough READ so identical reads share one call.
	Debounce time.Duration
	// RetryAfter names the header saying a refusal is about waiting, not access.
	RetryAfter string
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
	StoreColumns StoreMode = "columns"
	// StoreDocument keeps the response document itself, minus the declared
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
	Drop []string
	// Keep rescues exact key names from Drop. Every entry is a claim that some
	Keep []Keep
	// Reveal gates every read of this resource. A resource without one cannot
	Reveal *Reveal
}

// Key is one component of a resource's identity.
type Key struct {
	Name string
	// From is the path into an absorbed document that yields this key's value,
	From string
	// Fold lower-cases the value before it is stored or looked up. Declare it
	Fold bool
	// Credential fills this from the caller's own request, not the document.
	Credential bool
}

// FieldType is a stored column's type. The set is deliberately small: a mirror
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
	Min, Max int
	// Key includes this parameter in the cache key. A parameter that changes
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
	Params map[string]string
	// List marks a route whose answer is an ARRAY of the resource's rows rather
	List bool
	// Complete says this answer is the WHOLE set under its parent key, so an
	Complete bool
	// Query is the modelled query shape. A parameter outside it, a repeated
	Query []QueryParam
	// Accept lists the media types this route may answer. A caller asking for
	Accept []string
	// Absorb lists the upstream statuses whose answer is stored. A 2xx is
	Absorb []int
}

// Reveal is the proof a caller must have before a stored fact is revealed to
// them. Every branch is deliberate: a resource with no reveal rule is a config
// error, not an open resource.
type Reveal struct {
	// Public is a predicate over the stored row (`.row.<field>`). True means
	Public string
	// Probe re-asks the upstream with the CALLER's own forwarded credential. A
	Probe *Probe
	// GrantTTL is how long a proven access stays proven.
	GrantTTL time.Duration
	// DenyTTL is how long an authoritative denial is replayed without asking.
	DenyTTL time.Duration
	// Credential says the resource's own key already proves access.
	Credential bool
}

// Probe is the upstream request that proves a caller's access.
type Probe struct {
	Method string
	Path   string // template source over the resource's keys
}

// Purge forwards a write and, on a 2xx, deletes the cached row it changed.
type Purge struct {
	Method   string
	Path     string
	Resource string
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
	ReorderWindow time.Duration
	List          []*Event
}

// Event maps one delivery to stored rows.
type Event struct {
	Type     string
	Resource string
	// Subject is what this delivery is a view OF: the grain both the reorder
	Subject string
	// Clock is the payload path stating the moment this view is from. An
	Clock string
	// Unordered is the explicit opt-out for a payload that states no moment.
	Unordered bool
	// AbsorbWhenSuperseded lets a delivery the watermark refused still write its
	AbsorbWhenSuperseded bool
	// Keys say where the DELIVERY carries each key column.
	Keys []Set
	Sets []Set
	// Invalidate is the last resort: the payload does not carry the new value
	Invalidate *Invalidate
}

// Set writes one column from the delivery payload. A write touches only the
// columns its event names; an absent field never blanks one.
type Set struct {
	Field string
	From  string // path into the payload
	Expr  string // or template source
	// AllowNull writes a null the payload actually states, instead of leaving
	AllowNull bool
}

// Invalidate drops the stored row for this event's subject.
type Invalidate struct {
	Reason string
}
