# api-mirror — repo orientation for Claude

One XML spec in, one caching mirror of an API out. The engine holds every mechanism. The spec holds only the vocabulary of the API being mirrored. `README.md` is the human front page, `docs/design.md` is the grammar and the engine/spec split.

The sibling project `api-cli` turns the same kind of file into a CLI. Both shares the spec language itself lives in `wow-look-at-my/api-dsl` and. Never fork it locally.

## File map

The engine is `internal/mirror`, one package. `cmd/api-mirror` is the entry point and holds nothing else. Paths below are inside `internal/mirror` unless stated.

- `run.go` — `Run`: flags, `--check`, the server, the signal, the drain order.
- `lifecycle.go` — the construction and shutdown that `run.go` and `engine/` both call.
- `config.go` — the Spec model: Resource, Key, Field, Route, QueryParam, Show, Events, Event, Set. Types only.
- `validate.go` — what a spec must mean before it is served. Every check here exists because its absence is a silent runtime failure.
- `load.go` / `load_events.go` — XML to Spec. Builders check SHAPE. `validate` checks MEANING.
- `dsl.go` — the boundary with api-dsl. Below it is the language, above it is what a mirror means.
- `schema.go` — the derived DDL and the schema fingerprint.
- `store.go` / `store_resources.go` — engine tables via sqlc, resource tables built at run time from `columnsOf`. `mirror_list_member` (a list answer's rows, in order) is plain SQL in `schema.go`, because a push may not add a generated file.
- `internal/database/` — `schema.sql`, `queries/*.sql`, and sqlc's generated `dbgen/`.
- `freshness.go` — the TTL state machine, detached fetches, per-key singleflight, the shutdown drain.
- `router.go` — route matching, the Accept and query guards, the passthrough reason vocabulary. `<accept>` under `<mirror>` names the default representation for every JSON route.
- `unstored.go` — an upstream answer the route does not keep: relayed to the caller whose credential earned it, a 403 or 404 recorded as their denial.
- `invalidate.go` — an invalidation drops the freshness under its key too. A stored refusal goes with the rows.
- `store_refusal.go` — the refusal document kept with a stored refusal, and prefix deletes, in plain SQL.
- `redirect.go` — `store="redirect"` (a Location kept until its signed URL expires) and `store="text"` (a non-JSON body with its media type).
- `events_document.go` — an event's `<document>`: the payload object written whole as a document resource's answer.
- `events_patch.go` — an event's `<patch>`: one item rewritten in every stored page of a document list. A failure drops the pages instead.
- `project.go` — `<param empties=>`: a bool flag that serves one answer path as an empty array, off one stored answer.
- `validate_paths.go` — `{name...}` rest parameters and `<match>` shape constraints on path parameters.
- `<rewrite>` — a client's own spelling of a path, canonicalised in `dispatch` before any gate sees it. `gh` needs one, because it puts REST under `/api/v3` on a non-github.com host. It grants nothing.
- `relay.go` — `<relay>`: an uncached forward to a fixed URL off the upstream base, for login endpoints on another host that send no CORS headers. It forwards no bearer, because the body is the credential there.
- `body-key` on a `<route>` — the resource key the REQUEST body fills, for a query language that posts every question to one path. Naming it also makes the body travel: the fetch replays it upstream. Almost always credential-keyed, because such an API filters by the caller's token.
- `engine.go` / `fetch.go` — serving a declared route: show, ensure fresh, read, rebuild. And the fetch that absorbs.
- `absorb.go` — document to columns and back, the drop patterns, the JSON marshaller.
- `reveal.go` — the authorization ladder: public, grant, cached denial, probe. `AllowStored` is the probe-free half, for a decision with no caller credential.
- `identity.go` — who asks: `<identity>` (a verified assertion header, as `prefix:id`), `<whoami>` (a credential's owner), else the credential fingerprint. A refused assertion is a 401, not a fallback.
- `spread.go` — `<spread>` on a route: each item of a fetched listing is a grant for its caller, and optionally truth.
- `notify_api.go` — the public subscription API at `<notify path=>`, under the caller's own principal, and `<notify>` validation.
- `gsm_import.go` — `<notify import=>`: adopts a github-state-mirror subscription file once per row, translating each owner to the principal it resolves to here.
- `jwt.go` — the `jwt` and `readFile` template functions, for a credential that background work signs itself.
- `exchange.go` / `load_exchange.go` — `<credential>`: a token obtained by lookup and exchange, and cached per `for=` value until its margin. Only an upstream header with `credential=` sends it, on a call with no caller.
- `headers.go` — the answer-header prefix. `stale.go` — an answer served after a failed refresh.
- `compress.go` — admin gzip. `rowcap.go` — the per-table row cap.
- `subscribed.go` — the report of declared types the provider does not send. `admin_login.go` — the dashboard's OAuth sign-in.
- `ordering.go` / `events.go` — webhook ingest: subject and clock, the reorder window, the watermark, the apply.
- `upstream.go` — the outbound client, reporting every request.
- `telemetry.go` / `timeline.go` / `requestlog.go` / `ratemeter.go` — the four in-memory stores behind the page. Bounded, lazily swept, reset on start.
- `recorder.go` — the inbound wrapper that records what the caller actually received.
- `shapes.go` — a path to a route shape, and the `<route>` sketch that will stop it leaking.
- `admin.go` / `admin_api.go` / `admin_subs.go` / `admin_check.go` / `web.go` + `web/` — the operator surface and its embedded page. The page loads scratch_ui from the org's master deploy, dark only.
- `check.go` — the consistency check: re-ask the upstream about every stored key of one kind, and optionally write back what drifted.
- `refresh.go` / `replay.go` / `notify.go` / `subscriptions.go` / `debounce.go` — the background half.
- `mirror.schema.xsd` — the reference grammar. Not enforced at load. A test walks the shipped specs against it.
- `mirror.example.xml` — a runnable small spec. `samples/github/github.xml` — the full worked example. Both at the top of the tree, reached from a test through `repoRoot`.

## Invariants

- **A delivery type reaches every resource it moves.** A push states a branch tip. It also makes the repo's file-derived answers incorrect. The type is therefore declared per resource. The engine runs every handler of it. Each handler carries its own subject, so they order apart. On a shared subject the watermark refuses all but the earliest. The answer is the worst of their outcomes. A failed handler is thus never hidden behind a sibling that applied. The same type against the SAME resource stays refused, because the later declaration silently decides the row.

- **A background credential never answers for a caller.** A request-time upstream call drops every forwarded header the caller did not send, so the static header serves only work with no caller. A `credential=` header is not rendered for a caller at all. A failed exchange fails the call rather than going out anonymous.
- **A delivery notifies once, after its last handler applies**, and only a subscriber with stored proof for the row `<notify><reveal>` names. A failed handler anywhere means no notification.
- **Storage is global. Authorization is at the READ.** One row per fact, no actor column, and no way for a spec to declare one. Grants and denials are the only per-principal tables.
- **Apply the payload. Invalidation is the last resort.** A delivery carries the new value. The engine writes it. `<invalidate>` needs a stated reason, and `validate.go` rejects one without.
- **A write touches only the fields its event names.** A payload that does not carry a field cannot blank it. `null="allow"` is the explicit opt-in where absent and empty differ.
- **Order before you write.** Every event declares a subject and a clock, or declares itself unordered out loud. A watermark refuses a view older than one applied. Equal times apply.
- **An event says where the DELIVERY carries its key.** A resource's `from=` names a path in the upstream document, and a delivery wraps that document in an envelope. As a result, the two are not the same path. `<key field=>` states it, a `<set>` that writes a key column counts, and `validate` refuses an event that names neither. A write needs the whole key. An invalidate may name a prefix and deletes everything beneath it, because a payload names a commit rather than one page of a paginated answer.
- **Its outcome counts a held delivery.** With a reorder window the response goes out before the apply. As a result, the tally comes from the applier, not from that answer. Counting the answer reports every failure as accepted.
- **A list only deletes when it says it is complete.** `complete="true"` replace-syncs the set under the parent key. Without it an item that vanished upstream keeps being served, because one page of a paginated list is not the set.
- **Only authoritative answers are stored.** A 2xx always, a 4xx the route names. A 5xx or a rate-limit refusal relays and is forgotten.
- **An answer the route does not store belongs to the caller.** It is relayed to them and earns no backoff that another caller reads.
- **A fresh answer with no row is refetched.** A delivery dropped it, and serving it as absent will invent a 404.
- **A push may not change a generated file.** A new engine table is plain SQL in Go, beside `mirror_list_member`.
- **Fail closed.** Unknown visibility is private, a store or render error denies, and a resource with no `<reveal>` cannot be served.
- **Hit and miss share one path.** An answer is rebuilt from stored state either way, so a route's shape never changes with cache state.
- **A rebuilt answer is a subset of the upstream's.** Each field, and each key with `from=`, returns to its source path with its upstream type. A folded key serves its as-sent spelling. One source path is declared once. `dats/upstream-shape.dats` checks the answers against the upstream with `api-mirror-delta`.
- **A passthrough is unfinished work.** It is forwarded with a stated reason from a closed vocabulary. There is no "correctly uncached".
- **The fingerprint is derived, not declared.** It hashes the schema the spec produces. A resource change therefore always nukes, and hand keeps nothing.
- **A fetch outlives its request.** Detached context plus a safety timeout, drained before the database closes.
- **Every request this service sends is on the chart.** The reporting is in the TRANSPORT (`observedClient`), not at a call site: a call site only covers what somebody remembered to instrument. The passthrough proxy is exactly what gets forgotten. A new outbound client is built through `observedClient` or it is a hole.
- **The operator surface exists whether or not a spec declares one.** A spec chooses where it lives and what gates it. With no `<token>` the engine mints one per process and logs the URL carrying it: on by default, not open by default.
- **A background job with a missing precondition does not run.** `<replay requires=>` names a value the failure log cannot be read without. Empty means the job declines to start and logs which value will start it. A cycle that can only fail, forever, on a timer is a job that looks busy and recovers nothing.
- **Telemetry is memory-only and bounded.** A live view, not an audit log: a table will put sub-day-ephemeral rows behind a cache-nuking schema. Every store sweeps lazily and reports what it dropped rather than truncating quietly.
- **A browser fetches the page's subresources itself.** No header, no query string — which is why the dashboard's own assets are gated by a strict same-site cookie the shell sets. Test an operator surface the way a browser drives it, not the way the page does.

## Commands

- `go-toolchain` — tidy, vet, test with coverage, build, then `dats/*.dats`. Never a bare `go` command, and never pipe or redirect its output.
- `sqlc generate` — regenerates `internal/database/dbgen` after editing `schema.sql` or `queries/*.sql`.
- `api-mirror --spec <file> --check` — print the derived schema, routes and events, then exit.
- `dats/api-cli-smoke.dats` — drives a real `api-mirror` with `api-cli`'s own upstream GitHub sample against `cmd/fakegithub`, a deterministic stand-in. Proves a genuine independent client gets correctly shaped data on a miss and a byte-identical, zero-upstream-call hit.

## Conventions

- Tabs for indentation, in Go and in XML. Shipped `*.xml` declare `version="1.1"`. The loader strips the declaration, so inline test snippets omit it.
- Comments state the invariant and why, in short sentences and active voice. No changelogs, no dates, no history.
- Tests use testify, `require` to stop and `assert` to check. Package-level vars (`upstreamClient`, `logger`) are swapped with save-and-restore in `t.Cleanup`.
- go-toolchain warns, then errors, on a long file. Extract into a topical file rather than growing one.
