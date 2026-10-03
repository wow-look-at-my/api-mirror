# api-mirror design

## What this is

`api-cli` turns one XML file into a CLI for an API. `api-mirror` turns one XML file into a **caching mirror** of an API. An HTTP server that answers a consumer's requests from SQLite, keeps that SQLite current from the upstream API and from the upstream's webhooks. The HTTP server reveals each cached fact only to a caller who can have read it upstream.

The reference implementation of a hand-written mirror is `github-state-mirror`: about 34,000 lines of Go for ONE API. Every mechanism in it is generic except the vocabulary of the API it mirrors. This project moves the mechanism into an engine and leaves the vocabulary in a file.

## The one rule that shapes everything

A mirror has things to get right, and each has a failure mode that is silent:

1. **Store the fact once.** There is one true state upstream, so there is one row here. A cache keyed by "who asked" answers the same question multiple ways.
2. **Apply what you are told.** A webhook carries the new value. Throwing it away and re-fetching is a bug that looks like caution.
3. **Reveal by proof.** Storage is global. A read needs proof that this caller can read this resource upstream.

The engine enforces all three by construction. A spec cannot express a per-caller store, cannot express an invalidate-without-reason, and cannot serve a resource with no reveal rule.

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

A resource declares WHAT IS STORED. Its `<key>` children are the identity: the primary key of the derived table and the cache key of every route that reads it. There is no actor column and no way to declare one.

A key reads the document field of its own name. `from=` says otherwise, and `fold="true"` lower-cases the value so a differently cased URL cannot land on a row a delivery never reaches. The engine's own per-row column is `mirror_written_at`. The prefix keeps it clear of a field the upstream has. `updated_at` is exactly the name an API uses.

`<field>` declares a stored column with a name and a SQL-ish `type`. Its body is a path into the absorbed document (`owner.login`) or a template. `store="document"` on the resource stores the response body itself rather than columns, for a route whose answer is a blob nobody needs to query.

The rebuilt answer is a subset of the upstream document. A client of the upstream must read the mirror unchanged.

- Each field, and each key with `from=`, goes back to the path it was read from. The value and the JSON type stay the upstream's. `type="int"` on a key serves its stored text as a number.
- A key without `from=` is the route's word, not the upstream's, so it is never served.
- A folded key matches without regard to case. The engine also keeps the spelling the upstream sent, and serves that.
- An expr field is derived. As a result, it is stored and never served.
- A null cell is left out, because storage cannot tell a sent null from a field that was never sent.

One fact is declared once. Validation refuses declarations that read one source path, or one path under another, in a resource or in one event. `dats/upstream-shape.dats` holds every cached column route against the upstream's own answer.

### `<route>` — what a consumer asks for

A route binds an HTTP path pattern to a resource. Path parameters (`{owner}`) become the key. A cached route serves stored state. On a miss or a stale row it fetches upstream, absorbs, and REBUILDS the answer from what it stored. A route never replays bytes it did not parse.

`<param>` declares the query shape: a name, a type, a default, and a range. Some requests become a passthrough: a parameter the route does not declare, a repeated one, or a value outside its range. The mirror never keys an answer on a shape the spec did not describe. `<accept>` does the same for media types. `<absorb status="404"/>` names an upstream refusal worth remembering, and a status named nowhere relays without being stored.

`list="true"` marks a route whose answer is an array of the resource's rows. The answer is the page the upstream returned: the rows it named, in its order, and nothing another route stored under the same parent key. Each list answer records its members, keyed by the request together with every query value the route models, so page 2 is its own answer. A row a delivery deletes drops out of every page that named it. A document list stores the upstream's array whole and replays it.

`complete="true"` adds that the answer is the WHOLE set under its parent key. An item missing from it is deleted upstream, so its row goes too. It is declared rather than inferred, because only the author knows whether a page is the whole set. Replace-syncing one page of a paginated list will throw away the others. A list without it only ever adds rows, and an item that vanished upstream keeps its row until it is evicted some other way.

A route the spec does not declare is a passthrough: forwarded verbatim, uncached, and reported as uncached. There is no third state.

### `body-key` — when the question is in the body

A query language posts every question to one path. Nothing in the URL tells answers apart, so a path parameter cannot key the row. There is nothing to model as a query parameter either.

