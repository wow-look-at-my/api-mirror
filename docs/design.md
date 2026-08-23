# api-mirror design

## What this is

`api-cli` turns one XML file into a CLI for an API. `api-mirror` turns one XML
file into a **caching mirror** of an API: an HTTP server that answers a
consumer's requests from SQLite, keeps that SQLite current from the upstream API
and from the upstream's webhooks, and reveals each cached fact only to a caller
who could have read it upstream.

The reference implementation of a hand-written mirror is
`github-state-mirror`: about 34,000 lines of Go for ONE API. Every mechanism in
it is generic except the vocabulary of the API it mirrors. This project moves
the mechanism into an engine and leaves the vocabulary in a file.

## The one rule that shapes everything

A mirror has exactly three things to get right, and each one has a failure mode
that is silent:

1. **Store the fact once.** There is one true state upstream, so there is one
   row here. A cache keyed by "who asked" answers the same question two ways.
2. **Apply what you are told.** A webhook carries the new value. Throwing it
   away and re-fetching is a bug that looks like caution.
3. **Reveal by proof.** Storage is global; a read needs proof that this caller
   could read this resource upstream.

The engine enforces all three by construction. A spec cannot express a
per-caller store, cannot express an invalidate-without-reason, and cannot serve
a resource with no reveal rule.

## The grammar

```xml
<mirror name="github">
	<vars>
		<var name="upstream">https://api.github.com</var>
	</vars>

	<upstream base="{{ .var.upstream }}"/>

	<resource name="repo" ttl="6h">
		<key name="owner" from="owner.login" fold="true"/>
		<key name="name" from="name" fold="true"/>
		<field name="visibility" type="text">visibility</field>
		<field name="default_branch" type="text">default_branch</field>
		<reveal>...</reveal>
	</resource>

	<route method="GET" path="/repos/{owner}/{name}" resource="repo">
		<accept>application/json</accept>
		<absorb status="404"/>
	</route>

	<events>
		<event type="push" resource="branch">...</event>
	</events>
</mirror>
```

### `<resource>` — one table, one row per fact

A resource declares WHAT IS STORED. Its `<key>` children are the identity: the
primary key of the derived table and the cache key of every route that reads it.
There is no actor column and no way to declare one.

`<field>` declares a stored column: a name, a SQL-ish `type`, and a body that is
a path into the absorbed document (`owner.login`) or a template. `store="document"`
on the resource stores the response body itself rather than columns, for a route
whose answer is a blob nobody needs to query.

### `<route>` — what a consumer asks for

A route binds an HTTP path pattern to a resource. Path parameters (`{owner}`)
become the key. A cached route serves stored state; on a miss or a stale row it
fetches upstream, absorbs, and REBUILDS the answer from what it stored — a route
never replays bytes it did not parse.

`<param>` declares the query shape: a name, a type, a default, and a range. A
parameter the route does not declare, a repeated one, or a value outside its
range makes the request a passthrough rather than an answer keyed on a shape the
spec never described. `<accept>` does the same for media types. `<absorb
status="404"/>` names an upstream refusal worth remembering, and a status named
nowhere relays without being stored.

`list="true"` marks a route whose answer is an array of the resource's rows.
`complete="true"` adds that the answer is the WHOLE set under its parent key, so
an item missing from it has been deleted and its row goes too. It is declared
rather than inferred, because only the author knows whether a page is the whole
set — replace-syncing one page of a paginated list would throw away the others.
A list without it only ever adds, and an item that vanished upstream is served
until its row is evicted some other way.

A route the spec does not declare is a passthrough: forwarded verbatim,
uncached, and reported as uncached. There is no third state.

### `<events>` — the upstream telling us

An event declares how one webhook payload becomes stored rows:

- `<subject>` — what this delivery is a view OF (the ordering grain).
- `<clock>` — the payload field stating WHEN that view is from.
- `<apply>` — `<set field="...">payload path</set>`, one per column.
- `<invalidate reason="...">` — the escape hatch, and the `reason` is required.

The engine sorts deliveries for one subject inside a short reorder window,
refuses a view older than one already applied, and applies unconditionally —
there is no "does anyone have this cached?" gate to write.

### `<reveal>` — who may be told

A `<reveal>` sits on the resource, not the route: it governs the FACT, so every
route that reads that fact is gated the same way.

```xml
<reveal>
	<public>{{ eq .row.visibility "public" }}</public>
	<probe method="GET" path="/repos/{{ .key.owner }}/{{ .key.name }}"/>
	<grant ttl="24h"/>
	<deny ttl="5m"/>
</reveal>
```

Public is a predicate over the stored row. A probe re-asks upstream with the
CALLER's own credential; a 2xx earns a grant, an authoritative denial is cached
briefly, and a transient failure is never cached. Unknown fails closed.

## What the engine owns

Schema derivation and the fingerprint nuke, the freshness state machine and its
error backoff, detached fetches and the shutdown drain, request coalescing,
ordering, HMAC verification, grant and deny bookkeeping, and the observability
that makes every upstream request visible.

## What the spec owns

Names. Paths. Which field carries the clock. What "public" means for this API.
Nothing else.
