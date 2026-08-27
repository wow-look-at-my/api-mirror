# api-mirror — repo orientation for Claude

One XML spec in, one caching mirror of an API out. The engine holds every mechanism; the spec holds only the vocabulary of the API being mirrored. `README.md` is the human front page, `docs/design.md` is the grammar and the engine/spec split.

The sibling project `api-cli` turns the same kind of file into a CLI. The spec language itself lives in `wow-look-at-my/api-dsl` and is shared by both — never fork it locally.

## File map

The engine is `internal/mirror`, one package; `cmd/api-mirror` is the entry point and holds nothing else. Paths below are inside `internal/mirror` unless stated.

- `run.go` — `Run`: flags, `--check`, the server, the signal, the drain order.
- `config.go` — the Spec model: Resource, Key, Field, Route, QueryParam, Reveal, Events, Event, Set. Types only.
- `validate.go` — what a spec must mean before it is served. Every check here exists because its absence is a silent runtime failure.
- `load.go` / `load_events.go` — XML to Spec. Builders check SHAPE; `validate` checks MEANING.
- `dsl.go` — the boundary with api-dsl. Below it is the language, above it is what a mirror means.
- `schema.go` — the derived DDL and the schema fingerprint.
- `store.go` / `store_resources.go` — engine tables via sqlc, resource tables built at run time from `columnsOf`.
- `internal/database/` — `schema.sql`, `queries/*.sql`, and sqlc's generated `dbgen/`.
- `freshness.go` — the TTL state machine, detached fetches, per-key singleflight, the shutdown drain.
- `router.go` — route matching, the Accept and query guards, the passthrough reason vocabulary.
- `engine.go` / `fetch.go` — serving a declared route: reveal, ensure fresh, read, rebuild. And the fetch that absorbs.
- `absorb.go` — document to columns and back, the drop patterns, the one JSON marshaller.
- `reveal.go` — the authorization ladder: public, grant, cached denial, probe.
- `ordering.go` / `events.go` — webhook ingest: subject and clock, the reorder window, the watermark, the apply.
- `upstream.go` — the one outbound client, reporting every request.
- `load_ops.go` / the ops half of `validate.go` — `<dashboard>`, `<cors>`, `<notify>`, `<refresh>`, `<replay>`, `<health>`, `<ratelimit>`.
- `observe.go` — `Lane`, `Exchange`, `Observer`, and the transport every outbound client is built through.
- `telemetry.go` / `timeline.go` / `requestlog.go` / `ratemeter.go` — the four in-memory stores behind the page. Bounded, lazily swept, reset on restart.
- `recorder.go` — the inbound wrapper that records what the caller actually received.
- `shapes.go` — a path to a route shape, and the `<route>` sketch that would stop it leaking.
- `admin.go` / `admin_api.go` / `admin_subs.go` / `web.go` + `web/` — the operator surface and its embedded page.
- `refresh.go` / `replay.go` / `notify.go` / `subscriptions.go` / `debounce.go` — the background half.
- `mirror.schema.xsd` — the reference grammar. Not enforced at load; a test walks the shipped specs against it.
- `mirror.example.xml` — a runnable small spec. `samples/github/github.xml` — the full worked example. Both at the top of the tree, reached from a test through `repoRoot`.

## Invariants

- **Storage is global; authorization is at the READ.** One row per fact, no actor column, and no way for a spec to declare one. Grants and denials are the only per-principal tables.
- **Apply the payload. Invalidation is the last resort.** A delivery carries the new value; the engine writes it. `<invalidate>` needs a stated reason, and `validate.go` rejects one without.
- **A write touches only the fields its event names.** A payload that does not carry a field cannot blank it. `null="allow"` is the explicit opt-in where absent and empty differ.
- **Order before you write.** Every event declares a subject and a clock, or declares itself unordered out loud. A watermark refuses a view older than one applied; equal times apply.
- **A list only deletes when it says it is complete.** `complete="true"` replace-syncs the set under the parent key; without it an item that vanished upstream keeps being served, because one page of a paginated list is not the set.
- **Only authoritative answers are stored.** A 2xx always, a 4xx the route names. A 5xx or a rate-limit refusal relays and is forgotten.
- **Fail closed.** Unknown visibility is private, a store or render error denies, and a resource with no `<reveal>` cannot be served.
- **Hit and miss share one path.** An answer is rebuilt from stored state either way, so a route's shape never changes with cache state.
- **A passthrough is unfinished work.** It is forwarded with a stated reason from a closed vocabulary. There is no "correctly uncached".
- **The fingerprint is derived, never declared.** It hashes the schema the spec produces, so a resource change always nukes and nothing has to be kept in step by hand.
- **A fetch outlives its request.** Detached context plus a safety timeout, drained before the database closes.
- **Every request this service sends is on the chart.** The reporting is in the TRANSPORT (`observedClient`), never at a call site: a call site only covers what somebody remembered to instrument, and the passthrough proxy is exactly what gets forgotten. A new outbound client is built through `observedClient` or it is a hole.
- **The operator surface exists whether or not a spec declares one.** A spec chooses where it lives and what gates it. With no `<token>` the engine mints one per process and logs the URL carrying it: on by default, never open by default.
- **A background job with a missing precondition does not run.** `<replay requires=>` names a value the failure log cannot be read without; empty means the job declines to start and logs which value would start it. A cycle that can only fail, forever, on a timer is a job that looks busy and recovers nothing.
- **Telemetry is memory-only and bounded.** A live view, not an audit log: a table would put sub-day-ephemeral rows behind a cache-nuking schema. Every store sweeps lazily and reports what it dropped rather than truncating quietly.
- **A browser fetches the page's subresources itself.** No header, no query string — which is why the dashboard's own assets are gated by a strict same-site cookie the shell sets. Test an operator surface the way a browser drives it, not the way the page does.

## Commands

- `go-toolchain` — tidy, vet, test with coverage, build, then `dats/*.dats`. Never a bare `go` command, and never pipe or redirect its output.
- `sqlc generate` — regenerates `internal/database/dbgen` after editing `schema.sql` or `queries/*.sql`.
- `api-mirror --spec <file> --check` — print the derived schema, routes and events, then exit.
- `dats/api-cli-smoke.dats` — drives a real `api-mirror` with `api-cli`'s own upstream GitHub sample against `cmd/fakegithub`, a deterministic stand-in. Proves a genuine independent client gets correctly shaped data on a miss and a byte-identical, zero-upstream-call hit.

## Conventions

- Tabs for indentation, in Go and in XML. Shipped `*.xml` declare `version="1.1"`; the loader strips the declaration, so inline test snippets omit it.
- Comments state the invariant and why, in short sentences and active voice. No changelogs, no dates, no "this used to".
- Tests use testify, `require` to stop and `assert` to check. Package-level vars (`upstreamClient`, `logger`) are swapped with save-and-restore in `t.Cleanup`.
- go-toolchain warns at 500 lines and errors at 750. Extract into a topical file rather than growing one.