```xml
<resource name="graphql" store="document" ttl="1m">
	<key name="token_fp" credential="true"/>
	<key name="query"/>
	<reveal><credential/></reveal>
</resource>

<route method="POST" path="/graphql" resource="graphql" body-key="query"/>
```

`body-key` names the resource key the request body fills, as a digest. Naming it also makes the body TRAVEL: the fetch replays it to the upstream. This is the only way the upstream can answer the question at all. Declare the body's content type in `<upstream><forward>` so it arrives typed.

Such a resource is almost always credential-keyed, because a query language filters what it returns by the caller's own token. Callers asking the identical question are entitled to different answers, and must never share a row.

### `<rewrite>` — the client's spelling of the same path

A client can insist on a path shape the upstream's own docs do not use, and then every declared route misses. `gh` is the case that forces this. It reads any host that is not github.com as GitHub Enterprise Server. Pointed at a mirror with `GH_HOST`, it puts REST under `/api/v3` and GraphQL at `/api/graphql`. Nothing matches, and the whole session forwards uncached.

```xml
<rewrite from="/api/v3"/>
<rewrite from="/api/graphql" to="/graphql"/>
```

`from` is a prefix and `to` is what it becomes, with an empty `to` stripping it. The rewrite happens before anything else looks at the path, so one route table serves both spellings and the cache is shared between them.

It grants nothing. The rewritten path meets the same auth and reveal gates the bare path meets, so `/api/v3/repos/o/r` is exactly as guarded as `/repos/o/r`. A prefix matches at a segment boundary only, and rules never chain: a rewritten path is the answer, not an input to the next rule.

### `<relay>` — a path that is not on the upstream host

An API's login endpoints usually sit on a different host from its data, and send no CORS headers of their own. A browser app therefore cannot complete a sign-in against them at all. GitHub is the case that forces this: the OAuth code exchange and the device-flow start live on `github.com`, not on `api.github.com`.

```xml
<relay method="POST" path="/login/oauth/access_token" to="https://github.com/login/oauth/access_token"/>
```

A relay forwards the body verbatim to a fixed URL and answers with what comes back. Nothing is stored and nothing is keyed. As a result, it is a passthrough that happens to leave the upstream's host. It is reported as uncached like any other.

It carries no bearer. On these paths the BODY is the credential: an OAuth `client_secret` or a bare public `client_id`. The caller's `Authorization` header therefore never goes to a foreign host. The foreign host's own allow-origin headers are dropped as well. This is because `<cors>` is the single authority and a duplicate makes a browser refuse the answer.

`to` must be https, unless it names loopback, which is how a test points one at a local stand-in.

### `<events>` — the upstream telling us

An event declares how one webhook payload becomes stored rows:

- `<subject>` — what this delivery is a view OF (the ordering grain).
- `<clock>` — the payload field stating WHEN that view is from.
- `<key field="...">payload path</key>` — where THIS delivery carries a key column.
- `<apply>` — `<set field="...">payload path</set>`, one per column.
- `<invalidate reason="...">` — the escape hatch. The `reason` is required.

The engine sorts deliveries for one subject inside a short reorder window, refuses a view older than one already applied, and applies unconditionally. There is no "does anyone have this cached?" gate to write.

`<key>` exists because a resource's `from=` names a path in the upstream DOCUMENT. A delivery wraps that document in an envelope — the repository under `repository`, the action beside it. So the same path finds nothing at the payload root. A `<set>` that writes a key column answers this too, which is why most events need no `<key>` at all. `<invalidate>` events, which write nothing, always do. `validate` refuses an event that names neither. This is because at run time this is one failed delivery per delivery, forever, on a mirror that looks like it is working.

A write needs the whole key. An invalidate may name part of it and deletes every row that matches. A payload names a commit, not one page of a paginated answer, so a page-keyed document set can only be dropped wholesale.

Narrower forms:

- `when="{{ ... }}"` on `<event>` is a predicate over `.payload`. False skips that event for the delivery. A type may declare one resource twice only when every declaration has one. The two then cannot both decide the row.
- `<invalidate every="true">` drops the whole resource. It is for a per-credential answer whose key the payload cannot state.
- `<invalidate field="tip_sha" differs="pull_request.base.sha">` drops the one stored row only when its field disagrees with what the delivery states. A disagreement means a delivery was lost, so the next reader refetches.

