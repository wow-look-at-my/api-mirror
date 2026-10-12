package mirror

import (
	"regexp"
	"time"
)

// Spec is mirror declaration: the whole contents of a mirror XML file.
type Spec struct {
	Name string
	// HeaderPrefix starts the headers the engine sets on an answer.
	HeaderPrefix string
	// MaxRows caps every resource table. Zero turns the cap off.
	MaxRows int
	Vars    []Var
	// Accept lists media types that name the default representation. Every route that answers JSON also answers these.
	Accept    []string
	Upstream  Upstream
	Resources []*Resource
	Routes    []*Route
	Purges    []*Purge
	// Rewrites canonicalise an inbound path before anything matches it.
	Rewrites []*Rewrite
	// Relays forward a path to a fixed URL that is not the upstream base.
	Relays []*Relay
	Events *Events
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
	// Aliases repeat response headers under the names older clients read.
	Aliases []HeaderAlias
	// Identity resolves a credential to a stable principal. Nil keys a caller
	// by their credential's fingerprint.
	Identity *Identity
	// Whoami resolves a caller's own credential to a stable principal.
	Whoami *Whoami
	// Credentials are background credentials obtained by an exchange.
	Credentials []*Credential
}

// Credential is a background credential that the engine obtains by an
// exchange and caches until shortly before it expires. Only a call with no
// caller uses it.
type Credential struct {
	Name string
	// Requires names a value without which the credential is off.
	Requires string
	// For is template source over `.key`. Its value picks the token, so each owner gets the token that covers it.
	For string
	// Margin is how long before its expiry a token is replaced.
	Margin time.Duration
	// Headers authenticate the lookup and the exchange.
	Headers []Header
	// Lookup finds the value the exchange path needs. Nil means none.
	Lookup *CredentialLookup
	// Exchange is the call whose answer carries the token.
	Exchange CredentialExchange
}

// CredentialLookup is the call that finds which grant covers a `.for`
// value, such as the installation of an owner.
type CredentialLookup struct {
	Method string
	// Path is template source over `.for`.
	Path string
	// Field is the path into the answer that the exchange reads as `.lookup`.
	Field string
	// TTL bounds how long an answer is reused.
	TTL time.Duration
}

// CredentialExchange is the call that issues the token.
type CredentialExchange struct {
	Method string
	// Path is template source over `.for` and `.lookup`.
	Path string
	// Token is the path into the answer that holds the token.
	Token string
	// Expires is the path into the answer that states when the token expires.
	Expires string
	// TTL is the lifetime of a token whose answer states no expiry.
	TTL time.Duration
}

// Whoami is the upstream call that names the owner of a credential. A
// credential that cannot call it keeps the fingerprint principal.
type Whoami struct {
	Method string
	Path   string
	// ID is the path into the answer that names the owner.
	ID string
	// Prefix starts the principal, which is "<prefix>:<id>".
	Prefix string
	// TTL bounds how long an answer is reused.
	TTL time.Duration
}

// Identity says how a caller's credential becomes a stable principal. A
// principal gates what the reveal layer shows; it never partitions storage,
// and a verified assertion keeps it stable across every credential the
// caller rotates through.
type Identity struct {
	// TTL bounds how long a resolved credential is trusted without asking again.
	TTL time.Duration
	// User resolves a bearer through the upstream's own "who am I" answer.
	User *IdentityRule
	// Assertion lets a caller name an identity the upstream verifies, so
	// credentials that rotate keep the grants their holder earned.
	Assertion *IdentityRule
	// Header is the request header that carries the assertion.
	Header string
	// Method and Path are the upstream call that verifies the assertion.
	Method string
	Path   string
	// Send is the header that carries the assertion to that call.
	Send string
	// Scheme prefixes the assertion in Send, as in "Bearer <assertion>".
	Scheme string
	// ID and Name are paths into the verification answer.
	ID   string
	Name string
	// Prefix starts the principal, which is "<prefix>:<id>".
	Prefix string
}

