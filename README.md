# api-mirror

Declare an API in one XML file; get a caching mirror of it.

`api-mirror` reads a spec and serves an HTTP endpoint that answers a consumer's requests from SQLite, keeps that SQLite current from the upstream and from the upstream's webhooks, and reveals each cached fact only to a caller who could have read it upstream.

It is the sibling of [api-cli](https://github.com/wow-look-at-my/api-cli), which turns the same kind of file into a command-line tool. Both share the spec language, in [api-dsl](https://github.com/wow-look-at-my/api-dsl).

## Why

A hand-written mirror of one API runs to tens of thousands of lines, and almost none of it is about that API. It is about freshness, ordering, authorization, and the dozen ways a cache goes quietly wrong. That code is the same for every API. Only the vocabulary changes.

## Run it

```sh
api-mirror --spec mirror.xml --db mirror.db --listen :8080
api-mirror --spec mirror.xml --check     # print the derived schema and routes, then exit
```

## The spec, in one screen

```xml
<mirror name="github">
	<upstream base="https://api.github.com">
		<forward name="Authorization"/>
	</upstream>

	<resource name="repo" ttl="6h">
		<key name="owner" from="owner.login" fold="true"/>
		<key name="name" from="name" fold="true"/>
		<field name="visibility" type="text">visibility</field>
		<field name="default_branch" type="text">default_branch</field>
		<reveal>
			<public>{{ eq .row.visibility "public" }}</public>
			<probe path="/repos/{{ .key.owner }}/{{ .key.name }}"/>
			<grant ttl="24h"/>
			<deny ttl="5m"/>
		</reveal>
	</resource>

	<route method="GET" path="/repos/{owner}/{name}" resource="repo"/>

	<events path="/webhook" signature-header="X-Hub-Signature-256" type-header="X-GitHub-Event">
		<secret><value name="env.WEBHOOK_SECRET"/></secret>
		<event type="push" resource="branch" clock="repository.pushed_at">
			<subject>ref:<value name="payload.repository.full_name"/>:<value name="payload.ref"/></subject>
			<apply><set field="tip_sha">after</set></apply>
		</event>
	</events>
</mirror>
```

`samples/github/github.xml` is the full worked example. `mirror.example.xml` is a smaller one you can run against a public test API.

## What you get for declaring it

- **A derived schema.** One table per resource, fingerprinted. Editing a resource rebuilds the cache on the next start, with no migration and no version constant to forget.
- **A cache that stores state, not bytes.** An answer is rebuilt from stored columns, so its shape is yours and cannot drift with whatever the upstream added this week.
- **Webhooks that apply.** A delivery carries the new value, so the engine writes it. Discarding it and refetching is not the default here; it is a last resort that has to state its reason.
- **Ordering.** Deliveries are sorted per subject inside a short window, and a view older than one already applied is refused. A redelivered payload cannot reopen something that closed.
- **Authorization at the read.** Storage is global — one row per fact. Whether a caller may see it is proven per request, against the upstream, with that caller's own credential.
- **Honest passthrough.** A path the spec does not declare is forwarded and labelled with why. There is no "correctly uncached".

## Docs

- `docs/design.md` — the grammar and the split between engine and spec.
- `CLAUDE.md` — the file map and the invariants.

## Build

`go-toolchain` runs tidy, vet, tests and the build. `sqlc generate` regenerates the engine's typed queries after editing `internal/database/schema.sql` or `internal/database/queries/*.sql`.