`<events id-header=>` passes the provider's own delivery id to subscribers, and `<subscribed path= field=>` asks the provider which types it sends. As a result, the dashboard can name each declared type that never arrives.

### `<reveal>` — who may be told

A `<reveal>` sits on the resource, not the route: it governs the FACT, so every route that reads that fact is gated the same way.

```xml
<reveal>
	<public>{{ eq .row.visibility "public" }}</public>
	<probe method="GET" path="/repos/{{ .key.owner }}/{{ .key.name }}"/>
	<grant ttl="24h"/>
	<deny ttl="5m"/>
</reveal>
```

Public is a predicate over the stored row. A probe re-asks upstream with the CALLER's own credential. A 2xx earns a grant. An authoritative denial is cached briefly. A transient failure is never cached. Unknown fails closed.

A route's `<spread resource="repo" items="...">` earns grants in bulk. Each item of a listing the caller fetched is a grant for that caller, and, unless `absorb="false"`, a stored row of truth.

### Who is asking

A principal is the credential's fingerprint unless the spec names something more stable. The fingerprint is `token:` and the hex SHA-256 of the bare credential. A `Bearer` or `token` scheme word is not part of it, so one token is one caller. `<identity header= path= id= prefix=>` accepts an assertion in a request header, verifies it with one upstream call, and names the caller `prefix:id`. A refused assertion is a 401 and never falls back. `<whoami path= id= prefix=>` asks the upstream who owns the caller's own credential. Both keep a caller's grants across a rotated credential.

A static upstream header never answers for a caller. A request-time call drops every forwarded header the caller did not send, and never renders a `<credential>` header at all.

## The operations half

Everything above decides what an answer IS. What follows decides whether anybody can tell the answers are right, and what happens when nobody is looking. None of it changes a response body.

### `<dashboard>` — the operator surface

```xml
<dashboard path="/_mirror" title="GitHub mirror">
	<token><value name="env.MIRROR_ADMIN_TOKEN"/></token>
</dashboard>
```

The surface exists whether or not a spec declares one. This decides where it lives and what gates it. An absent `<token>` mints one per process and logs the URL that carries it — on by default, never open by default. Everything admin hangs off the prefix, so a deployment gates the whole thing in one rule.

The page reports traffic by lane, cache contents by kind, the rate budget per identity, deliveries by type, and principals with their standing. The tab that matters most shows **what is still leaving**. A passthrough is unfinished work. As a result, the page groups each leaking family by route shape and counts it. It gives each family real sample paths and the `<resource>` and `<route>` that will stop it.

### Everything the mirror exchanges is on the chart

This is an invariant, not a feature. A request nobody can see spends budget, can be slow, can fail, and none of that reaches the operator. Instrumenting call sites cannot hold the property — it covers the calls somebody remembered. The reverse proxy is exactly what gets forgotten. So the rule is about the CLIENT: it reports from its transport, or it is a hole.

### Background work

```xml
<refresh interval="6h"/>
<replay interval="5m" requires="env.GITHUB_TOKEN">
	<list>/app/hook/deliveries?status=failure</list>
	<redeliver>/app/hook/deliveries/<value name="delivery.id"/>/attempts</redeliver>
	<id>id</id>
	<at>delivered_at</at>
</replay>
```

`<refresh>` sweeps stored keys, which matters most for the key nobody is reading: with no reader there is no refetch. A missed delivery goes unnoticed for the whole TTL. The sweep holds no caller's credential, so a credential-keyed row is skipped rather than refetched under the wrong identity.

`<replay>` reads the upstream's own failure log and asks for what never arrived, once per delivery id. `requires=` names a value the log cannot be read without. Empty means the job declines to start and logs which value will start it, rather than calling an authenticated endpoint on a timer forever. Its `<header>` children replace the static headers on its own calls. The `jwt` template function signs the RS256 assertion an App-level endpoint needs.

A duration attribute and a `<cors><origin>` may be template source over `.env`, rendered at load, so an operator tunes them without editing the spec. `0` turns the debounce and the replayer off. `<mirror max-rows=>` caps each resource table, oldest writes first, default one million.

### `<credential>` — a token the mirror obtains itself

A request with no caller needs a credential of its own. A static header cannot hold one that expires within the hour, such as a GitHub App installation token. A `<credential>` declares how to obtain the token. An upstream header sends it.