// Dashboard is the operator surface: the tabs, the admin JSON, and the token
// that gates them.
type Dashboard struct {
	// Path is the prefix everything admin hangs off, gateable in rule.
	Path string
	// Token is template source for the secret. Empty mints and logs the URL.
	Token string
	// Title is what the page calls this mirror.
	Title string
	// SignIn lets a human in through the upstream's OAuth. Nil leaves the
	// token as the only way in.
	SignIn *SignIn
	// Login lets an operator sign in through the upstream's OAuth flow.
	Login *Login
}

// Login is an OAuth code flow in front of the dashboard. The upstream names
// the operator, and the operator's login must be on Admins.
type Login struct {
	Authorize string
	Exchange  string
	// User is the upstream path that names the signed-in operator.
	User string
	// Name is the path into that answer that holds the login.
	Name          string
	ClientID      string
	ClientSecret  string
	SessionSecret string
	// Admins is a comma-separated list of logins, compared without case.
	Admins string
	// BaseURL is the public address the callback is built from. Empty derives it from the request.
	BaseURL string
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
// applied. A consumer stops racing the mirror's ingestion with its own copy of
// the upstream's webhooks.
type Notify struct {
	// Path is the subscription CRUD prefix.
	Path string
	// DB is a SEPARATE file: the cache nuke must not delete a registration.
	DB string
	// Import is template source naming a github-state-mirror registry to adopt at startup. Empty renders mean no import.
	Import string
	// SignatureHeader carries the HMAC digest of each notification body.
	SignatureHeader string
	// Timeout bounds delivery attempt.
	Timeout time.Duration
	// Retries is how many times a failed delivery is retried.
	Retries int
	// DisableAfter is the consecutive-failure count that parks a subscription.
	DisableAfter int
	// Scope renders a delivery to the value a subscription filter matches. It is template source over `.payload`.
	Scope string
	// ScopeField names the JSON field of a subscription that holds its filter.
	ScopeField string
	// EventHeader and DeliveryHeader name the headers of a notification.
	EventHeader    string
	DeliveryHeader string
	// Reveal names the row a subscriber must be able to see.
	Reveal *NotifyReveal
	// Fields add delivery values to the notification body.
	Fields []Set
}

// NotifyReveal gates a notification on the subscriber's access to one row.
type NotifyReveal struct {
	Resource string
	Keys     []Set
}

// Refresh is the periodic sweep that keeps declared kinds warm without a
// consumer having to ask.
type Refresh struct {
	Interval time.Duration
	// Kinds names the resources swept. Empty means every routed resource.
	Kinds []string
}

// Replay asks the upstream to re-send deliveries the mirror never received.
type Replay struct {
	Interval time.Duration
	// List is the upstream path returning failed deliveries.
	List string
	// Redeliver asks for to be re-sent; template source over `.delivery`.
	Redeliver string
	// Method is how a redelivery is asked for.
	Method string
	// ID is the path into a listed delivery that names it.
	ID string
	// At is the path into a listed delivery stating when it was attempted.
	At string
	// Lookback bounds how far back a cycle will ask.
	Lookback time.Duration
	// Max bounds how many redeliveries cycle asks for.
	Max int
	// Requires names a value without which every cycle can only fail.
	Requires string
	Off      bool
	// Headers replace the upstream's static headers on the replayer's calls.
	Headers []Header
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
	// Require names forwarded headers a caller must send.
	Require []string
	// Rate names the budget headers. Their spelling is the upstream's, not ours.
	Rate RateHeaders
	// Debounce holds an eligible passthrough READ so identical reads share call.
	Debounce time.Duration
	// RetryAfter names the header saying a refusal is about waiting, not access.
	RetryAfter string
	// App is the mirror's own signed identity for the calls it makes itself.
	App *App
}

// Header is header sent upstream.
type Header struct {
	Name  string
	Value string // template source
	// Background sends this header only on the mirror's own calls: refresh,
	// replay and check. A caller's request never carries it.
	Background bool
	// Credential names the <credential> whose token renders as `.credential`.
	Credential string
}

// StoreMode says what a resource keeps per key.
type StoreMode string

const (
	// StoreColumns absorbs the upstream document into declared columns. A read
	StoreColumns StoreMode = "columns"
	// StoreDocument keeps the response document itself, minus the declared
	StoreDocument StoreMode = "document"
	// StoreRaw keeps the response body byte for byte, for a media type that is
	// not JSON. Its route's earliest <accept> is the type asked for and served.
	StoreRaw StoreMode = "raw"
)

const (
	// StoreRedirect keeps the Location of a redirect, for as long as the signed URL it names stays valid.
	StoreRedirect StoreMode = "redirect"
	// StoreText keeps a representation that is not JSON, such as a diff, with its media type.
	StoreText StoreMode = "text"
)

// whole reports whether the resource keeps each answer in a single stored cell.
func (r *Resource) whole() bool {
	return r.Store == StoreDocument || r.Store == StoreRaw
}

// Redirect says how long a stored Location may be served.
type Redirect struct {
	// Margin is how long before a signed URL expires it stops being served.
	Margin time.Duration
	// Unsigned is the lifetime of a Location with no query string, which carries no signature to expire. Zero stores none.
	Unsigned time.Duration
	// Fallback is the lifetime of a Location whose expiry no <expires> reads. Zero stores none.
	Fallback time.Duration
	// Expires are the query parameters that state a signed URL's expiry, tried in order.
	Expires []ExpiryParam
}

// ExpiryParam reads a signed URL's expiry from one of its query parameters.
type ExpiryParam struct {
	Param string
	// Format is "time" (RFC 3339), "unix" (seconds since the epoch), or "seconds".
	Format string
	// Since names the parameter that holds the signing time, as 20060102T150405Z.
	Since string
}

// answersJSON reports whether a resource's answer is a JSON document.
func (m StoreMode) answersJSON() bool {
	return m == StoreColumns || m == StoreDocument
}

// Resource is kind of stored fact: derived SQLite table, row per key.
type Resource struct {
	Name   string
	Store  StoreMode
	TTL    time.Duration
	Keys   []Key
	Fields []Field
	// Drop lists key patterns removed from an absorbed document.
	Drop []string
	// Keep rescues exact key names from Drop. Every entry is a claim that some
	Keep []Keep
	// Reveal gates every read of this resource. A resource without cannot
	Reveal *Reveal
	// RevokeOn is the document path holding a credential this resource hands
	// out. A row whose credential the upstream refuses is dropped.
	RevokeOn string
	// Contradiction names another resource whose later rows can prove a
	// stored row stale.
	Contradiction *Contradiction
	// Redirect is the lifetime rule of a redirect resource.
	Redirect *Redirect
}

// Key is component of a resource's identity.
type Key struct {
	Name string
	// From is the path into an absorbed document that yields this key's value,
	From string
	// Fold lower-cases the value before it is stored or looked up. Declare it
	Fold bool
	// Credential fills this from the caller's own request, not the document.
	Credential bool
	// ByPrincipal fills a credential key with the caller's resolved principal
	// instead of the credential's fingerprint, so a rotated credential of the
	// same identity finds the same row.
	ByPrincipal bool
	// Type is the JSON type the answer serves a From key as: text or int.
	Type FieldType
}

// Served reports whether the answer carries this key.
func (k Key) Served() bool { return k.From != "" }

// sentColumn is the engine column that keeps a folded key's spelling as the
// upstream sent it, or "" for a key that needs none.
func (k Key) sentColumn() string {
	if !k.Fold || !k.Served() {
		return ""
	}
	return enginePrefix + "sent_" + k.Name
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

// Field is stored column.
type Field struct {
	Name string
	Type FieldType
	// From is a dotted path into the absorbed document ("owner.login"). Exactly
	From string
	// Expr is template source evaluated against the absorbed document.
	Expr string
	// Version marks the column the upstream advances on every change. A write
	// carrying an older value than the stored row is refused.
	Version bool
}

// Keep rescues document key from a resource's Drop patterns.
type Keep struct {
	Name   string
	Reason string
}

// QueryParam is query parameter a route models.
type QueryParam struct {
	Name    string
	Type    FieldType
	Default string
	// Min and Max bound an int parameter.
	Min, Max int
	// Key includes this parameter in the cache key. A parameter that changes
	Key bool
	// Empties is an answer path that a true value serves as an empty array.
	Empties string
}

// Route binds an HTTP path the consumer asks for to a resource that answers
// it. A path the spec does not declare is a passthrough: forwarded verbatim
// and reported as uncached.
type Route struct {
	Method   string
	Path     string // "/repos/{owner}/{repo}"
	Resource string
	TTL      time.Duration // means the resource's own TTL
	// Params maps a path parameter to the resource key it supplies. Empty means
	Params map[string]string
	List   bool
	// Complete says this answer is the WHOLE set under its parent key.
	Complete bool
	// Query is the modelled query shape. A parameter outside it, a repeated
	Query []QueryParam
	// Accept lists the media types this route may answer.
	Accept []string
	// Absorb lists the upstream statuses whose answer is stored.
	Absorb []int
	// BodyKey names the resource key the REQUEST body fills, as a digest, for a route whose question travels in its body.
	BodyKey string
	// Bypass is a regular expression over the request body. A body it matches
	// is a write, such as a GraphQL mutation, and is forwarded uncached.
	Bypass *regexp.Regexp
	// Assert says the caller's bearer is itself an identity assertion, verified
	// by the <identity><assertion> rule. A bearer that does not verify is
	// forwarded uncached.
	Assert bool
	// Spreads carry the items of a fetched answer into another resource.
	Spreads []*Spread
	// Match constrains the shape of a path parameter. A request that fails one is left for a later route on the same path.
	Match []ParamMatch
}

// ParamMatch requires a path parameter to match a regular expression.
type ParamMatch struct {
	Param   string
	Pattern *regexp.Regexp
}

// admits reports whether every constrained path parameter has its shape.
func (rt *Route) admits(params map[string]string) bool {
	for _, m := range rt.Match {
		if !m.Pattern.MatchString(params[m.Param]) {
			return false
		}
	}
	return true
}

// Reveal is the proof a caller must have before a stored fact is revealed to
// them.
type Reveal struct {
	// Public is a predicate over the stored row (`.row.<field>`). True means
	Public string
	// Probe re-asks the upstream with the CALLER's own forwarded credential.
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

// Rewrite canonicalises an inbound path prefix before anything matches it.
type Rewrite struct {
	// From is the prefix a caller sends, and To what it becomes. An empty To strips it.
	From string
	To   string
}

// Relay forwards a path to a fixed URL off the upstream base, uncached and
// with no bearer.
type Relay struct {
	Method string
	Path   string
	// To is the absolute URL the body is forwarded to, verbatim.
	To string
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
	// Subscribed asks the provider which types it sends, for the dashboard.
	Subscribed *Subscribed
	// IDHeader carries the provider's own delivery id, for a subscriber.
	IDHeader string
	// ReorderWindow holds a delivery so other deliveries for the same subject
	ReorderWindow time.Duration
	List          []*Event
	// Subscriptions asks the upstream which types it sends. Nil reports none.
	Subscriptions *EventSubscriptions
}

// Event maps delivery to stored rows.
type Event struct {
	Type     string
	Resource string
	// Subject is what this delivery is a view OF: the grain both the reorder
	Subject string
	// Clock is the payload path stating the moment this view is from.
	Clock string
	// Unordered is the explicit opt-out for a payload that states no moment.
	Unordered bool
	// AbsorbWhenSuperseded lets a delivery the watermark refused still write its
	AbsorbWhenSuperseded bool
	// Actions limits the event to deliveries whose action is any of these.
	// Empty handles every action.
	Actions []string
	// When is template source over `.payload`. The event handles a delivery
	// only when it renders truthy.
	When string
	// Keys say where the DELIVERY carries each key column.
	Keys []Set
	Sets []Set
	// Document is the payload path whose object becomes a document resource's stored answer.
	Document string
	// Invalidate is the last resort: the payload does not carry the new value
	Invalidate *Invalidate
}

// Set writes column from the delivery payload. A write touches only the
// columns its event names; an absent field never blanks.
type Set struct {
	Field string
	From  string // path into the payload
	Expr  string // or template source
	// AllowNull writes a null the payload states, instead of leaving
	AllowNull bool
}

// Invalidate drops the stored row for this event's subject.
type Invalidate struct {
	Reason string
	// All drops every row of the resource, for rows no delivery can key.
	All bool
	// Every drops every row of the resource.
	Every bool
	// Field and Differs make the drop conditional.
	Field   string
	Differs string
}
