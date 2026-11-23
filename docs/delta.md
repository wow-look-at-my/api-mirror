# api-mirror-delta reference

The config is YAML. An unknown key is an error. As a result, a misspelt rule fails at load rather than ignoring nothing. Every problem in a config is reported at once.

| Key | Default | Meaning |
| --- | --- | --- |
| `listen` | `127.0.0.1:8090` | Address `serve` listens on. |
| `truth.url` | required | Base URL of the real API. A base path is kept: `https://ghe.example/api/v3`. |
| `truth.headers` | none | Headers set on every request to the truth, replacing the client's. `${VAR}` reads the environment; an unset variable is an error. |
| `mirrors[]` | required | Names are unique and never `truth`. |
| `serve` | `truth` | The target whose answer the client receives. |
| `compare[]` | `GET`, `HEAD` | `method` and `route` (a regex on the path). A request no entry matches goes to the served target alone and counts as `uncompared`. |
| `annotate` | `X-Mirror-Cache`, `X-Mirror-Passthrough-Reason` | Mirror response headers copied into each record, so a difference can be read against hit or miss. |
| `report` | none | NDJSON file; one record per request per mirror, appended. |
| `recent` | `500` | Records kept in memory for `/_delta/recent`. |
| `timeout` | `60s` | Per-target request timeout. |
| `ignore[]` | none | Rules, below. |

## A fanned-out write runs once per target

api-mirror forwards a write to its upstream. A `POST` sent to the truth and to a mirror of the same API therefore reaches that API twice. Add a non-`GET` entry to `compare` only for a request that reads, such as a GraphQL query.

## Kinds of difference

| Kind | Path | Meaning |
| --- | --- | --- |
| `status` | | The status codes differ. |
| `header` | the header name | A header in `headers` differs. |
| `missing` | JSONPath | The truth has the node; the mirror does not. |
| `extra` | JSONPath | The mirror has the node; the truth does not. |
| `changed` | JSONPath | Both have a scalar there, with different values. Numbers compare by value. |
| `type` | JSONPath | Both have the node, with different JSON types. |
| `body` | `byte N` | A body that is not JSON on both sides differs; N is the first differing byte. |
| `error` | | A target did not answer at all. |

Before comparison, each target's own base URL is replaced with `{base}` in its headers and body strings. A mirror that echoes the truth's URL is still a difference: a client that follows it bypasses the mirror.

## Ignore rules

```yaml
ignore:
  - path: $..*_url          # a JSONPath; selects missing/extra/changed/type
    kinds: [missing]        # optional narrowing
    method: GET             # optional
    route: ^/repos/         # optional regex on the request path
    mirror: local           # optional
    reason: why this is expected
  - header: Link            # a header difference
  - kinds: [status]         # a kind alone
    route: ^/user$
```

A rule must name a path, a header or a kind. A rule that names none will hide everything. The first rule that covers a difference claims it. The summary lists every rule with the number of differences it hid. It marks a rule at zero, because a rule that never matches is either stale or incorrect.

### JSONPath subset

A pattern covers the node it selects and that node's whole subtree.

| Syntax | Selects |
| --- | --- |
| `$` | the root, so every JSON difference |
| `.name`, `['name']` | a member |
| `[3]` | an array element |
| `.*`, `[*]` | any member or element |
| `..name` | `name` at any depth below |
| `.*_url` | a member whose name matches the glob; `*` is any run of characters |

A filter, a slice or a union is an error at load, not a silent non-match. A `*` inside a quoted name is a literal star.

## The report surfaces

- The log: one WARN line per difference that no rule hid, one ERROR line per target that did not answer. `-v` also logs every match.
- `/_delta/summary` (JSON) and `/_delta/summary?format=text`: counts, every ignore rule, and one row per method, path, mirror, kind and path shape. A shape writes every index as `[*]`, so one difference in every element of a list is one row. The summary keeps a bounded number of rows. Past that it counts and logs what it can not keep.
- `/_delta/recent?limit=N`: the newest records, newest first.
- The NDJSON report file, when `report` is set. Each record carries the unignored `diffs` and the `ignored` ones, each with the index of the rule that hid it. It also carries both status codes, both latencies, and the annotations.

A value over 1 KiB in a record is cut to its first 1 KiB and the difference is marked `truncated`.