```xml
<upstream base="...">
	<header name="Authorization"><value name="env.GITHUB_BACKGROUND_TOKEN"/></header>
	<header name="Authorization" credential="app">token <value name="credential"/></header>
	<forward name="Authorization"/>
</upstream>

<credential name="app" requires="env.GITHUB_APP_ID" for="{{ .key.owner }}" margin="5m">
	<header name="Authorization">Bearer <value expr='{{ jwt .env.GITHUB_APP_ID .env.GITHUB_APP_PRIVATE_KEY }}'/></header>
	<lookup method="GET" path="/users/{{ urlpath .for }}/installation" field="id" ttl="1h"/>
	<exchange method="POST" path="/app/installations/{{ .lookup }}/access_tokens" token="token" expires="expires_at"/>
</credential>
```

- `for=` is template source over `.key`, the key of the row the request is about. Its value picks the token, so each owner gets the token of its own installation. Without it, one token serves every request.
- `<lookup>` finds which grant covers that value. The exchange path reads its `field` as `.lookup`. The answer is kept for `ttl`, default one hour.
- `<exchange>` issues the token. `token` and `expires` are paths into its answer. `ttl` stands in for an answer that states no expiry. The engine keeps a token until `margin` before it expires, default a few minutes.
- The credential's own `<header>` children authenticate both calls.
- Empty `requires` turns the credential off. The log says so at startup. The header is then skipped, and an earlier header of the same name stands. That is how a spec states a fallback.

A failed lookup or exchange fails the request with an error that names the step and the status. It never falls back to anonymous. An authoritative refusal is kept for a minute. As a result, a missing installation costs one call a minute. A header with `credential=` is never rendered for a caller, whatever `<forward>` lists, so a caller's request never starts an exchange.

### `<notify>` — telling consumers, so they stop racing us

A consumer can also receive the upstream's webhooks. It then reads the mirror before the mirror has ingested the delivery. It gets the state that delivery was about to replace. `<notify>` lets them register an endpoint and sends an HMAC-signed notification AFTER the write lands. Registrations live in a separate database file, because the cache nuke is safe only for rows the upstream still has a copy of.

A caller manages its own registrations at `<notify path=>`, under its resolved principal. A delivery notifies once, after its last handler applies. It reaches a subscriber only when `<scope>` matches the subscriber's filter and the subscriber holds stored proof for the row `<reveal>` names. A filter entry matches its scope or any scope beneath it at a `/`, so `acme` admits `acme/widget`. `<field>` adds payload values to the body.

`<notify import=>` names a github-state-mirror subscription file, as template source over `.env`. At startup the engine adopts each row it has not adopted before. The row keeps its id, secret, filters, counters, timestamps (to the second) and owner. A ledger records each source id, so a registration deleted here stays deleted. The owner is translated into the principal the same caller resolves to here. A row that no caller here can own is not imported. It is logged by id and counted on the dashboard, and the next start tries it again. A named file that is missing or not that format stops startup. A value that renders empty imports nothing and logs that once. The format belongs to that service. As a result, the importer is its own file, `gsm_import.go`.

### `<login>` — operators sign in upstream

`<dashboard><login>` runs an OAuth code flow in front of the dashboard. The upstream names the operator, and only a login on the admin list gets an HMAC-signed session. The token still works beside it.

### `<cors>`, `<health>`, and debouncing

`<cors>` names the origins and the response headers a cross-origin script may read. The engine always adds its own answer headers to that list, under the prefix `<mirror header-prefix=>` names (default `X-Mirror-`). An author does not have to know them.

`<health>` registers the paths an orchestrator polls. An unrouted path falls through to the proxy rather than answering 404. A checker that reads any non-404 as "implemented" then calls a healthy container unhealthy forever.

`<upstream debounce=>` shares one upstream call between identical concurrent passthrough reads. It never shares one across credentials.

## What the engine owns

- Schema derivation and the fingerprint nuke.
- The freshness state machine and its error backoff.
- Detached fetches and the shutdown drain.
- Request coalescing, ordering, and HMAC verification.
- Grant and deny bookkeeping.
- The dashboard and its telemetry.
- The background jobs.
- The observability that makes every upstream request visible.

## What the spec owns

Names. Paths. Which field carries the clock. What "public" means for this API. Nothing else.
